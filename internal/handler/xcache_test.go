package handler_test

import (
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
)

// A response read from disk must say X-Cache: MISS. serveFromDisk set MISS
// and then served through serveFromCache, which overwrote it with HIT, so
// every response claimed to be a cache hit.
func TestXCache_DiskReadReportsMissAndCacheHitReportsHit(t *testing.T) {
	_, cfg := setupTestDir(t)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	for _, tc := range []struct {
		method, uri, want string
	}{
		{"GET", "/style.css", "MISS"},
		{"GET", "/style.css", "HIT"},
		{"HEAD", "/app.js", "MISS"},
		{"HEAD", "/app.js", "HIT"},
		{"GET", "/", "MISS"},
		{"GET", "/", "HIT"},
	} {
		ctx := newTestCtx(tc.method, tc.uri)
		h(ctx)
		if got := string(ctx.Response.Header.Peek("X-Cache")); got != tc.want {
			t.Errorf("%s %s: X-Cache = %q, want %q", tc.method, tc.uri, got, tc.want)
		}
	}

	// With the file cache disabled every response is read from disk.
	cfg.Cache.Enabled = false
	h = handler.BuildHandlerQuiet(cfg, nil)
	for i := 0; i < 2; i++ {
		ctx := newTestCtx("GET", "/style.css")
		h(ctx)
		if got := string(ctx.Response.Header.Peek("X-Cache")); got != "MISS" {
			t.Errorf("cache disabled, request %d: X-Cache = %q, want MISS", i, got)
		}
	}
}
