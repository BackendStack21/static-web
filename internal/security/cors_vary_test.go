package security_test

import (
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

func varyHasOrigin(ctx *fasthttp.RequestCtx) bool {
	for _, v := range ctx.Response.Header.PeekAll("Vary") {
		for _, tok := range strings.Split(string(v), ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "Origin") {
				return true
			}
		}
	}
	return false
}

// HTTP-F5: with a specific cors_origins list the response depends on the
// Origin header, so every response must carry Vary: Origin -- including
// those to requests without an Origin or with a disallowed one. Otherwise a
// shared cache stores the variant without ACAO and serves it to an allowed
// origin.
func TestMiddleware_CORS_SpecificListAlwaysVariesOnOrigin(t *testing.T) {
	cfg := &config.SecurityConfig{CORSOrigins: []string{"https://a.example", "https://b.example"}}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}
	h := security.Middleware(cfg, t.TempDir(), next)

	cases := []struct {
		method, origin string
	}{
		{"GET", ""},
		{"GET", "https://evil.example"},
		{"GET", "https://a.example"},
		{"HEAD", ""},
		{"OPTIONS", ""},
		{"OPTIONS", "https://evil.example"},
		{"OPTIONS", "https://b.example"},
	}
	for _, tc := range cases {
		ctx := newSecurityCtx(tc.method, "/app.js")
		if tc.origin != "" {
			ctx.Request.Header.Set("Origin", tc.origin)
		}
		h(ctx)
		if !varyHasOrigin(ctx) {
			t.Errorf("%s with Origin %q: Vary = %q, want it to include Origin",
				tc.method, tc.origin, ctx.Response.Header.Peek("Vary"))
		}
		if n := len(ctx.Response.Header.PeekAll("Vary")); n > 1 {
			t.Errorf("%s with Origin %q: %d Vary header lines, want 1", tc.method, tc.origin, n)
		}
	}
}

// With the wildcard every origin gets the same "*" response, so Vary: Origin
// stays off (SEC-012), with or without an Origin header.
func TestMiddleware_CORS_WildcardNeverVariesOnOrigin(t *testing.T) {
	cfg := &config.SecurityConfig{CORSOrigins: []string{"*"}}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}
	h := security.Middleware(cfg, t.TempDir(), next)
	for _, origin := range []string{"", "https://x.example"} {
		ctx := newSecurityCtx("GET", "/app.js")
		if origin != "" {
			ctx.Request.Header.Set("Origin", origin)
		}
		h(ctx)
		if varyHasOrigin(ctx) {
			t.Errorf("Origin %q: Vary = %q, must not include Origin with the wildcard", origin, ctx.Response.Header.Peek("Vary"))
		}
	}
}

// With the wildcard the response must not depend on Origin at all: every
// response carries "Access-Control-Allow-Origin: *", or a shared cache could
// store the variant without it (from a request with no Origin) and serve it
// to cross-origin fetches, which the browser would then block.
func TestMiddleware_CORS_WildcardAllowsOriginOnEveryResponse(t *testing.T) {
	cfg := &config.SecurityConfig{CORSOrigins: []string{"*"}}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}
	h := security.Middleware(cfg, t.TempDir(), next)
	for _, origin := range []string{"", "https://x.example"} {
		ctx := newSecurityCtx("GET", "/app.js")
		if origin != "" {
			ctx.Request.Header.Set("Origin", origin)
		}
		h(ctx)
		if got := string(ctx.Response.Header.Peek("Access-Control-Allow-Origin")); got != "*" {
			t.Errorf("Origin %q: Access-Control-Allow-Origin = %q, want *", origin, got)
		}
	}
	// A preflight still needs an Origin; a bare OPTIONS is a plain 204.
	ctx := newSecurityCtx("OPTIONS", "/app.js")
	h(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusNoContent || len(ctx.Response.Header.Peek("Access-Control-Allow-Methods")) != 0 {
		t.Errorf("bare OPTIONS: status %d, Allow-Methods %q; want a plain 204",
			ctx.Response.StatusCode(), ctx.Response.Header.Peek("Access-Control-Allow-Methods"))
	}
}
