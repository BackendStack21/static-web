package handler_test

import (
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// HTTP-F5 (full chain): with a specific cors_origins list, file responses to
// requests without an Origin must still vary on Origin, and the handler's own
// Vary: Accept-Encoding must be merged with it, not replace it.
func TestCORS_SpecificListFileResponsesVaryOnOrigin(t *testing.T) {
	_, cfg := setupTestDir(t)
	cfg.Security.CORSOrigins = []string{"https://a.example"}
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	first := newTestCtx("GET", "/app.js")
	h(first) // miss
	etag := string(first.Response.Header.Peek("ETag"))

	second := newTestCtx("GET", "/app.js")
	second.Request.Header.Set("Accept-Encoding", "gzip")
	h(second) // hit

	notModified := newTestCtx("GET", "/app.js")
	notModified.Request.Header.Set("If-None-Match", etag)
	h(notModified)

	missing := newTestCtx("GET", "/missing.js")
	h(missing)

	for name, ctx := range map[string]*fasthttp.RequestCtx{
		"miss": first, "hit": second, "304": notModified, "404": missing,
	} {
		lines, toks := varyTokens(ctx)
		if !hasToken(toks, "Origin") {
			t.Errorf("%s (status %d): Vary = %q, want Origin", name, ctx.Response.StatusCode(), ctx.Response.Header.Peek("Vary"))
		}
		if name != "404" && !hasToken(toks, "Accept-Encoding") {
			t.Errorf("%s: Vary = %q, want Accept-Encoding kept", name, ctx.Response.Header.Peek("Vary"))
		}
		if lines > 1 {
			t.Errorf("%s: %d Vary header lines, want 1", name, lines)
		}
	}
}
