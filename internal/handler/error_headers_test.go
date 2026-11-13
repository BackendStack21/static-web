package handler_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// ehAssertSecurityHeaders checks the headers security.Middleware set before
// the file handler answered with an error.
func ehAssertSecurityHeaders(t *testing.T, label string, ctx *fasthttp.RequestCtx, origin string) {
	t.Helper()
	for k, want := range map[string]string{
		"Content-Security-Policy":     "default-src 'self'",
		"X-Content-Type-Options":      "nosniff",
		"X-Frame-Options":             "SAMEORIGIN",
		"Access-Control-Allow-Origin": origin,
	} {
		if got := string(ctx.Response.Header.Peek(k)); got != want {
			t.Errorf("%s: %s = %q, want %q", label, k, got, want)
		}
	}
	if vary := string(ctx.Response.Header.Peek("Vary")); !strings.Contains(vary, "Origin") {
		t.Errorf("%s: Vary = %q, want it to contain Origin", label, vary)
	}
}

// HTTP-F4: the file handler's 413 and 403 responses were written with
// ctx.Error, which resets the response and dropped the CSP, nosniff, frame
// and CORS headers that security.Middleware had already set. The 413 body
// also disclosed the configured max_serve_file_size.
func TestErrorResponses_KeepSecurityAndCORSHeaders(t *testing.T) {
	root, cfg := setupTestDir(t)
	const origin = "https://a.example"
	cfg.Security.CORSOrigins = []string{origin}
	cfg.Files.MaxServeFileSize = 2 << 20
	big := make([]byte, 3<<20)
	if err := os.WriteFile(filepath.Join(root, "huge.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	t.Run("413 over max_serve_file_size", func(t *testing.T) {
		ctx := newTestCtx("GET", "/huge.bin")
		ctx.Request.Header.Set("Origin", origin)
		h(ctx)
		if got := ctx.Response.StatusCode(); got != fasthttp.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", got)
		}
		ehAssertSecurityHeaders(t, "413", ctx, origin)
		body := string(ctx.Response.Body())
		if body != "Payload Too Large" {
			t.Errorf("413 body = %q, want the generic %q", body, "Payload Too Large")
		}
		if strings.Contains(body, strconv.FormatInt(cfg.Files.MaxServeFileSize, 10)) {
			t.Errorf("413 body %q discloses the configured limit", body)
		}
	})

	if os.Geteuid() == 0 {
		t.Skip("root can read mode 0000 files")
	}
	for name, size := range map[string]int{"small": 10, "large": 2 << 20} {
		file := "noperm-" + name + ".txt"
		if err := os.WriteFile(filepath.Join(root, file), make([]byte, size), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Run("403 unreadable "+name+" file", func(t *testing.T) {
			ctx := newTestCtx("GET", "/"+file)
			ctx.Request.Header.Set("Origin", origin)
			h(ctx)
			if got := ctx.Response.StatusCode(); got != fasthttp.StatusForbidden {
				t.Fatalf("status = %d, want 403", got)
			}
			ehAssertSecurityHeaders(t, "403 "+name, ctx, origin)
		})
	}
}
