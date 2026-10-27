// Package config provides configuration loading for the static web server.
// It supports TOML file loading, sensible defaults, and environment variable overrides.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration struct for the static web server.
type Config struct {
	Server      ServerConfig      `toml:"server"`
	Files       FilesConfig       `toml:"files"`
	Cache       CacheConfig       `toml:"cache"`
	Compression CompressionConfig `toml:"compression"`
	Headers     HeadersConfig     `toml:"headers"`
	Security    SecurityConfig    `toml:"security"`
}

// ServerConfig holds network and TLS settings.
type ServerConfig struct {
	// Addr is the HTTP listen address. Default: ":8080".
	Addr string `toml:"addr"`
	// TLSAddr is the HTTPS listen address. Default: ":8443".
	TLSAddr string `toml:"tls_addr"`
	// RedirectHost is the canonical host used for HTTP→HTTPS redirects when TLS is enabled.
	// When empty, the server falls back to the host in TLSAddr if one is configured.
	RedirectHost string `toml:"redirect_host"`
	// TLSCert is the path to the TLS certificate file.
	TLSCert string `toml:"tls_cert"`
	// TLSKey is the path to the TLS private key file.
	TLSKey string `toml:"tls_key"`
	// ReadTimeout is the maximum duration for reading the entire request (headers + body).
	// With fasthttp, this single timeout covers the full read phase (there is no
	// separate ReadHeaderTimeout). Default: 10s.
	ReadTimeout time.Duration `toml:"read_timeout"`
	// WriteTimeout is the maximum duration for writing a response.
	WriteTimeout time.Duration `toml:"write_timeout"`
	// IdleTimeout is the maximum duration for keep-alive connections.
	IdleTimeout time.Duration `toml:"idle_timeout"`
	// ShutdownTimeout is how long to wait for in-flight requests during shutdown.
	ShutdownTimeout time.Duration `toml:"shutdown_timeout"`
	// MaxConnsPerIP limits the number of concurrent connections from a single IP
	// address. 0 means unlimited. Default: 0 (disabled).
	// SEC-015: Rate-limiting defence against connection exhaustion attacks.
	MaxConnsPerIP int `toml:"max_conns_per_ip"`
}

// FilesConfig holds file-serving settings.
type FilesConfig struct {
	// Root is the directory to serve files from. Default: "./public".
	Root string `toml:"root"`
	// Index is the index filename served for directory requests. Default: "index.html".
	Index string `toml:"index"`
	// NotFound is the path (relative to Root) of the custom 404 page.
	NotFound string `toml:"not_found"`
	// MaxServeFileSize is the maximum file size (in bytes) that will be served.
	// Files exceeding this limit receive a 413 Payload Too Large response.
	// This prevents a single request from loading an arbitrarily large file
	// into memory. Default: 1 GB. Set to 0 to disable the limit.
	// SEC-011: Hard upper bound for large file serving.
	MaxServeFileSize int64 `toml:"max_serve_file_size"`
}

// CacheConfig holds in-memory cache settings.
type CacheConfig struct {
	// Enabled turns the in-memory cache on or off. Default: true.
	Enabled bool `toml:"enabled"`
	// Preload walks the files root at startup and loads every eligible file
	// into the in-memory cache so that the first request for each file is
	// served from RAM instead of hitting the filesystem. Default: false.
	Preload bool `toml:"preload"`
	// MaxBytes is the maximum total byte size for the cache. Default: 256 MB.
	MaxBytes int64 `toml:"max_bytes"`
	// MaxFileSize is the maximum individual file size to cache. Default: 10 MB.
	MaxFileSize int64 `toml:"max_file_size"`
	// TTL is an optional time-to-live for cache entries (0 means no expiry).
	TTL time.Duration `toml:"ttl"`
	// GCPercent sets the Go runtime garbage collector target percentage via
	// debug.SetGCPercent(). A higher value reduces GC frequency at the cost of
	// more memory. The default value of 0 means "do not change" (use Go's
	// default of 100). Recommended: 400 for high-throughput deployments
	// serving preloaded files.
	GCPercent int `toml:"gc_percent"`
}

// CompressionConfig controls response compression settings.
type CompressionConfig struct {
	// Enabled turns on response compression. Default: true.
	Enabled bool `toml:"enabled"`
	// MinSize is the minimum response size in bytes to compress. Default: 1024.
	MinSize int `toml:"min_size"`
	// Level is the gzip compression level (1–9). Default: 5.
	Level int `toml:"level"`
	// Precompressed enables serving pre-compressed .gz/.br sidecar files. Default: true.
	Precompressed bool `toml:"precompressed"`
	// MaxCompressSize is the maximum response body size (in bytes) eligible for
	// on-the-fly gzip compression. Bodies exceeding this limit are served
	// uncompressed to avoid excessive memory allocation and CPU usage.
	// Default: 10 MB. Set to 0 to disable the limit.
	// SEC-005: Bounds on-the-fly compression memory usage.
	MaxCompressSize int `toml:"max_compress_size"`
}

