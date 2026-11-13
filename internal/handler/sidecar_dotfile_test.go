package handler_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
)

// FS-3: a precompressed sidecar that is a symlink to an in-root dotfile must
// not be served when dotfiles are blocked; the request falls back to another
// variant instead of failing.
func TestSidecar_SymlinkToDotfileNotServed(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	const secret = "DB_PASSWORD=hunter2"
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	js := bytes.Repeat([]byte("var a=1;"), 50)
	if err := os.WriteFile(filepath.Join(root, "big.js"), js, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(root, "big.js.br")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".hidden", "x.gz"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".hidden/x.gz", filepath.Join(root, "big.js.gz")); err != nil {
		t.Fatal(err)
	}

	for _, ae := range []string{"br", "gzip", "br, gzip"} {
		h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
		ctx := newTestCtx("GET", "/big.js")
		ctx.Request.Header.Set("Accept-Encoding", ae)
		h(ctx)
		if strings.Contains(string(ctx.Response.Body()), secret) {
			t.Fatalf("AE %q: dotfile served through a sidecar (Content-Encoding %q)", ae, ctx.Response.Header.Peek("Content-Encoding"))
		}
		if ctx.Response.StatusCode() != 200 {
			t.Fatalf("AE %q: status = %d, want 200 (fall back to another variant)", ae, ctx.Response.StatusCode())
		}
		if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "br" {
			t.Errorf("AE %q: Content-Encoding = br, but the only br sidecar is a dotfile", ae)
		}
	}
}

func TestValidateSidecarPath_RejectsDotfileTarget(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(root, "app.js.br")); err != nil {
		t.Fatal(err)
	}

	fh := handler.NewFileHandler(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	if p, err := fh.ValidateSidecarPath(filepath.Join(root, "app.js.br")); err == nil {
		t.Errorf("ValidateSidecarPath accepted a sidecar resolving to a dotfile: %q", p)
	}

	cfg.Security.BlockDotfiles = false
	fh = handler.NewFileHandler(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	if _, err := fh.ValidateSidecarPath(filepath.Join(root, "app.js.br")); err != nil {
		t.Errorf("with dotfiles allowed, ValidateSidecarPath rejected the sidecar: %v", err)
	}
}
