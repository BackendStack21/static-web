package security_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// B4: a path that did not exist must not be memoised as "safe": the
// filesystem can change (e.g. a symlink appears) before the next request.
func TestPathCache_DoesNotCacheMissingPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const marker = "outside file marker"
	if err := os.WriteFile(filepath.Join(outside, "x.txt"), []byte(marker), 0644); err != nil {
		t.Fatal(err)
	}

	pc := security.NewPathCache(100)
	// Downstream handler mirrors the file handler: PathCache first, then PathSafe.
	next := func(ctx *fasthttp.RequestCtx) {
		urlPath := string(ctx.Path())
		p, ok := pc.Lookup(urlPath)
		if !ok {
			var err error
			if p, err = security.PathSafe(urlPath, root, true); err != nil {
				ctx.SetStatusCode(fasthttp.StatusForbidden)
				return
			}
		}
		data, err := os.ReadFile(p)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			return
		}
		ctx.SetBody(data)
	}
	cfg := &config.SecurityConfig{BlockDotfiles: true}
	h := security.Middleware(cfg, root, next, pc)

	get := func() *fasthttp.RequestCtx {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod("GET")
		ctx.Request.SetRequestURI("/up/x.txt")
		h(&ctx)
		return &ctx
	}

	first := get()
	if first.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Fatalf("first status = %d, want 404", first.Response.StatusCode())
	}
	if _, ok := pc.Lookup("/up/x.txt"); ok {
		t.Error("PathCache stored a path that did not exist")
	}

	if err := os.Symlink(outside, filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	second := get()
	if strings.Contains(string(second.Response.Body()), marker) || second.Response.StatusCode() == fasthttp.StatusOK {
		t.Errorf("second request served outside content: status %d body %q", second.Response.StatusCode(), second.Response.Body())
	}
}
