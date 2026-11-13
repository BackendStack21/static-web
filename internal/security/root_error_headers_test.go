package security_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// HTTP-F4: when files.root cannot be resolved at startup, Middleware answers
// every request with a 500. It used ctx.Error, which resets the response, and
// built the security headers only after that early return, so the 500 went
// out without CSP, nosniff or frame options.
func TestMiddleware_UnresolvableRootErrorKeepsSecurityHeaders(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Skipf("cannot remove the working directory: %v", err)
	}
	if _, err := filepath.Abs("public"); err == nil {
		t.Skip("filepath.Abs still succeeds after the working directory is removed on this platform")
	}

	cfg := &config.SecurityConfig{CSP: "default-src 'self'", ReferrerPolicy: "no-referrer"}
	next := func(ctx *fasthttp.RequestCtx) { t.Error("next handler called with an unresolvable root") }
	h := security.Middleware(cfg, "public", next)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/index.html")
	ctx.Response.Header.Set("Access-Control-Allow-Origin", "https://a.example") // set by an outer layer
	h(&ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got)
	}
	for k, want := range map[string]string{
		"Content-Security-Policy":     "default-src 'self'",
		"X-Content-Type-Options":      "nosniff",
		"X-Frame-Options":             "SAMEORIGIN",
		"Referrer-Policy":             "no-referrer",
		"Access-Control-Allow-Origin": "https://a.example",
	} {
		if got := string(ctx.Response.Header.Peek(k)); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
