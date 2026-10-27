package handler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/BackendStack21/static-web/internal/security"
)

func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// B3: directory index files must be validated like any other path.
func TestDirectoryIndex_SymlinkEscapeNotServed(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	outside := realTempDir(t)
	const secret = "TOP-SECRET-OUTSIDE"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "sub", "index.html")); err != nil {
		t.Fatal(err)
	}

	for _, uri := range []string{"/sub/index.html", "/sub/", "/sub"} {
		h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
		ctx := newTestCtx("GET", uri)
		h(ctx)
		if strings.Contains(string(ctx.Response.Body()), secret) || ctx.Response.StatusCode() == 200 {
			t.Errorf("GET %s: status %d, body leaked outside file: %q", uri, ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if sc := ctx.Response.StatusCode(); sc != 403 && sc != 404 {
			t.Errorf("GET %s: status = %d, want 403 or 404", uri, sc)
		}
	}
}

func TestDirectoryIndex_DotfileTargetNotServed(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	cfg.Security.BlockDotfiles = true
	const secret = "DB_PASSWORD=hunter2"
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "d2"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.env", filepath.Join(root, "d2", "index.html")); err != nil {
		t.Fatal(err)
	}

	for _, uri := range []string{"/d2/", "/d2"} {
		h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
		ctx := newTestCtx("GET", uri)
		h(ctx)
		if strings.Contains(string(ctx.Response.Body()), secret) || ctx.Response.StatusCode() == 200 {
			t.Errorf("GET %s: status %d, dotfile content served: %q", uri, ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if sc := ctx.Response.StatusCode(); sc != 403 && sc != 404 {
			t.Errorf("GET %s: status = %d, want 403 or 404", uri, sc)
		}
	}
}

// B4 (integration): with a PathCache, a missing path must not become a
// cached verdict that later serves a file behind a newly created symlink.
func TestPathCache_MissingPathThenSymlinkNotServed(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	cfg.Cache.Enabled = false
	outside := realTempDir(t)
	const secret = "OUTSIDE-SECRET"
	if err := os.WriteFile(filepath.Join(outside, "x.txt"), []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}
	pc := security.NewPathCache(100)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes), pc)

	ctx := newTestCtx("GET", "/up/x.txt")
	h(ctx)
	if ctx.Response.StatusCode() != 404 {
		t.Fatalf("first status = %d, want 404", ctx.Response.StatusCode())
	}
	if err := os.Symlink(outside, filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	ctx = newTestCtx("GET", "/up/x.txt")
	h(ctx)
	if strings.Contains(string(ctx.Response.Body()), secret) || ctx.Response.StatusCode() == 200 {
		t.Errorf("served outside content: status %d body %q", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}
