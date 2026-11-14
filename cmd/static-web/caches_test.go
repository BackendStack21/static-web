package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
)

func cachesTestConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<p>hi</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Files.Root = root
	if err := resolveFilesRoot(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// P-F1: the path cache must exist whenever the file cache does, not only
// with --preload; otherwise every default-config cache hit pays a full
// EvalSymlinks walk.
func TestNewCaches_PathCacheWithoutPreload(t *testing.T) {
	cfg := cachesTestConfig(t)
	cfg.Cache.Enabled = true
	cfg.Cache.Preload = false

	c, pc := newCaches(cfg, true)
	if c == nil {
		t.Fatal("file cache is nil with cache enabled")
	}
	if pc == nil {
		t.Fatal("path cache is nil without --preload")
	}
	if pc.Len() != 0 {
		t.Errorf("path cache has %d entries without preload, want 0", pc.Len())
	}
}

func TestNewCaches_PreloadPreWarmsPathCache(t *testing.T) {
	cfg := cachesTestConfig(t)
	cfg.Cache.Enabled = true
	cfg.Cache.Preload = true

	c, pc := newCaches(cfg, true)
	if c == nil || pc == nil {
		t.Fatalf("caches = %v, %v; want both", c, pc)
	}
	if _, ok := c.Get("/index.html"); !ok {
		t.Error("file cache not preloaded")
	}
	if p, ok := pc.Lookup("/index.html"); !ok || p != filepath.Join(cfg.Files.Root, "index.html") {
		t.Errorf("path cache entry = %q, %v; want the absolute index path", p, ok)
	}
}

// P-F9: preload as wired by the binary must build the same variants as lazy
// serving: precompressed sidecars (.br here) and a generated zstd variant
// next to gzip. PreloadConfig supports both, but newCaches set neither
// Precompressed nor ZstdFn, so preloaded entries had gzip only.
func TestNewCaches_PreloadBuildsSidecarAndZstdVariants(t *testing.T) {
	cfg := cachesTestConfig(t)
	cfg.Cache.Enabled = true
	cfg.Cache.Preload = true
	cfg.Compression.Enabled = true
	cfg.Compression.Precompressed = true
	cfg.Compression.MinSize = 1024
	js := []byte(strings.Repeat("console.log('preloaded asset');\n", 200))
	br := []byte("BR-SIDECAR-BYTES")
	if err := os.WriteFile(filepath.Join(cfg.Files.Root, "app.js"), js, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Files.Root, "app.js.br"), br, 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := newCaches(cfg, true)
	f, ok := c.Get("/app.js")
	if !ok {
		t.Fatal("app.js not preloaded")
	}
	if !bytes.Equal(f.BrData, br) {
		t.Errorf("BrData = %q, want the app.js.br sidecar", f.BrData)
	}
	if f.ZstdData == nil {
		t.Error("ZstdData is nil, want a generated zstd variant")
	}
	if f.GzipData == nil {
		t.Error("GzipData is nil, want a generated gzip variant")
	}
}

// With the file cache disabled every request reads the disk and is fully
// re-validated anyway, so a path cache would only add LRU work.
func TestNewCaches_NoCachesWhenCacheDisabled(t *testing.T) {
	cfg := cachesTestConfig(t)
	cfg.Cache.Enabled = false
	cfg.Cache.Preload = true

	c, pc := newCaches(cfg, true)
	if c != nil || pc != nil {
		t.Errorf("caches = %v, %v; want nil, nil with the cache disabled", c, pc)
	}
}
