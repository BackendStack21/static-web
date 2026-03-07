package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/static-web/server/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\") returned unexpected error: %v", err)
	}

	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q, want %q", cfg.Server.Addr, ":8080")
	}
	if cfg.Server.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("Server.ReadHeaderTimeout = %v, want 5s", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("Server.ReadTimeout = %v, want 30s", cfg.Server.ReadTimeout)
	}
	if cfg.Files.Root != "./public" {
		t.Errorf("Files.Root = %q, want %q", cfg.Files.Root, "./public")
	}
	if !cfg.Cache.Enabled {
		t.Error("Cache.Enabled should default to true")
	}
	if cfg.Cache.MaxBytes != 256*1024*1024 {
		t.Errorf("Cache.MaxBytes = %d, want %d", cfg.Cache.MaxBytes, 256*1024*1024)
	}
	if !cfg.Compression.Enabled {
		t.Error("Compression.Enabled should default to true")
	}
	if cfg.Security.CSP != "default-src 'self'" {
		t.Errorf("Security.CSP = %q, want %q", cfg.Security.CSP, "default-src 'self'")
	}
	if !cfg.Security.BlockDotfiles {
		t.Error("Security.BlockDotfiles should default to true")
	}
}

func TestLoadFromTOML(t *testing.T) {
	toml := `
[server]
addr = ":9090"
read_timeout = "10s"

[files]
root = "/srv/www"

[cache]
enabled = false
max_bytes = 1048576

[security]
csp = "default-src 'none'"
`
	tmp := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(tmp, []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(tmp)
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.Server.Addr != ":9090" {
		t.Errorf("Server.Addr = %q, want :9090", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 10*time.Second {
		t.Errorf("Server.ReadTimeout = %v, want 10s", cfg.Server.ReadTimeout)
	}
	if cfg.Files.Root != "/srv/www" {
		t.Errorf("Files.Root = %q, want /srv/www", cfg.Files.Root)
	}
	if cfg.Cache.Enabled {
		t.Error("Cache.Enabled should be false from config")
	}
	if cfg.Cache.MaxBytes != 1048576 {
		t.Errorf("Cache.MaxBytes = %d, want 1048576", cfg.Cache.MaxBytes)
	}
	if cfg.Security.CSP != "default-src 'none'" {
		t.Errorf("Security.CSP = %q, want 'default-src none'", cfg.Security.CSP)
	}
	// Unset fields should still have defaults
	if cfg.Server.WriteTimeout != 30*time.Second {
		t.Errorf("Server.WriteTimeout = %v, want 30s (default)", cfg.Server.WriteTimeout)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("STATIC_SERVER_ADDR", ":7070")
	t.Setenv("STATIC_FILES_ROOT", "/env/root")
	t.Setenv("STATIC_CACHE_ENABLED", "false")
	t.Setenv("STATIC_CACHE_MAX_BYTES", "52428800")
	t.Setenv("STATIC_SECURITY_CSP", "default-src 'none'")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.Server.Addr != ":7070" {
		t.Errorf("Server.Addr = %q, want :7070", cfg.Server.Addr)
	}
	if cfg.Files.Root != "/env/root" {
		t.Errorf("Files.Root = %q, want /env/root", cfg.Files.Root)
	}
	if cfg.Cache.Enabled {
		t.Error("Cache.Enabled should be false from env")
	}
	if cfg.Cache.MaxBytes != 52428800 {
		t.Errorf("Cache.MaxBytes = %d, want 52428800", cfg.Cache.MaxBytes)
	}
	if cfg.Security.CSP != "default-src 'none'" {
		t.Errorf("Security.CSP = %q", cfg.Security.CSP)
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(tmp, []byte("not [ valid toml !!!"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(tmp)
	if err == nil {
		t.Error("expected error for invalid TOML, got nil")
	}
}
