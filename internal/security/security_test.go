package security_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

func TestPathSafe_ValidPaths(t *testing.T) {
	root := t.TempDir()
	// Resolve the root itself (macOS /var → /private/var symlink).
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}

	cases := []struct {
		name    string
		urlPath string
	}{
		{"root", "/"},
		{"simple file", "/index.html"},
		{"nested", "/assets/style.css"},
		{"no leading slash", "index.html"},
		// path.Clean normalises traversal attempts before join — they stay inside root.
		{"traversal collapsed to root", "/../etc/passwd"},
		{"double traversal", "/../../secret"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := security.PathSafe(tc.urlPath, root, false)
			if err != nil {
				t.Errorf("PathSafe(%q, %q) unexpected error: %v", tc.urlPath, root, err)
			}
			if got == "" {
				t.Error("PathSafe returned empty path on success")
			}
			// Verify result is still inside the real (symlink-resolved) root.
			rel, relErr := filepath.Rel(realRoot, got)
			if relErr != nil {
				t.Errorf("result %q not relative to root %q: %v", got, realRoot, relErr)
			}
			if len(rel) > 2 && rel[:3] == "../" {
				t.Errorf("result %q escapes root %q (rel=%q)", got, realRoot, rel)
			}
		})
	}
}

func TestPathSafe_NullByte(t *testing.T) {
	root := t.TempDir()
	_, err := security.PathSafe("/foo\x00bar", root, false)
	if !errors.Is(err, security.ErrNullByte) {
		t.Errorf("expected ErrNullByte, got %v", err)
	}
}

func TestPathSafe_Dotfiles(t *testing.T) {
	root := t.TempDir()

	dotPaths := []string{
		"/.hidden",
		"/.git/config",
		"/assets/.secret",
	}

	for _, p := range dotPaths {
		t.Run(p, func(t *testing.T) {
			_, err := security.PathSafe(p, root, true)
			if !errors.Is(err, security.ErrDotfile) {
				t.Errorf("PathSafe(%q, blockDotfiles=true) = %v, want ErrDotfile", p, err)
			}
		})
	}
}

func TestPathSafe_DotfilesAllowed(t *testing.T) {
	root := t.TempDir()
	_, err := security.PathSafe("/.hidden", root, false)
	if err != nil {
		t.Errorf("expected no error with blockDotfiles=false, got %v", err)
	}
}

func TestPathSafe_AbsoluteResult(t *testing.T) {
	root := t.TempDir()
	// Resolve the root itself for correct comparison on macOS.
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}

	got, err := security.PathSafe("/sub/file.html", root, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(realRoot, "sub", "file.html")
	if got != want {
		t.Errorf("PathSafe = %q, want %q", got, want)
	}
}

func TestPathSafe_ResultAlwaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	// Resolve the root itself for correct comparison on macOS.
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}

	// All of these URL paths, regardless of traversal sequences, must produce
	// a result that is inside root after path.Clean normalisation.
	paths := []string{
		"/../etc/passwd",
		"/../../secret",
		"/.././..",
		"../outside",
		"/a/../../b",
		"/normal/file.html",
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			got, err := security.PathSafe(p, root, false)
			if err != nil {
				// Only ErrDotfile/ErrNullByte acceptable, not traversal in these cases.
				if !errors.Is(err, security.ErrDotfile) {
					t.Errorf("PathSafe(%q) unexpected error: %v", p, err)
				}
				return
			}
			// Confirm it's actually inside root.
			if len(got) < len(realRoot) {
				t.Errorf("result %q is shorter than root %q — possible escape", got, realRoot)
			}
		})
	}
}

// newSecurityCtx creates a fasthttp.RequestCtx with the given method and URI.
func newSecurityCtx(method, uri string) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	return &ctx
}

func TestMiddleware_BlocksDotfile(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{BlockDotfiles: true}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/.env")
	handler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Errorf("status = %d, want 403", ctx.Response.StatusCode())
	}
}

func TestMiddleware_SetsSecurityHeaders(t *testing.T) {
	root := t.TempDir()
	// Create a real file so the handler chain succeeds.
	_ = os.WriteFile(filepath.Join(root, "index.html"), []byte("hi"), 0644)

	cfg := &config.SecurityConfig{
		BlockDotfiles:     true,
		CSP:               "default-src 'self'",
		ReferrerPolicy:    "strict-origin-when-cross-origin",
		PermissionsPolicy: "geolocation=(), microphone=(), camera=()",
	}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/index.html")
	handler(ctx)

	if got := string(ctx.Response.Header.Peek("X-Content-Type-Options")); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := string(ctx.Response.Header.Peek("X-Frame-Options")); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if got := string(ctx.Response.Header.Peek("Content-Security-Policy")); got != "default-src 'self'" {
		t.Errorf("CSP = %q, want default-src 'self'", got)
	}
	if got := string(ctx.Response.Header.Peek("Referrer-Policy")); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
	if got := string(ctx.Response.Header.Peek("Permissions-Policy")); got != "geolocation=(), microphone=(), camera=()" {
		t.Errorf("Permissions-Policy = %q, want geolocation=(), microphone=(), camera=()", got)
	}
}

