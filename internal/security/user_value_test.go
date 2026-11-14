package security

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// After a PathCache hit the middleware validated no path during this request,
// so SafePathFromCtx must report nothing -- even if an outer layer put a value
// under the same key -- and the file handler re-validates on a cache miss.
func TestMiddleware_PathCacheHitDoesNotHandOverForeignSafePath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := NewPathCache(16)
	var got string
	var ok bool
	next := func(ctx *fasthttp.RequestCtx) { got, ok = SafePathFromCtx(ctx) }
	mw := Middleware(&config.SecurityConfig{BlockDotfiles: true}, root, next, pc)

	// First request validates and stores the verdict.
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/a.txt")
	mw(ctx)
	if !ok {
		t.Fatalf("first request: SafePathFromCtx not set")
	}

	// Second request hits the PathCache; an outer layer planted a value.
	ctx = &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/a.txt")
	ctx.SetUserValue(safePathUserValueKey, "/etc/passwd")
	got, ok = "", false
	mw(ctx)
	if ok {
		t.Errorf("PathCache hit handed over %q, want no validated path", got)
	}
}
