package handler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// HTTP-F4: a directory that cannot be read answers 403 without losing the
// security headers set by the security middleware.
func TestDirListing_UnreadableDirKeepsSecurityHeaders(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 directory")
	}
	root, cfg := setupDirListingRoot(t)
	cfg.Security.CSP = "default-src 'self'"
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	ctx := newTestCtx("GET", "/locked/")
	h(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "SAMEORIGIN",
	} {
		if got := string(ctx.Response.Header.Peek(k)); got != want {
			t.Errorf("%s = %q on 403, want %q", k, got, want)
		}
	}
}
