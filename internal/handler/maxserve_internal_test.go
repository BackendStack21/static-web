package handler

// Regression tests from the 2026-11-14 adversarial review.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// files.max_serve_file_size applies to every file, including those small
// enough for the in-memory cache, with the cache on or off.
func TestMaxServeFileSize_AppliesBelowCacheLimit(t *testing.T) {
	for _, cacheOn := range []bool{true, false} {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "f.txt"), bytes.Repeat([]byte("x"), 5000), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("small"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{}
		cfg.Files.Root = root
		cfg.Files.MaxServeFileSize = 1000
		cfg.Cache.Enabled = cacheOn
		cfg.Cache.MaxBytes = 1 << 20
		cfg.Cache.MaxFileSize = 1 << 20
		cfg.Server.WriteTimeout = 10 * time.Second
		c := cache.NewCache(cfg.Cache.MaxBytes)
		h := NewFileHandler(cfg, c)

		for _, tc := range []struct {
			uri  string
			want int
		}{{"/f.txt", fasthttp.StatusRequestEntityTooLarge}, {"/ok.txt", fasthttp.StatusOK}} {
			ctx := swCtx("GET", tc.uri)
			h.HandleRequest(ctx)
			if got := ctx.Response.StatusCode(); got != tc.want {
				t.Errorf("cache=%v %s: status = %d, want %d", cacheOn, tc.uri, got, tc.want)
			}
		}
		if _, ok := c.Get("/f.txt"); ok {
			t.Errorf("cache=%v: a file over max_serve_file_size was cached", cacheOn)
		}
	}
}

func TestMaxInMemoryFileSize(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.MaxFileSize = 10 << 20
	for _, tc := range []struct{ serve, want int64 }{
		{0, 10 << 20},       // unlimited serving
		{1 << 30, 10 << 20}, // serving limit above the cache limit
		{1000, 1000},        // serving limit below it caps it
	} {
		cfg.Files.MaxServeFileSize = tc.serve
		if got := cfg.MaxInMemoryFileSize(); got != tc.want {
			t.Errorf("max_serve_file_size=%d: MaxInMemoryFileSize = %d, want %d", tc.serve, got, tc.want)
		}
	}
}