// HeadersConfig controls HTTP response header settings.
type HeadersConfig struct {
	// ImmutablePattern is a glob pattern for files to mark as immutable (max-age + immutable).
	ImmutablePattern string `toml:"immutable_pattern"`
	// StaticMaxAge is the Cache-Control max-age for non-HTML static assets. Default: 3600.
	StaticMaxAge int `toml:"static_max_age"`
	// HTMLMaxAge is the Cache-Control max-age for HTML files. Default: 0.
	HTMLMaxAge int `toml:"html_max_age"`
	// EnableETags enables ETag generation and If-None-Match validation. Default: true.
	EnableETags bool `toml:"enable_etags"`
}

// SecurityConfig controls security settings.
type SecurityConfig struct {
	// BlockDotfiles prevents serving files whose path components start with ".". Default: true.
	BlockDotfiles bool `toml:"block_dotfiles"`
	// DirectoryListing enables or disables directory index listing. Default: false.
	DirectoryListing bool `toml:"directory_listing"`
	// CORSOrigins is the list of allowed CORS origins.
	CORSOrigins []string `toml:"cors_origins"`
	// CSP is the Content-Security-Policy header value. Default: "default-src 'self'".
	CSP string `toml:"csp"`
	// ReferrerPolicy sets the Referrer-Policy header. Default: "strict-origin-when-cross-origin".
	ReferrerPolicy string `toml:"referrer_policy"`
	// PermissionsPolicy sets the Permissions-Policy header. Default: "geolocation=(), microphone=(), camera=()".
	PermissionsPolicy string `toml:"permissions_policy"`
	// HSTSMaxAge is the max-age value in seconds for the Strict-Transport-Security header.
	// Only sent over HTTPS. Default: 31536000 (1 year). Set to 0 to disable HSTS.
	HSTSMaxAge int `toml:"hsts_max_age"`
	// HSTSIncludeSubdomains adds the includeSubDomains directive to the HSTS header.
	HSTSIncludeSubdomains bool `toml:"hsts_include_subdomains"`
}

// Load applies defaults, reads the TOML config file at path (when path is
// non-empty), then applies environment variable overrides.
//
// It returns an error when path is non-empty but the file does not exist, when
// the file cannot be parsed, or when an environment variable holds a value
// that cannot be parsed for its field. Load does not call Validate: callers
// apply their own overrides (e.g. CLI flags) first and validate afterwards.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	applyDefaults(cfg)

	// OPS-006: a non-empty path must exist. Callers that want "load the default
	// file only if present" must check for it themselves and pass "" otherwise.
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("config: file %q not found", path)
			}
			return nil, fmt.Errorf("config: cannot access %q: %w", path, err)
		}
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("config: failed to parse %q: %w", path, err)
		}
	}

	// OPS-005: malformed environment values are reported instead of dropped.
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// applyDefaults sets all default values on a zero-value Config.
func applyDefaults(cfg *Config) {
	cfg.Server.Addr = ":8080"
	cfg.Server.TLSAddr = ":8443"
	cfg.Server.ReadTimeout = 10 * time.Second
	cfg.Server.WriteTimeout = 10 * time.Second
	cfg.Server.IdleTimeout = 75 * time.Second
	cfg.Server.ShutdownTimeout = 15 * time.Second

	cfg.Files.Root = "./public"
	cfg.Files.Index = "index.html"
	cfg.Files.MaxServeFileSize = 1024 * 1024 * 1024 // 1 GB

	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 256 * 1024 * 1024   // 256 MB
	cfg.Cache.MaxFileSize = 10 * 1024 * 1024 // 10 MB

	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 1024
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = true
	cfg.Compression.MaxCompressSize = 10 * 1024 * 1024 // 10 MB

	cfg.Headers.StaticMaxAge = 3600
	cfg.Headers.HTMLMaxAge = 0
	cfg.Headers.EnableETags = true

	cfg.Security.BlockDotfiles = true
	cfg.Security.DirectoryListing = false
	cfg.Security.CSP = "default-src 'self'"
	cfg.Security.ReferrerPolicy = "strict-origin-when-cross-origin"
	cfg.Security.PermissionsPolicy = "geolocation=(), microphone=(), camera=()"
	cfg.Security.HSTSMaxAge = 31536000 // 1 year
	cfg.Security.HSTSIncludeSubdomains = false
}

