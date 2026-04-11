// Command static-web is the entry point for the static web file server.
//
// Usage:
//
//	static-web [command] [flags] [directory]
//
// When no command is given, "serve" is assumed.
// Run "static-web --help" for the full flag reference.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/BackendStack21/static-web/internal/server"
	"github.com/BackendStack21/static-web/internal/version"
	"github.com/valyala/fasthttp"
)

//go:embed config.toml.example
var configExample []byte

func main() {
	if len(os.Args) < 2 {
		// No arguments at all — run serve with defaults.
		runServe([]string{})
		return
	}

	cmd := os.Args[1]
	rest := os.Args[2:]

	// If the first argument looks like a flag or a path (not a known subcommand),
	// treat it as an implicit "serve" invocation and pass all args through.
	switch cmd {
	case "serve":
		runServe(rest)
	case "init":
		runInit(rest)
	case "version":
		runVersion()
	case "--help", "-h", "help":
		printHelp()
		os.Exit(0)
	default:
		// Anything else (flags starting with '-', relative paths, etc.) is treated
		// as an argument to the implicit serve subcommand.
		runServe(os.Args[1:])
	}
}

// --------------------------------------------------------------------------
// serve subcommand
// --------------------------------------------------------------------------

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, serveUsage)
		fs.PrintDefaults()
	}

	// Global flag.
	cfgPath := fs.String("config", "config.toml", "path to TOML config file")

	// Network.
	host := fs.String("host", "", "host/IP to listen on (default: all interfaces)")
	port := fs.Int("p", 0, "shorthand for --port")
	portLong := fs.Int("port", 0, "HTTP port to listen on (default: 8080)")
	redirectHost := fs.String("redirect-host", "", "canonical host for HTTP to HTTPS redirects")
	tlsCert := fs.String("tls-cert", "", "path to TLS certificate file (PEM)")
	tlsKey := fs.String("tls-key", "", "path to TLS private key file (PEM)")
	tlsPort := fs.Int("tls-port", 0, "HTTPS port (default: 8443)")

	// Files.
	index := fs.String("index", "", "index filename for directory requests (default: index.html)")
	notFound := fs.String("404", "", "custom 404 page path relative to root")

	// Cache.
	noCache := fs.Bool("no-cache", false, "disable in-memory file cache")
	cacheSize := fs.String("cache-size", "", "max cache size, e.g. 256MB, 1GB (default: 256MB)")
	preload := fs.Bool("preload", false, "preload all files into cache at startup for maximum throughput")
	gcPercent := fs.Int("gc-percent", 0, "set Go GC target percentage (0=default, 400 recommended for high throughput)")

	// Compression.
	noCompress := fs.Bool("no-compress", false, "disable response compression")

	// Headers.
	noEtag := fs.Bool("no-etag", false, "disable ETag generation and If-None-Match validation")

	// Security.
	cors := fs.String("cors", "", "allowed CORS origins, comma-separated or * for all")
	dirListing := fs.Bool("dir-listing", false, "enable directory listing")
	noDotfileBlock := fs.Bool("no-dotfile-block", false, "disable dotfile blocking")
	csp := fs.String("csp", "", "Content-Security-Policy header value")

	// Logging.
	quiet := fs.Bool("q", false, "shorthand for --quiet")
	quietLong := fs.Bool("quiet", false, "suppress per-request access log")
	verbose := fs.Bool("verbose", false, "log resolved config on startup")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	// Merge short/long flag aliases.
	effectivePort := *portLong
	if *port != 0 {
		effectivePort = *port
	}
	effectiveQuiet := *quiet || *quietLong

	// Load configuration (defaults → config file → env vars).
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load config from %q: %v\n", *cfgPath, err)
		os.Exit(1)
	}

	// Positional argument: [directory] overrides files.root.
	if dir := fs.Arg(0); dir != "" {
		cfg.Files.Root = dir
	}

	// Apply CLI flag overrides (highest priority).
	if err := applyFlagOverrides(cfg, flagOverrides{
		host:           *host,
		port:           effectivePort,
		redirectHost:   *redirectHost,
		tlsCert:        *tlsCert,
		tlsKey:         *tlsKey,
		tlsPort:        *tlsPort,
		index:          *index,
		notFound:       *notFound,
		noCache:        *noCache,
		cacheSize:      *cacheSize,
		preload:        *preload,
		gcPercent:      *gcPercent,
		noCompress:     *noCompress,
		noEtag:         *noEtag,
		cors:           *cors,
		dirListing:     *dirListing,
		noDotfileBlock: *noDotfileBlock,
		csp:            *csp,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *verbose {
		logConfig(cfg)
	}

	if !effectiveQuiet {
		log.Printf("static-web %s starting (addr=%s, root=%s)", version.Version, cfg.Server.Addr, cfg.Files.Root)
	}
	if cfg.Cache.GCPercent > 0 {
		old := debug.SetGCPercent(cfg.Cache.GCPercent)
		if !effectiveQuiet {
			log.Printf("GC target set to %d%% (was %d%%)", cfg.Cache.GCPercent, old)
		}
	}

	// Initialise the in-memory file cache (respects cfg.Cache.Enabled).
	var c *cache.Cache
	if cfg.Cache.Enabled {
		c = cache.NewCache(cfg.Cache.MaxBytes, cfg.Cache.TTL)
	} else {
		c = nil
	}

	// Preload files into cache at startup if requested.
	var pathCache *security.PathCache
	if c != nil && cfg.Cache.Preload {
		pcfg := cache.PreloadConfig{
			MaxFileSize:      cfg.Cache.MaxFileSize,
			IndexFile:        cfg.Files.Index,
			BlockDotfiles:    cfg.Security.BlockDotfiles,
			CompressEnabled:  cfg.Compression.Enabled,
			CompressMinSize:  cfg.Compression.MinSize,
			CompressLevel:    cfg.Compression.Level,
			CompressFn:       compress.GzipBytes,
			HTMLMaxAge:       cfg.Headers.HTMLMaxAge,
			StaticMaxAge:     cfg.Headers.StaticMaxAge,
			ImmutablePattern: cfg.Headers.ImmutablePattern,
		}
		stats := c.Preload(cfg.Files.Root, pcfg)
		if !effectiveQuiet {
			log.Printf("preloaded %d files (%s) into cache (%d skipped)",
				stats.Files, formatByteSize(stats.Bytes), stats.Skipped)
		}

		// Pre-warm the path cache with every URL key the file cache knows about.
		pathCache = security.NewPathCache(security.DefaultPathCacheSize)
		pathCache.PreWarm(stats.Paths, cfg.Files.Root, cfg.Security.BlockDotfiles)
		if !effectiveQuiet {
			log.Printf("path cache pre-warmed with %d entries", pathCache.Len())
		}
	}

	// Build the full middleware + handler chain.
	var h fasthttp.RequestHandler
	if effectiveQuiet {
		h = handler.BuildHandlerQuiet(cfg, c, pathCache)
	} else {
		h = handler.BuildHandler(cfg, c, pathCache)
	}

	// Create the HTTP/HTTPS server.
	serverCfg := cfg.Server
	srv := server.New(&serverCfg, &cfg.Security, h)

	// Start listeners in the background.
	go func() {
		if err := srv.Start(&serverCfg); err != nil {
			log.Printf("server start error: %v", err)
		}
	}()

	// Block until SIGTERM/SIGINT, handling SIGHUP for live reload.
	ctx := context.Background()
	server.RunSignalHandler(ctx, srv, c, *cfgPath, &cfg, pathCache)
}

// flagOverrides groups all serve-subcommand CLI flags that can override config.
type flagOverrides struct {
	host           string
	port           int
	redirectHost   string
	tlsCert        string
	tlsKey         string
	tlsPort        int
	index          string
	notFound       string
	noCache        bool
	cacheSize      string
	preload        bool
	gcPercent      int
	noCompress     bool
	noEtag         bool
	cors           string
	dirListing     bool
	noDotfileBlock bool
	csp            string
}

// applyFlagOverrides applies CLI flag values on top of the already-loaded config.
// Only flags that were explicitly provided (non-zero value) override the config.
func applyFlagOverrides(cfg *config.Config, f flagOverrides) error {
	// Network: reconstruct server.addr if host or port was provided.
	if f.host != "" || f.port != 0 {
		// Decompose current addr to fill in whichever side wasn't specified.
		currentHost, currentPortStr, _ := net.SplitHostPort(cfg.Server.Addr)
		h := currentHost
		p := currentPortStr
		if f.host != "" {
			h = f.host
		}
		if f.port != 0 {
			p = strconv.Itoa(f.port)
		}
		cfg.Server.Addr = net.JoinHostPort(h, p)
	}

	if f.tlsCert != "" {
		cfg.Server.TLSCert = f.tlsCert
	}
	if f.redirectHost != "" {
		cfg.Server.RedirectHost = f.redirectHost
	}
	if f.tlsKey != "" {
		cfg.Server.TLSKey = f.tlsKey
	}
	if f.tlsPort != 0 {
		// Preserve the host part of TLSAddr, replace only the port.
		currentHost, _, _ := net.SplitHostPort(cfg.Server.TLSAddr)
		cfg.Server.TLSAddr = net.JoinHostPort(currentHost, strconv.Itoa(f.tlsPort))
	}

	// Files.
	if f.index != "" {
		cfg.Files.Index = f.index
	}
	if f.notFound != "" {
		cfg.Files.NotFound = f.notFound
	}

	// Cache.
	if f.noCache {
		cfg.Cache.Enabled = false
	}
	if f.preload {
		cfg.Cache.Preload = true
	}
	if f.gcPercent != 0 {
		cfg.Cache.GCPercent = f.gcPercent
	}
	if f.cacheSize != "" {
		n, err := parseBytes(f.cacheSize)
		if err != nil {
			return fmt.Errorf("invalid --cache-size %q: %w", f.cacheSize, err)
		}
		cfg.Cache.MaxBytes = n
	}

	// Compression.
	if f.noCompress {
		cfg.Compression.Enabled = false
	}

	// Headers.
	if f.noEtag {
		cfg.Headers.EnableETags = false
	}

	// Security.
	if f.cors != "" {
		parts := strings.Split(f.cors, ",")
		for i, p := range parts {
			parts[i] = strings.TrimSpace(p)
		}
		cfg.Security.CORSOrigins = parts
	}
	if f.dirListing {
		cfg.Security.DirectoryListing = true
	}
	if f.noDotfileBlock {
		cfg.Security.BlockDotfiles = false
	}
	if f.csp != "" {
		cfg.Security.CSP = f.csp
	}

	return nil
}

// parseBytes parses a human-readable byte size string like "256MB", "1GB", "512kb".
// Supported suffixes: B, KB, MB, GB (case-insensitive). Plain integers are bytes.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}

	upper := strings.ToUpper(s)
	var multiplier int64 = 1
	var numStr string

	switch {
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1024 * 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "B"):
		multiplier = 1
		numStr = s[:len(s)-1]
	default:
		numStr = s
	}

	n, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q as a number", numStr)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must be non-negative")
	}

	return n * multiplier, nil
}

