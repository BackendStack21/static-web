package handler

import (
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// headersThatMustSurviveAnError are set by outer middleware (security, CORS,
// HSTS) before the inner handler runs.
var headersThatMustSurviveAnError = map[string]string{
	"Content-Security-Policy":     "default-src 'self'",
	"X-Content-Type-Options":      "nosniff",
	"X-Frame-Options":             "SAMEORIGIN",
	"Referrer-Policy":             "no-referrer",
	"Permissions-Policy":          "camera=()",
	"Access-Control-Allow-Origin": "https://a.example",
	"Vary":                        "Origin",
	"Strict-Transport-Security":   "max-age=31536000",
}

// representationHeaders describe a successful body that an error replaces.
var representationHeadersSetBeforeError = map[string]string{
	"Content-Encoding": "gzip",
	"Content-Range":    "bytes 0-9/100",
	"ETag":             `W/"abc"`,
	"Last-Modified":    "Mon, 01 Jan 2024 00:00:00 GMT",
	"Cache-Control":    "public, max-age=3600",
	"Accept-Ranges":    "bytes",
	"X-Cache":          "MISS",
}

func TestWriteError_KeepsSecurityHeadersAndDropsRepresentationHeaders(t *testing.T) {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/big.bin")
	for k, v := range headersThatMustSurviveAnError {
		ctx.Response.Header.Set(k, v)
	}
	for k, v := range representationHeadersSetBeforeError {
		ctx.Response.Header.Set(k, v)
	}
	ctx.Response.Header.Set("Content-Type", "multipart/byteranges; boundary=x")
	ctx.SetBodyStream(strings.NewReader(strings.Repeat("x", 100)), 100)

	writeError(&ctx, "Forbidden", fasthttp.StatusForbidden)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
	if got := string(ctx.Response.Header.ContentType()); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if ctx.Response.IsBodyStream() {
		t.Error("body stream still attached after writeError")
	}
	if got := string(ctx.Response.Body()); got != "Forbidden" {
		t.Errorf("body = %q, want Forbidden", got)
	}
	for k, want := range headersThatMustSurviveAnError {
		if got := string(ctx.Response.Header.Peek(k)); got != want {
			t.Errorf("%s = %q after writeError, want %q (must be preserved)", k, got, want)
		}
	}
	for k := range representationHeadersSetBeforeError {
		if got := ctx.Response.Header.Peek(k); len(got) != 0 {
			t.Errorf("%s = %q after writeError, want it removed", k, got)
		}
	}
}

func TestRecoveryMiddleware_PanicKeepsSecurityHeaders(t *testing.T) {
	cfg := &config.SecurityConfig{
		CSP:         "default-src 'self'",
		CORSOrigins: []string{"https://a.example"},
	}
	panicking := func(ctx *fasthttp.RequestCtx) {
		// A handler that had started building a cacheable, encoded response.
		ctx.Response.Header.Set("Content-Encoding", "gzip")
		ctx.Response.Header.Set("Cache-Control", "public, max-age=3600")
		ctx.Response.Header.Set("ETag", `W/"abc"`)
		panic("boom")
	}
	h := recoveryMiddleware(security.Middleware(cfg, t.TempDir(), panicking))

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.Set("Origin", "https://a.example")
	h(&ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got)
	}
	for k, want := range map[string]string{
		"Content-Security-Policy":     "default-src 'self'",
		"X-Content-Type-Options":      "nosniff",
		"X-Frame-Options":             "SAMEORIGIN",
		"Access-Control-Allow-Origin": "https://a.example",
	} {
		if got := string(ctx.Response.Header.Peek(k)); got != want {
			t.Errorf("%s = %q on recovered 500, want %q", k, got, want)
		}
	}
	for _, k := range []string{"Content-Encoding", "Cache-Control", "ETag"} {
		if got := ctx.Response.Header.Peek(k); len(got) != 0 {
			t.Errorf("%s = %q on recovered 500, want it removed", k, got)
		}
	}
	if got := string(ctx.Response.Body()); got != "Internal Server Error" {
		t.Errorf("body = %q, want Internal Server Error", got)
	}
}
