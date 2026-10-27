package config_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
)

func defaults(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	return cfg
}

// OPS-006: an explicit path that does not exist is an error.
func TestLoad_MissingFileIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")
	_, err := config.Load(missing)
	if err == nil {
		t.Fatal("Load(missing) returned nil error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

// OPS-005: malformed env values are reported, not silently dropped.
func TestLoad_InvalidEnvValuesAreReported(t *testing.T) {
	t.Setenv("STATIC_SERVER_READ_TIMEOUT", "ten seconds")
	t.Setenv("STATIC_CACHE_MAX_BYTES", "256MB")
	t.Setenv("STATIC_CACHE_ENABLED", "yes please")
	t.Setenv("STATIC_COMPRESSION_LEVEL", "max")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("Load returned nil error for malformed env values")
	}
	for _, name := range []string{
		"STATIC_SERVER_READ_TIMEOUT",
		"STATIC_CACHE_MAX_BYTES",
		"STATIC_CACHE_ENABLED",
		"STATIC_COMPRESSION_LEVEL",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention %s: %v", name, err)
		}
	}
}

func TestLoad_BoolEnvParsing(t *testing.T) {
	cases := map[string]bool{"true": true, "TRUE": true, "1": true, "false": false, "False": false, "0": false}
	for v, want := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("STATIC_CACHE_PRELOAD", v)
			cfg := defaults(t)
			if cfg.Cache.Preload != want {
				t.Errorf("STATIC_CACHE_PRELOAD=%q → %v, want %v", v, cfg.Cache.Preload, want)
			}
		})
	}
}

// OPS-008: env vars documented in USER_GUIDE.md are implemented.
func TestLoad_SecurityEnvOverrides(t *testing.T) {
	t.Setenv("STATIC_SECURITY_HSTS_MAX_AGE", "0")
	t.Setenv("STATIC_SECURITY_HSTS_INCLUDE_SUBDOMAINS", "true")
	t.Setenv("STATIC_SECURITY_DIRECTORY_LISTING", "true")

	cfg := defaults(t)
	if cfg.Security.HSTSMaxAge != 0 {
		t.Errorf("HSTSMaxAge = %d, want 0", cfg.Security.HSTSMaxAge)
	}
	if !cfg.Security.HSTSIncludeSubdomains {
		t.Error("HSTSIncludeSubdomains = false, want true")
	}
	if !cfg.Security.DirectoryListing {
		t.Error("DirectoryListing = false, want true")
	}
}

func TestValidate_DefaultsAreValid(t *testing.T) {
	if err := defaults(t).Validate(); err != nil {
		t.Fatalf("defaults failed validation: %v", err)
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"empty addr", func(c *config.Config) { c.Server.Addr = "" }, "server.addr"},
		{"addr without port", func(c *config.Config) { c.Server.Addr = "localhost" }, "server.addr"},
		{"addr bad port", func(c *config.Config) { c.Server.Addr = ":70000" }, "server.addr"},
		{"cert without key", func(c *config.Config) { c.Server.TLSCert = "a.crt" }, "tls_key must be set together"},
		{"key without cert", func(c *config.Config) { c.Server.TLSKey = "a.key" }, "tls_key must be set together"},
		{"bad tls addr", func(c *config.Config) {
			c.Server.TLSCert, c.Server.TLSKey, c.Server.TLSAddr = "a", "b", "nope"
		}, "server.tls_addr"},
		{"negative read timeout", func(c *config.Config) { c.Server.ReadTimeout = -time.Second }, "server.read_timeout"},
		{"negative shutdown timeout", func(c *config.Config) { c.Server.ShutdownTimeout = -1 }, "server.shutdown_timeout"},
		{"negative ttl", func(c *config.Config) { c.Cache.TTL = -time.Second }, "cache.ttl"},
		{"negative max conns", func(c *config.Config) { c.Server.MaxConnsPerIP = -1 }, "max_conns_per_ip"},
		{"empty root", func(c *config.Config) { c.Files.Root = " " }, "files.root"},
		{"negative max serve", func(c *config.Config) { c.Files.MaxServeFileSize = -1 }, "max_serve_file_size"},
		{"zero cache bytes", func(c *config.Config) { c.Cache.MaxBytes = 0 }, "cache.max_bytes"},
		{"negative cache file size", func(c *config.Config) { c.Cache.MaxFileSize = -5 }, "cache.max_file_size"},
		{"negative gc percent", func(c *config.Config) { c.Cache.GCPercent = -1 }, "gc_percent"},
		{"negative min size", func(c *config.Config) { c.Compression.MinSize = -1 }, "min_size"},
		{"negative max compress", func(c *config.Config) { c.Compression.MaxCompressSize = -1 }, "max_compress_size"},
		{"level zero", func(c *config.Config) { c.Compression.Level = 0 }, "compression.level"},
		{"level ten", func(c *config.Config) { c.Compression.Level = 10 }, "compression.level"},
		{"negative static max age", func(c *config.Config) { c.Headers.StaticMaxAge = -1 }, "static_max_age"},
		{"negative html max age", func(c *config.Config) { c.Headers.HTMLMaxAge = -1 }, "html_max_age"},
		{"negative hsts", func(c *config.Config) { c.Security.HSTSMaxAge = -1 }, "hsts_max_age"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate returned nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestValidate_Accepts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"cache disabled with zero sizes", func(c *config.Config) {
			c.Cache.Enabled = false
			c.Cache.MaxBytes = 0
			c.Cache.MaxFileSize = 0
		}},
		{"compression disabled with level 0", func(c *config.Config) {
			c.Compression.Enabled = false
			c.Compression.Level = 0
		}},
		{"gzip default level", func(c *config.Config) { c.Compression.Level = -1 }},
		{"level 1", func(c *config.Config) { c.Compression.Level = 1 }},
		{"level 9", func(c *config.Config) { c.Compression.Level = 9 }},
		{"ipv6 addr", func(c *config.Config) { c.Server.Addr = "[::1]:8080" }},
		{"ephemeral port", func(c *config.Config) { c.Server.Addr = "127.0.0.1:0" }},
		{"tls pair", func(c *config.Config) { c.Server.TLSCert, c.Server.TLSKey = "a.crt", "a.key" }},
		{"bad tls addr ignored without tls", func(c *config.Config) { c.Server.TLSAddr = "" }},
		{"zero timeouts", func(c *config.Config) {
			c.Server.ReadTimeout, c.Server.WriteTimeout, c.Server.IdleTimeout = 0, 0, 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults(t)
			tc.mutate(cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestValidate_ReportsAllProblems(t *testing.T) {
	cfg := defaults(t)
	cfg.Server.MaxConnsPerIP = -1
	cfg.Compression.Level = 42
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate returned nil")
	}
	if !strings.Contains(err.Error(), "max_conns_per_ip") || !strings.Contains(err.Error(), "compression.level") {
		t.Errorf("expected both problems reported, got: %v", err)
	}
}
