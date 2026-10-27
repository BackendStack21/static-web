package handler_test

import (
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// Embedded default assets (served when the root lacks them) must honour
// conditional requests like any other file (RFC 9110 §13.1.2, §15.4.5).
func TestEmbeddedAsset_IfNoneMatchReturns304(t *testing.T) {
	_, cfg := setupTestDir(t)
	cfg.Files.Root = t.TempDir() // empty: forces the embedded fallback
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	first := newTestCtx("GET", "/style.css")
	h(first)
	if first.Response.StatusCode() != 200 {
		t.Fatalf("first status = %d, want 200 (embedded style.css)", first.Response.StatusCode())
	}
	etag := string(first.Response.Header.Peek("ETag"))
	cc := string(first.Response.Header.Peek("Cache-Control"))
	if etag == "" || cc == "" {
		t.Fatalf("embedded response lacks ETag/Cache-Control: %q %q", etag, cc)
	}

	for _, inm := range []string{etag, etag[len("W/"):], `"other", ` + etag} {
		ctx := newTestCtx("GET", "/style.css")
		ctx.Request.Header.Set("If-None-Match", inm)
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		h(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusNotModified {
			t.Errorf("If-None-Match %s: status = %d, want 304", inm, ctx.Response.StatusCode())
			continue
		}
		if len(ctx.Response.Body()) != 0 {
			t.Errorf("304 body length = %d", len(ctx.Response.Body()))
		}
		if got := string(ctx.Response.Header.Peek("ETag")); got != etag {
			t.Errorf("304 ETag = %q, want %q", got, etag)
		}
		if got := string(ctx.Response.Header.Peek("Cache-Control")); got != cc {
			t.Errorf("304 Cache-Control = %q, want %q", got, cc)
		}
		if _, toks := varyTokens(ctx); !hasToken(toks, "Accept-Encoding") {
			t.Errorf("304 Vary tokens = %v, want Accept-Encoding", toks)
		}
	}
}