// formatByteSize returns a human-readable string like "7.7 KB" or "256.0 MB".
func formatByteSize(b int64) string {
	const (
		kb = 1024
		mb = 1024 * 1024
		gb = 1024 * 1024 * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// logConfig writes the resolved configuration to the standard logger.
func logConfig(cfg *config.Config) {
	log.Printf("[config] server.addr=%s tls_addr=%s redirect_host=%q tls_cert=%q tls_key=%q",
		cfg.Server.Addr, cfg.Server.TLSAddr, cfg.Server.RedirectHost, cfg.Server.TLSCert, cfg.Server.TLSKey)
	log.Printf("[config] files.root=%q files.index=%q files.not_found=%q",
		cfg.Files.Root, cfg.Files.Index, cfg.Files.NotFound)
	log.Printf("[config] cache.enabled=%v cache.preload=%v cache.max_bytes=%d cache.max_file_size=%d cache.gc_percent=%d",
		cfg.Cache.Enabled, cfg.Cache.Preload, cfg.Cache.MaxBytes, cfg.Cache.MaxFileSize, cfg.Cache.GCPercent)
	log.Printf("[config] compression.enabled=%v compression.min_size=%d compression.level=%d",
		cfg.Compression.Enabled, cfg.Compression.MinSize, cfg.Compression.Level)
	log.Printf("[config] security.block_dotfiles=%v security.directory_listing=%v security.cors_origins=%v",
		cfg.Security.BlockDotfiles, cfg.Security.DirectoryListing, cfg.Security.CORSOrigins)
	log.Printf("[config] security.csp=%q security.referrer_policy=%q",
		cfg.Security.CSP, cfg.Security.ReferrerPolicy)
}

// --------------------------------------------------------------------------
// init subcommand
// --------------------------------------------------------------------------

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: static-web init [--output path] [--force]")
		fs.PrintDefaults()
	}

	output := fs.String("output", "config.toml", "path to write the config file")
	force := fs.Bool("force", false, "overwrite if the file already exists")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	dest := *output

	if !*force {
		if _, err := os.Stat(dest); err == nil {
			fmt.Fprintf(os.Stderr, "error: %q already exists (use --force to overwrite)\n", dest)
			os.Exit(1)
		}
	}

	if err := os.WriteFile(dest, configExample, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write %q: %v\n", dest, err)
		os.Exit(1)
	}

	abs, err := filepath.Abs(dest)
	if err != nil {
		abs = dest
	}
	fmt.Printf("Config written to %s\n", abs)
}

