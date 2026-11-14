package handler_test

import (
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// A trailing slash names a directory. Requesting a regular file with one
// ("/app.js/") must be a 404, not serve the file and cache it a second time
// under the "/app.js/" key.
func TestTrailingSlash_OnRegularFileIs404(t *testing.T) {
	for _, withPC := range []bool{false, true} {
		name := map[bool]string{false: "NoPathCache", true: "PathCache"}[withPC]
		t.Run(name, func(t *testing.T) {
			_, cfg := setupTestDir(t)
			c := cache.NewCache(cfg.Cache.MaxBytes)
			var h fasthttp.RequestHandler
			if withPC {
				h = handler.BuildHandlerQuiet(cfg, c, security.NewPathCache(100))
			} else {
				h = handler.BuildHandlerQuiet(cfg, c)
			}

			// Both before and after the file itself is cached.
			for i := 0; i < 2; i++ {
				for _, uri := range []string{"/app.js/", "/subdir/page.html/"} {
					ctx := newTestCtx("GET", uri)
					h(ctx)
					if sc := ctx.Response.StatusCode(); sc != fasthttp.StatusNotFound {
						t.Errorf("round %d: GET %s: status = %d, want 404", i, uri, sc)
					}
				}
				ok := newTestCtx("GET", "/app.js")
				h(ok)
				if ok.Response.StatusCode() != 200 {
					t.Fatalf("GET /app.js: status = %d", ok.Response.StatusCode())
				}
			}
			if _, ok := c.Get("/app.js/"); ok {
				t.Error("file cached a second time under the trailing-slash key")
			}

			// Directories with a trailing slash still resolve to their index.
			ctx := newTestCtx("GET", "/")
			h(ctx)
			if ctx.Response.StatusCode() != 200 {
				t.Errorf("GET /: status = %d, want 200", ctx.Response.StatusCode())
			}
		})
	}
}
