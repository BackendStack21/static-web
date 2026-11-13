package handler_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// prewarmedHandler preloads and pre-warms the caches the way main.go does
// with --preload, using root exactly as given (absolute or relative).
func prewarmedHandler(t *testing.T, cfg *config.Config, root string, maxFileSize int64) fasthttp.RequestHandler {
	t.Helper()
	cfg.Files.Root = root
	c := cache.NewCache(cfg.Cache.MaxBytes, 30*time.Millisecond)
	st := c.Preload(root, cache.PreloadConfig{MaxFileSize: maxFileSize, IndexFile: "index.html", BlockDotfiles: true})
	pc := security.NewPathCache(100)
	pc.PreWarm(st.Paths, root, true)
	return handler.BuildHandlerQuiet(cfg, c, pc)
}

// FS-2: a sidecar added after preload must be picked up once the cache entry
// expires, whether files.root is absolute or relative (the default ./public).
func TestPreWarm_RelativeRootStillServesNewSidecar(t *testing.T) {
	for _, relative := range []bool{false, true} {
		name := map[bool]string{false: "absolute", true: "relative"}[relative]
		t.Run(name, func(t *testing.T) {
			root, cfg := setupTestDir(t)
			root, _ = filepath.EvalSymlinks(root)
			use := root
			if relative {
				t.Chdir(filepath.Dir(root))
				use = "./" + filepath.Base(root)
			}
			h := prewarmedHandler(t, cfg, use, 1<<20)

			if err := os.WriteFile(filepath.Join(root, "style.css.br"), []byte("BRSIDE"), 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)

			ctx := newTestCtx("GET", "/style.css")
			ctx.Request.Header.Set("Accept-Encoding", "br")
			h(ctx)
			if ctx.Response.StatusCode() != 200 {
				t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
			}
			if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "br" {
				t.Errorf("Content-Encoding = %q, want br (sidecar ignored)", enc)
			}
		})
	}
}

// FS-2: a file that grows past max_file_size after preload must be served as
// a large file, not refused, whether files.root is absolute or relative.
func TestPreWarm_RelativeRootServesFileThatGrew(t *testing.T) {
	for _, relative := range []bool{false, true} {
		name := map[bool]string{false: "absolute", true: "relative"}[relative]
		t.Run(name, func(t *testing.T) {
			root, cfg := setupTestDir(t)
			root, _ = filepath.EvalSymlinks(root)
			cfg.Cache.MaxFileSize = 64
			use := root
			if relative {
				t.Chdir(filepath.Dir(root))
				use = "./" + filepath.Base(root)
			}
			h := prewarmedHandler(t, cfg, use, 64)

			grown := bytes.Repeat([]byte("var a=1;"), 64)
			if err := os.WriteFile(filepath.Join(root, "app.js"), grown, 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)

			ctx := newTestCtx("GET", "/app.js")
			h(ctx)
			if ctx.Response.StatusCode() != 200 {
				t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
			}
			if !bytes.Equal(ctx.Response.Body(), grown) {
				t.Errorf("body length %d, want the grown file (%d bytes)", len(ctx.Response.Body()), len(grown))
			}
		})
	}
}