// applyEnvOverrides reads well-known environment variables and overrides the
// corresponding config fields if the variable is set. Every variable whose
// value cannot be parsed is collected and reported in the returned error
// (OPS-005); valid variables are still applied.
func applyEnvOverrides(cfg *Config) error {
	e := &envReader{}

	e.str("STATIC_SERVER_ADDR", &cfg.Server.Addr)
	e.str("STATIC_SERVER_TLS_ADDR", &cfg.Server.TLSAddr)
	e.str("STATIC_SERVER_REDIRECT_HOST", &cfg.Server.RedirectHost)
	e.str("STATIC_SERVER_TLS_CERT", &cfg.Server.TLSCert)
	e.str("STATIC_SERVER_TLS_KEY", &cfg.Server.TLSKey)
	e.duration("STATIC_SERVER_READ_TIMEOUT", &cfg.Server.ReadTimeout)
	e.duration("STATIC_SERVER_WRITE_TIMEOUT", &cfg.Server.WriteTimeout)
	e.duration("STATIC_SERVER_IDLE_TIMEOUT", &cfg.Server.IdleTimeout)
	e.duration("STATIC_SERVER_SHUTDOWN_TIMEOUT", &cfg.Server.ShutdownTimeout)
	e.integer("STATIC_SERVER_MAX_CONNS_PER_IP", &cfg.Server.MaxConnsPerIP)

	e.str("STATIC_FILES_ROOT", &cfg.Files.Root)
	e.str("STATIC_FILES_INDEX", &cfg.Files.Index)
	e.str("STATIC_FILES_NOT_FOUND", &cfg.Files.NotFound)
	e.int64("STATIC_FILES_MAX_SERVE_FILE_SIZE", &cfg.Files.MaxServeFileSize)

	e.boolean("STATIC_CACHE_ENABLED", &cfg.Cache.Enabled)
	e.boolean("STATIC_CACHE_PRELOAD", &cfg.Cache.Preload)
	e.int64("STATIC_CACHE_MAX_BYTES", &cfg.Cache.MaxBytes)
	e.int64("STATIC_CACHE_MAX_FILE_SIZE", &cfg.Cache.MaxFileSize)
	e.duration("STATIC_CACHE_TTL", &cfg.Cache.TTL)
	e.integer("STATIC_CACHE_GC_PERCENT", &cfg.Cache.GCPercent)

	e.boolean("STATIC_COMPRESSION_ENABLED", &cfg.Compression.Enabled)
	e.integer("STATIC_COMPRESSION_MIN_SIZE", &cfg.Compression.MinSize)
	e.integer("STATIC_COMPRESSION_LEVEL", &cfg.Compression.Level)
	e.integer("STATIC_COMPRESSION_MAX_COMPRESS_SIZE", &cfg.Compression.MaxCompressSize)

	e.boolean("STATIC_HEADERS_ENABLE_ETAGS", &cfg.Headers.EnableETags)

	e.boolean("STATIC_SECURITY_BLOCK_DOTFILES", &cfg.Security.BlockDotfiles)
	e.boolean("STATIC_SECURITY_DIRECTORY_LISTING", &cfg.Security.DirectoryListing)
	e.str("STATIC_SECURITY_CSP", &cfg.Security.CSP)
	e.integer("STATIC_SECURITY_HSTS_MAX_AGE", &cfg.Security.HSTSMaxAge)
	e.boolean("STATIC_SECURITY_HSTS_INCLUDE_SUBDOMAINS", &cfg.Security.HSTSIncludeSubdomains)
	if v := os.Getenv("STATIC_SECURITY_CORS_ORIGINS"); v != "" {
		parts := strings.Split(v, ",")
		for i, p := range parts {
			parts[i] = strings.TrimSpace(p)
		}
		cfg.Security.CORSOrigins = parts
	}

	return errors.Join(e.errs...)
}

// envReader applies typed environment variables and accumulates parse errors.
// Unset or empty variables leave the target untouched.
type envReader struct {
	errs []error
}

func (e *envReader) fail(name, value, want string) {
	e.errs = append(e.errs, fmt.Errorf("config: invalid %s=%q: expected %s", name, value, want))
}

func (e *envReader) str(name string, dst *string) {
	if v := os.Getenv(name); v != "" {
		*dst = v
	}
}

func (e *envReader) duration(name string, dst *time.Duration) {
	if v := os.Getenv(name); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			e.fail(name, v, `a duration such as "10s" or "1m30s"`)
			return
		}
		*dst = d
	}
}

