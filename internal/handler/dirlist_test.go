package handler_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/static-web/server/internal/cache"
	"github.com/static-web/server/internal/config"
	"github.com/static-web/server/internal/handler"
)

// setupDirListingRoot creates a temporary directory tree for directory listing tests:
//
//	root/
//	  index.html
//	  about.html
//	  style.css
//	  .secret         ← dotfile
//	  subdir/
//	    page.html
//	  .hidden/         ← hidden directory
//	    file.txt
func setupDirListingRoot(t *testing.T) (string, *config.Config) {
	t.Helper()
	root := t.TempDir()

	tree := map[string]string{
		"index.html":       "<html>root index</html>",
		"about.html":       "<html>about</html>",
		"style.css":        "body{}",
		".secret":          "secret content",
		"subdir/page.html": "<html>subpage</html>",
		".hidden/file.txt": "hidden dir file",
	}
	for name, content := range tree {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = false // keep tests simple — no cache side effects
	cfg.Cache.MaxBytes = 10 * 1024 * 1024
	cfg.Cache.MaxFileSize = 1 * 1024 * 1024
	cfg.Compression.Enabled = false
	cfg.Security.BlockDotfiles = true
	cfg.Security.DirectoryListing = true
	cfg.Headers.StaticMaxAge = 3600

	return root, cfg
}

// buildDirListHandler is a helper that builds a handler with DirectoryListing enabled.
func buildDirListHandler(t *testing.T) (string, http.Handler) {
	t.Helper()
	root, cfg := setupDirListingRoot(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	return root, handler.BuildHandler(cfg, c)
}

// ---------------------------------------------------------------------------
// Core behaviour
// ---------------------------------------------------------------------------

func TestDirListing_RootReturns200(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestDirListing_ContentTypeIsHTML(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestDirListing_ContainsExpectedFiles(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	for _, want := range []string{"about.html", "style.css", "subdir"} {
		if !strings.Contains(body, want) {
			t.Errorf("directory listing should contain %q\nbody:\n%s", want, body)
		}
	}
}

func TestDirListing_HidesDotfilesWhenBlocked(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	if strings.Contains(body, ".secret") {
		t.Error("dotfile .secret should not appear in directory listing when block_dotfiles=true")
	}
	if strings.Contains(body, ".hidden") {
		t.Error("hidden directory .hidden should not appear in directory listing when block_dotfiles=true")
	}
}

func TestDirListing_ShowsDotfilesWhenAllowed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".visible"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = false
	cfg.Cache.MaxBytes = 1024 * 1024
	cfg.Cache.MaxFileSize = 512 * 1024
	cfg.Compression.Enabled = false
	cfg.Security.BlockDotfiles = false // allow dotfiles
	cfg.Security.DirectoryListing = true

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !strings.Contains(rr.Body.String(), ".visible") {
		t.Error(".visible dotfile should appear in listing when block_dotfiles=false")
	}
}

func TestDirListing_ContainsBreadcrumb(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/subdir/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "subdir") {
		t.Error("breadcrumb should contain directory name")
	}
}

func TestDirListing_ContainsParentLink(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/subdir/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "..") {
		t.Error("subdirectory listing should contain a parent (..) link")
	}
}

func TestDirListing_RootHasNoParentLink(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := rr.Body.String()
	// The ".." entry is only added for non-root paths.
	if strings.Contains(body, `href="..`) {
		t.Error("root directory listing should not contain a .. link")
	}
}

func TestDirListing_SubdirReturns200(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/subdir/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "page.html") {
		t.Error("subdir listing should contain page.html")
	}
}

func TestDirListing_DisabledFallsBackToIndex(t *testing.T) {
	_, cfg := setupDirListingRoot(t)
	cfg.Security.DirectoryListing = false // disable listing

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (index.html fallback)", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "root index") {
		t.Error("with listing disabled, / should serve index.html, not a directory listing")
	}
	// Make sure we didn't accidentally serve the directory listing.
	if strings.Contains(body, "<table") {
		t.Error("directory listing table should not appear when listing is disabled")
	}
}

func TestDirListing_FileRequestStillWorks(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/about.html", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for direct file request", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "about") {
		t.Error("file contents should be served for direct file requests")
	}
}

func TestDirListing_HeadRequest(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD response body should be empty, got %d bytes", rr.Body.Len())
	}
}

func TestDirListing_SecurityHeadersPresent(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
}

func TestDirListing_NonExistentSubdir(t *testing.T) {
	_, h := buildDirListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for non-existent directory", rr.Code)
	}
}
