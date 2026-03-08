// Package config provides configuration loading for the static web server.
// It supports TOML file loading, sensible defaults, and environment variable overrides.
package config

import (
	"fmt"
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
}

// FilesConfig holds file-serving settings.
type FilesConfig struct {
	// Root is the directory to serve files from. Default: "./public".
	Root string `toml:"root"`
	// Index is the index filename served for directory requests. Default: "index.html".
	Index string `toml:"index"`
	// NotFound is the path (relative to Root) of the custom 404 page.
	NotFound string `toml:"not_found"`
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
}

// HeadersConfig controls HTTP response header settings.
type HeadersConfig struct {
	// ImmutablePattern is a glob pattern for files to mark as immutable (max-age + immutable).
	ImmutablePattern string `toml:"immutable_pattern"`
	// StaticMaxAge is the Cache-Control max-age for non-HTML static assets. Default: 3600.
	StaticMaxAge int `toml:"static_max_age"`
	// HTMLMaxAge is the Cache-Control max-age for HTML files. Default: 0.
	HTMLMaxAge int `toml:"html_max_age"`
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

// Load reads the TOML config file at path, applies defaults, then applies
// environment variable overrides. Returns a validated *Config or an error.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	applyDefaults(cfg)

	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if _, err := toml.DecodeFile(path, cfg); err != nil {
				return nil, fmt.Errorf("config: failed to parse %q: %w", path, err)
			}
		}
	}

	applyEnvOverrides(cfg)

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

	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 256 * 1024 * 1024   // 256 MB
	cfg.Cache.MaxFileSize = 10 * 1024 * 1024 // 10 MB

	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 1024
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = true

	cfg.Headers.StaticMaxAge = 3600
	cfg.Headers.HTMLMaxAge = 0

	cfg.Security.BlockDotfiles = true
	cfg.Security.DirectoryListing = false
	cfg.Security.CSP = "default-src 'self'"
	cfg.Security.ReferrerPolicy = "strict-origin-when-cross-origin"
	cfg.Security.PermissionsPolicy = "geolocation=(), microphone=(), camera=()"
	cfg.Security.HSTSMaxAge = 31536000 // 1 year
	cfg.Security.HSTSIncludeSubdomains = false
}

// applyEnvOverrides reads well-known environment variables and overrides the
// corresponding config fields if the variable is set.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("STATIC_SERVER_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("STATIC_SERVER_TLS_ADDR"); v != "" {
		cfg.Server.TLSAddr = v
	}
	if v := os.Getenv("STATIC_SERVER_REDIRECT_HOST"); v != "" {
		cfg.Server.RedirectHost = v
	}
	if v := os.Getenv("STATIC_SERVER_TLS_CERT"); v != "" {
		cfg.Server.TLSCert = v
	}
	if v := os.Getenv("STATIC_SERVER_TLS_KEY"); v != "" {
		cfg.Server.TLSKey = v
	}
	if v := os.Getenv("STATIC_SERVER_READ_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.ReadTimeout = d
		}
	}
	if v := os.Getenv("STATIC_SERVER_WRITE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.WriteTimeout = d
		}
	}
	if v := os.Getenv("STATIC_SERVER_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.IdleTimeout = d
		}
	}
	if v := os.Getenv("STATIC_SERVER_SHUTDOWN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.ShutdownTimeout = d
		}
	}

	if v := os.Getenv("STATIC_FILES_ROOT"); v != "" {
		cfg.Files.Root = v
	}
	if v := os.Getenv("STATIC_FILES_INDEX"); v != "" {
		cfg.Files.Index = v
	}
	if v := os.Getenv("STATIC_FILES_NOT_FOUND"); v != "" {
		cfg.Files.NotFound = v
	}

	if v := os.Getenv("STATIC_CACHE_ENABLED"); v != "" {
		cfg.Cache.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("STATIC_CACHE_PRELOAD"); v != "" {
		cfg.Cache.Preload = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("STATIC_CACHE_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Cache.MaxBytes = n
		}
	}
	if v := os.Getenv("STATIC_CACHE_MAX_FILE_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Cache.MaxFileSize = n
		}
	}
	if v := os.Getenv("STATIC_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Cache.TTL = d
		}
	}
	if v := os.Getenv("STATIC_CACHE_GC_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.GCPercent = n
		}
	}

	if v := os.Getenv("STATIC_COMPRESSION_ENABLED"); v != "" {
		cfg.Compression.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("STATIC_COMPRESSION_MIN_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Compression.MinSize = n
		}
	}
	if v := os.Getenv("STATIC_COMPRESSION_LEVEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Compression.Level = n
		}
	}

	if v := os.Getenv("STATIC_SECURITY_BLOCK_DOTFILES"); v != "" {
		cfg.Security.BlockDotfiles = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("STATIC_SECURITY_CSP"); v != "" {
		cfg.Security.CSP = v
	}
	if v := os.Getenv("STATIC_SECURITY_CORS_ORIGINS"); v != "" {
		parts := strings.Split(v, ",")
		for i, p := range parts {
			parts[i] = strings.TrimSpace(p)
		}
		cfg.Security.CORSOrigins = parts
	}
}