func TestMiddleware_NullByte(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	// fasthttp SetRequestURI won't allow null bytes in path easily,
	// so we set the URI to something valid and then use raw path injection.
	ctx.Request.SetRequestURI("/foo\x00bar")
	handler(&ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Errorf("status = %d, want 400", ctx.Response.StatusCode())
	}
}

func TestMiddleware_PassesValidRequest(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{BlockDotfiles: true, CSP: "default-src 'self'"}
	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/style.css")
	handler(ctx)

	if !called {
		t.Error("next handler should be called for valid path")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200", ctx.Response.StatusCode())
	}
}

// ---------------------------------------------------------------------------
// CORS / originAllowed coverage
// ---------------------------------------------------------------------------

func TestMiddleware_CORS_AllowedOrigin(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{
		BlockDotfiles: false,
		CORSOrigins:   []string{"https://example.com"},
	}
	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/file.js")
	ctx.Request.Header.Set("Origin", "https://example.com")
	handler(ctx)

	if !called {
		t.Error("next handler should be called for allowed CORS origin")
	}
	if got := string(ctx.Response.Header.Peek("Access-Control-Allow-Origin")); got != "https://example.com" {
		t.Errorf("ACAO = %q, want https://example.com", got)
	}
	if got := string(ctx.Response.Header.Peek("Vary")); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

func TestMiddleware_CORS_WildcardOrigin(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{
		BlockDotfiles: false,
		CORSOrigins:   []string{"*"},
	}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/asset.css")
	ctx.Request.Header.Set("Origin", "https://random-domain.io")
	handler(ctx)

	// Wildcard must emit literal "*", NOT the reflected origin (SEC-005).
	if got := string(ctx.Response.Header.Peek("Access-Control-Allow-Origin")); got != "*" {
		t.Errorf("ACAO = %q, want literal * for wildcard config (must not reflect origin)", got)
	}
	// Vary: Origin must NOT be set when using wildcard.
	if got := string(ctx.Response.Header.Peek("Vary")); strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, must not contain Origin when wildcard is configured", got)
	}
}

func TestMiddleware_CORS_DisallowedOrigin(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{
		BlockDotfiles: false,
		CORSOrigins:   []string{"https://allowed.com"},
	}
	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/file.js")
	ctx.Request.Header.Set("Origin", "https://evil.com")
	handler(ctx)

	// next is still called (origin just doesn't get CORS headers)
	if !called {
		t.Error("next should still be called for disallowed CORS origin")
	}
	if got := string(ctx.Response.Header.Peek("Access-Control-Allow-Origin")); got != "" {
		t.Errorf("ACAO should be empty for disallowed origin, got %q", got)
	}
}