// --------------------------------------------------------------------------
// version subcommand
// --------------------------------------------------------------------------

func runVersion() {
	fmt.Printf("static-web %s\n", version.Version)
	fmt.Printf("  go:     %s\n", runtime.Version())
	fmt.Printf("  os:     %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  commit: %s\n", version.Commit)
}

// --------------------------------------------------------------------------
// help
// --------------------------------------------------------------------------

func printHelp() {
	fmt.Print(helpText)
}

const serveUsage = `Usage: static-web serve [flags] [directory]
       static-web [flags] [directory]   (serve is the default subcommand)

Flags:`

const helpText = `static-web — a fast, secure static file server

Usage:
  static-web [command] [flags] [directory]

Commands:
  serve     Start the file server (default when command is omitted)
  init      Scaffold a config.toml in the current directory
  version   Print version information and exit

Examples:
  static-web                          serve ./public on :8080
  static-web ./dist                   serve ./dist on :8080
  static-web --port 3000 ./dist       serve ./dist on :3000
  static-web --no-cache .             serve . with caching disabled
  static-web init                     write a starter config.toml
  static-web version                  print version info

Serve flags:
  --config string        path to TOML config file (default "config.toml")
  --host string          host/IP to listen on (default: all interfaces)
  --port, -p int         HTTP port (default 8080)
  --redirect-host string canonical host for HTTP to HTTPS redirects
  --tls-cert string      path to TLS certificate (PEM)
  --tls-key string       path to TLS private key (PEM)
  --tls-port int         HTTPS port (default 8443)
  --index string         index filename for directory requests (default "index.html")
  --404 string           custom 404 page, relative to root
  --no-cache             disable in-memory file cache
  --cache-size string    max cache size, e.g. 256MB, 1GB (default 256MB)
  --preload              preload all files into cache at startup
  --gc-percent int       set Go GC target %% (0=default, 400 for high throughput)
  --no-compress          disable response compression
  --cors string          CORS origins, comma-separated or * for all
  --dir-listing          enable directory listing
  --no-dotfile-block     disable dotfile blocking
  --csp string           Content-Security-Policy header value
  --quiet, -q            suppress per-request access log
  --verbose              log resolved config on startup

Run "static-web serve --help" for full flag descriptions.
`
