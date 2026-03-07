package security_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/static-web/server/internal/config"
	"github.com/static-web/server/internal/security"
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

func TestMiddleware_BlocksDotfile(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{BlockDotfiles: true}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/.env", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("CSP = %q, want default-src 'self'", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
	if got := rr.Header().Get("Permissions-Policy"); got != "geolocation=(), microphone=(), camera=()" {
		t.Errorf("Permissions-Policy = %q, want geolocation=(), microphone=(), camera=()", got)
	}
}

func TestMiddleware_NullByte(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	req.URL.Path = "/foo\x00bar"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestMiddleware_PassesValidRequest(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{BlockDotfiles: true, CSP: "default-src 'self'"}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next handler should be called for valid path")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/file.js", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next handler should be called for allowed CORS origin")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO = %q, want https://example.com", got)
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

func TestMiddleware_CORS_WildcardOrigin(t *testing.T) {
	root := t.TempDir()
	cfg := &config.SecurityConfig{
		BlockDotfiles: false,
		CORSOrigins:   []string{"*"},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/asset.css", nil)
	req.Header.Set("Origin", "https://random-domain.io")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Wildcard must emit literal "*", NOT the reflected origin (SEC-005).
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want literal * for wildcard config (must not reflect origin)", got)
	}
	// Vary: Origin must NOT be set when using wildcard.
	if got := rr.Header().Get("Vary"); strings.Contains(got, "Origin") {
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/file.js", nil)
	req.Header.Set("Origin", "https://evil.com")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// next is still called (origin just doesn't get CORS headers)
	if !called {
		t.Error("next should still be called for disallowed CORS origin")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodOptions, "/api/data", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// OPTIONS preflight must NOT call next and must return 204.
	if called {
		t.Error("next should NOT be called for CORS preflight OPTIONS")
	}
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for CORS preflight", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods should be set on OPTIONS preflight")
	}
	if got := rr.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Errorf("Access-Control-Max-Age = %q, want 86400", got)
	}
}

func TestMiddleware_CORS_NoCORSConfigured(t *testing.T) {
	root := t.TempDir()
	// No CORSOrigins — CORS block should be entirely skipped.
	cfg := &config.SecurityConfig{BlockDotfiles: false}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/page.html", nil)
	req.Header.Set("Origin", "https://any.com")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next should be called when no CORS origins configured")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/file.txt", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Security-Policy"); got != "" {
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := security.Middleware(cfg, root, next)

	disallowed := []string{
		http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodPatch, "TRACE", "CONNECT",
	}
	for _, method := range disallowed {
		called = false
		req := httptest.NewRequest(method, "/file.txt", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if called {
			t.Errorf("next should NOT be called for method %s", method)
		}
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: status = %d, want 405", method, rr.Code)
		}
	}
}

func TestMiddleware_MethodWhitelist_Allowed(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "file.txt"), []byte("hi"), 0644)
	cfg := &config.SecurityConfig{}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		handler := security.Middleware(cfg, root, next)
		req := httptest.NewRequest(method, "/file.txt", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if !called {
			t.Errorf("next should be called for method %s", method)
		}
		if rr.Code != http.StatusOK {
			t.Errorf("method %s: status = %d, want 200", method, rr.Code)
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := security.Middleware(cfg, root, next)
	req := httptest.NewRequest(http.MethodGet, "/.env", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	// Security headers must be present even on 403 error responses.
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q on 403, want nosniff", got)
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("CSP = %q on 403, want default-src 'self'", got)
	}
}
