package handler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// B2 (integration): Range + Accept-Encoding: gzip must not produce a gzip-encoded 206.
func TestRangeWithGzip_NotEncoded(t *testing.T) {
	root, cfg := setupTestDir(t)
	content := strings.Repeat("hello world ", 500)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg.Compression.Precompressed = false

	for _, cacheOn := range []bool{true, false} {
		cfg.Cache.Enabled = cacheOn
		h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
		ctx := newTestCtx("GET", "/big.txt")
		ctx.Request.Header.Set("Range", "bytes=0-99")
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		h(ctx)

		if ctx.Response.StatusCode() != fasthttp.StatusPartialContent {
			t.Fatalf("cache=%v status = %d, want 206", cacheOn, ctx.Response.StatusCode())
		}
		if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "" {
			t.Errorf("cache=%v Content-Encoding = %q on 206, want none", cacheOn, ce)
		}
		if got := string(ctx.Response.Body()); got != content[:100] {
			t.Errorf("cache=%v 206 body is not the identity bytes 0-99 (len %d)", cacheOn, len(got))
		}
	}
}