func TestMiddleware_CORS_PreflightOptions(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{
		BlockDotfiles: false,
		CORSOrigins:   []string{"https://example.com"},
	}
	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("OPTIONS", "/api/data")
	ctx.Request.Header.Set("Origin", "https://example.com")
	handler(ctx)

	// OPTIONS preflight must NOT call next and must return 204.
	if called {
		t.Error("next should NOT be called for CORS preflight OPTIONS")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusNoContent {
		t.Errorf("status = %d, want 204 for CORS preflight", ctx.Response.StatusCode())
	}
	if got := string(ctx.Response.Header.Peek("Access-Control-Allow-Methods")); got == "" {
		t.Error("Access-Control-Allow-Methods should be set on OPTIONS preflight")
	}
	if got := string(ctx.Response.Header.Peek("Access-Control-Max-Age")); got != "86400" {
		t.Errorf("Access-Control-Max-Age = %q, want 86400", got)
	}
}

func TestMiddleware_CORS_NoCORSConfigured(t *testing.T) {
	root := t.TempDir()
	// No CORSOrigins — CORS block should be entirely skipped.
	cfg := &config.SecurityConfig{BlockDotfiles: false}
	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/page.html")
	ctx.Request.Header.Set("Origin", "https://any.com")
	handler(ctx)

	if !called {
		t.Error("next should be called when no CORS origins configured")
	}
	if got := string(ctx.Response.Header.Peek("Access-Control-Allow-Origin")); got != "" {
		t.Errorf("ACAO should be empty when CORS not configured, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Additional PathSafe edge cases
// ---------------------------------------------------------------------------

func TestPathSafe_EncodedTraversalVariants(t *testing.T) {
	root := t.TempDir()
	// Resolve the root itself for correct comparison on macOS.
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}

	// These URL-encoded or otherwise crafted paths must all stay inside root.
	cases := []struct {
		name    string
		urlPath string
	}{
		{"percent-encoded dot-dot", "/%2e%2e/etc/passwd"},
		{"mixed slash traversal", "/assets/../../../etc/passwd"},
		{"triple dot", "/..."},
		{"spaces in path", "/path with spaces/file.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := security.PathSafe(tc.urlPath, root, false)
			if err != nil {
				// Only dotfile/null errors are acceptable; traversal should NOT occur.
				if errors.Is(err, security.ErrPathTraversal) {
					// PathSafe already blocked it — that's also acceptable.
					return
				}
				if errors.Is(err, security.ErrDotfile) {
					return
				}
				t.Errorf("PathSafe(%q) unexpected error: %v", tc.urlPath, err)
				return
			}
			// Result must be inside realRoot.
			if !strings.HasPrefix(got, realRoot) && got != realRoot {
				t.Errorf("PathSafe(%q) = %q escapes root %q", tc.urlPath, got, realRoot)
			}
		})
	}
}

func TestPathSafe_WithDotfileInMiddleSegment(t *testing.T) {
	root := t.TempDir()
	// Path like /valid/.hidden/file — the middle segment starts with a dot.
	_, err := security.PathSafe("/valid/.hidden/file.txt", root, true)
	if !errors.Is(err, security.ErrDotfile) {
		t.Errorf("expected ErrDotfile for path with hidden segment, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Symlink escape prevention (SEC-001)
// ---------------------------------------------------------------------------

func TestPathSafe_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// Create a symlink inside root that points outside root.
	symlinkPath := filepath.Join(root, "escape")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Skipf("cannot create symlink (may require elevated privileges): %v", err)
	}

	_, err := security.PathSafe("/escape", root, false)
	if !errors.Is(err, security.ErrPathTraversal) {
		t.Errorf("expected ErrPathTraversal for symlink escape, got %v", err)
	}
}

func TestPathSafe_SymlinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	// Resolve root for comparisons.
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}

	// Create a real target inside root.
	targetPath := filepath.Join(root, "real.html")
	if err := os.WriteFile(targetPath, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a symlink that points to it (also inside root) — should be allowed.
	symlinkPath := filepath.Join(root, "link.html")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := security.PathSafe("/link.html", root, false)
	if err != nil {
		t.Errorf("unexpected error for safe symlink: %v", err)
	}
	want := filepath.Join(realRoot, "real.html")
	if got != want {
		t.Errorf("PathSafe symlink = %q, want %q", got, want)
	}
}

func TestPathSafe_NonExistentPath(t *testing.T) {
	root := t.TempDir()
	// Resolve root for comparisons.
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}

	// Non-existent path should succeed (returns unresolved candidate).
	got, err := security.PathSafe("/nonexistent/file.html", root, false)
	if err != nil {
		t.Errorf("unexpected error for non-existent path: %v", err)
	}
	want := filepath.Join(realRoot, "nonexistent", "file.html")
	if got != want {
		t.Errorf("PathSafe non-existent = %q, want %q", got, want)
	}
}

func TestPathSafe_CSPNotSetWhenEmpty(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "file.txt"), []byte("hi"), 0644)

	// CSP is empty — header must NOT be set.
	cfg := &config.SecurityConfig{BlockDotfiles: false, CSP: ""}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/file.txt")
	handler(ctx)

	if got := string(ctx.Response.Header.Peek("Content-Security-Policy")); got != "" {
		t.Errorf("CSP should NOT be set when config CSP is empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Method whitelist (SEC-009)
// ---------------------------------------------------------------------------

func TestMiddleware_MethodWhitelist(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{}
	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}
	handler := security.Middleware(cfg, root, next)

	disallowed := []string{
		"POST", "PUT", "DELETE",
		"PATCH", "TRACE", "CONNECT",
	}
	for _, method := range disallowed {
		called = false
		ctx := newSecurityCtx(method, "/file.txt")
		handler(ctx)

		if called {
			t.Errorf("next should NOT be called for method %s", method)
		}
		if ctx.Response.StatusCode() != fasthttp.StatusMethodNotAllowed {
			t.Errorf("method %s: status = %d, want 405", method, ctx.Response.StatusCode())
		}
	}
}

func TestMiddleware_MethodWhitelist_Allowed(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "file.txt"), []byte("hi"), 0644)
	cfg := &config.SecurityConfig{}

	for _, method := range []string{"GET", "HEAD"} {
		called := false
		next := func(ctx *fasthttp.RequestCtx) {
			called = true
			ctx.SetStatusCode(fasthttp.StatusOK)
		}
		handler := security.Middleware(cfg, root, next)
		ctx := newSecurityCtx(method, "/file.txt")
		handler(ctx)

		if !called {
			t.Errorf("next should be called for method %s", method)
		}
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Errorf("method %s: status = %d, want 200", method, ctx.Response.StatusCode())
		}
	}
}

// ---------------------------------------------------------------------------
// Security headers on error responses (SEC-006)
// ---------------------------------------------------------------------------

func TestMiddleware_SecurityHeadersOnForbidden(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{
		BlockDotfiles: true,
		CSP:           "default-src 'self'",
	}
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := security.Middleware(cfg, root, next)
	ctx := newSecurityCtx("GET", "/.env")
	handler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Fatalf("status = %d, want 403", ctx.Response.StatusCode())
	}
	// Security headers must be present even on 403 error responses.
	if got := string(ctx.Response.Header.Peek("X-Content-Type-Options")); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q on 403, want nosniff", got)
	}
	if got := string(ctx.Response.Header.Peek("Content-Security-Policy")); got != "default-src 'self'" {
		t.Errorf("CSP = %q on 403, want default-src 'self'", got)
	}
}