func (e *envReader) integer(name string, dst *int) {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			e.fail(name, v, "an integer")
			return
		}
		*dst = n
	}
}

func (e *envReader) int64(name string, dst *int64) {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			e.fail(name, v, "an integer number of bytes")
			return
		}
		*dst = n
	}
}

func (e *envReader) boolean(name string, dst *bool) {
	if v := os.Getenv(name); v != "" {
		b, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(v)))
		if err != nil {
			e.fail(name, v, "true/false or 1/0")
			return
		}
		*dst = b
	}
}

// Validate checks the configuration for values that are out of range or
// mutually inconsistent and returns all problems found, joined into a single
// error (OPS-007). It is intended to run after every override source (file,
// environment, CLI flags) has been applied.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("config: "+format, args...))
	}

	// [server]
	if err := validateListenAddr(c.Server.Addr); err != nil {
		add("server.addr %q: %v", c.Server.Addr, err)
	}
	certSet, keySet := c.Server.TLSCert != "", c.Server.TLSKey != ""
	if certSet != keySet {
		add("server.tls_cert and server.tls_key must be set together (tls_cert=%q, tls_key=%q)",
			c.Server.TLSCert, c.Server.TLSKey)
	}
	if certSet && keySet {
		if err := validateListenAddr(c.Server.TLSAddr); err != nil {
			add("server.tls_addr %q: %v", c.Server.TLSAddr, err)
		}
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"server.read_timeout", c.Server.ReadTimeout},
		{"server.write_timeout", c.Server.WriteTimeout},
		{"server.idle_timeout", c.Server.IdleTimeout},
		{"server.shutdown_timeout", c.Server.ShutdownTimeout},
		{"cache.ttl", c.Cache.TTL},
	} {
		if d.v < 0 {
			add("%s must not be negative (got %v)", d.name, d.v)
		}
	}
	if c.Server.MaxConnsPerIP < 0 {
		add("server.max_conns_per_ip must be >= 0 (got %d; 0 = unlimited)", c.Server.MaxConnsPerIP)
	}

	// [files]
	if strings.TrimSpace(c.Files.Root) == "" {
		add("files.root must not be empty")
	}
	if c.Files.MaxServeFileSize < 0 {
		add("files.max_serve_file_size must be >= 0 (got %d; 0 = unlimited)", c.Files.MaxServeFileSize)
	}

	// [cache]
	if c.Cache.Enabled {
		if c.Cache.MaxBytes <= 0 {
			add("cache.max_bytes must be > 0 when the cache is enabled (got %d)", c.Cache.MaxBytes)
		}
		if c.Cache.MaxFileSize <= 0 {
			add("cache.max_file_size must be > 0 when the cache is enabled (got %d)", c.Cache.MaxFileSize)
		}
	}
	if c.Cache.GCPercent < 0 {
		add("cache.gc_percent must be >= 0 (got %d; 0 = Go default)", c.Cache.GCPercent)
	}

	// [compression]
	if c.Compression.MinSize < 0 {
		add("compression.min_size must be >= 0 (got %d)", c.Compression.MinSize)
	}
	if c.Compression.MaxCompressSize < 0 {
		add("compression.max_compress_size must be >= 0 (got %d; 0 = unlimited)", c.Compression.MaxCompressSize)
	}
	// gzip levels: 1 (BestSpeed) .. 9 (BestCompression); -1 selects the
	// library default. Anything else would silently fall back to the default.
	if c.Compression.Enabled && c.Compression.Level != -1 &&
		(c.Compression.Level < 1 || c.Compression.Level > 9) {
		add("compression.level must be between 1 and 9 or -1 for the gzip default (got %d)", c.Compression.Level)
	}

	// [headers]
	if c.Headers.StaticMaxAge < 0 {
		add("headers.static_max_age must be >= 0 (got %d)", c.Headers.StaticMaxAge)
	}
	if c.Headers.HTMLMaxAge < 0 {
		add("headers.html_max_age must be >= 0 (got %d)", c.Headers.HTMLMaxAge)
	}

	// [security]
	if c.Security.HSTSMaxAge < 0 {
		add("security.hsts_max_age must be >= 0 (got %d; 0 = disabled)", c.Security.HSTSMaxAge)
	}

	return errors.Join(errs...)
}

// validateListenAddr checks that addr is a "host:port" pair with a numeric
// port in 0..65535 (0 selects an ephemeral port).
func validateListenAddr(addr string) error {
	if addr == "" {
		return errors.New("must not be empty")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}
