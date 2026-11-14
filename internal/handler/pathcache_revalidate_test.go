package handler_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

const outsideMarker = "TOP-SECRET-OUTSIDE"

// assertNotLeaked fails unless ctx carries a 403 or 404 without outside content.
func assertNotLeaked(t *testing.T, ctx *fasthttp.RequestCtx, what string) {
	t.Helper()
	sc := ctx.Response.StatusCode()
	if strings.Contains(string(ctx.Response.Body()), outsideMarker) {
		t.Fatalf("%s: served content from outside the root (status %d)", what, sc)
	}
	if sc != fasthttp.StatusForbidden && sc != fasthttp.StatusNotFound {
		t.Fatalf("%s: status = %d, want 403 or 404", what, sc)
	}
}

// FS-1: a large file (never in the file cache) is validated and its verdict
// stored in the PathCache; it is then swapped for a symlink leaving the root.
// The next request misses the file cache, so it must be re-validated.
func TestPathCache_LargeFileSymlinkSwapIsRevalidated(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	cfg.Cache.MaxFileSize = 64
	outside := realTempDir(t)
	secret := bytes.Repeat([]byte(outsideMarker+" "), 20)
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(root, "big.txt")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 500), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := security.NewPathCache(100)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes), pc)

	ctx := newTestCtx("GET", "/big.txt")
	h(ctx)
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("first request: status %d, want 200", ctx.Response.StatusCode())
	}
	if _, ok := pc.Lookup("/big.txt"); !ok {
		t.Fatal("precondition: /big.txt verdict not stored in the PathCache")
	}

	if err := os.Remove(big); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), big); err != nil {
		t.Fatal(err)
	}
	ctx = newTestCtx("GET", "/big.txt")
	h(ctx)
	assertNotLeaked(t, ctx, "GET /big.txt after symlink swap")
	if _, ok := pc.Lookup("/big.txt"); ok {
		t.Error("PathCache still holds the verdict for a path that failed re-validation")
	}
}

// FS-1: preloaded + pre-warmed caches; a directory is swapped for a symlink
// leaving the root, and the file-cache entry then expires (TTL).
func TestPathCache_TTLExpirySymlinkSwapIsRevalidated(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	outside := realTempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "page.html"), []byte(outsideMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	c := cache.NewCache(cfg.Cache.MaxBytes, 50*time.Millisecond)
	st := c.Preload(root, cache.PreloadConfig{MaxFileSize: 1 << 20, IndexFile: "index.html", BlockDotfiles: true})
	pc := security.NewPathCache(100)
	pc.PreWarm(st.Paths, root, true)
	if _, ok := pc.Lookup("/subdir/page.html"); !ok {
		t.Fatal("precondition: /subdir/page.html not pre-warmed")
	}
	h := handler.BuildHandlerQuiet(cfg, c, pc)

	if err := os.RemoveAll(filepath.Join(root, "subdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "subdir")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)

	ctx := newTestCtx("GET", "/subdir/page.html")
	h(ctx)
	assertNotLeaked(t, ctx, "GET /subdir/page.html after TTL expiry")
	if _, ok := pc.Lookup("/subdir/page.html"); ok {
		t.Error("PathCache still holds the verdict for a path that failed re-validation")
	}
}

// FS-1: same as the TTL case, but the file-cache entry is lost to LRU
// eviction instead of expiry.
func TestPathCache_EvictionSymlinkSwapIsRevalidated(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	cfg.Compression.Enabled = false
	outside := realTempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "page.html"), []byte(outsideMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "filler.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	// Room for the filler (4096 B + 256 B overhead) but not for the filler and
	// the page (20 B + 256 B overhead) together.
	c := cache.NewCache(4500)
	pc := security.NewPathCache(100)
	h := handler.BuildHandlerQuiet(cfg, c, pc)

	ctx := newTestCtx("GET", "/subdir/page.html")
	h(ctx)
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("first request: status %d", ctx.Response.StatusCode())
	}
	if _, ok := pc.Lookup("/subdir/page.html"); !ok {
		t.Fatal("precondition: verdict not stored in the PathCache")
	}
	h(newTestCtx("GET", "/filler.bin")) // evicts /subdir/page.html
	if _, ok := c.Get("/subdir/page.html"); ok {
		t.Fatal("precondition: /subdir/page.html was not evicted")
	}

	if err := os.RemoveAll(filepath.Join(root, "subdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "subdir")); err != nil {
		t.Fatal(err)
	}
	ctx = newTestCtx("GET", "/subdir/page.html")
	h(ctx)
	assertNotLeaked(t, ctx, "GET /subdir/page.html after eviction")
}

// A FileHandler used without security.Middleware must not trust a PathCache
// verdict before touching the disk.
func TestFileHandler_BareHandlerRevalidatesStalePathCacheEntry(t *testing.T) {
	root, cfg := setupTestDir(t)
	root, _ = filepath.EvalSymlinks(root)
	cfg.Files.Root = root
	outside := realTempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outsideMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "x.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Fatal(err)
	}
	pc := security.NewPathCache(100)
	pc.Store("/x.txt", link) // stale verdict: x.txt was a regular file when validated
	fh := handler.NewFileHandler(cfg, cache.NewCache(cfg.Cache.MaxBytes), pc)

	ctx := newTestCtx("GET", "/x.txt")
	fh.HandleRequest(ctx)
	assertNotLeaked(t, ctx, "bare handler GET /x.txt")
	if _, ok := pc.Lookup("/x.txt"); ok {
		t.Error("stale PathCache entry was not dropped after failing validation")
	}
}

// A FileHandler used without security.Middleware must run the lexical checks
// before the file-cache lookup: a dotfile key must never be served from cache,
// however the request spells it. (fasthttp always decodes and normalises
// ctx.Path(), so a raw ".." never reaches the handler; CheckLexical's own
// tests cover un-normalised input.)
func TestFileHandler_BareHandlerDoesNotServeDotfileKeyFromCache(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	secret := &cache.CachedFile{Data: []byte("DB_PASSWORD=hunter2"), ContentType: "text/plain", Size: 19}
	secret.InitHeaders()
	c.Put("/.env", secret)
	fh := handler.NewFileHandler(cfg, c, security.NewPathCache(100))

	for _, uri := range []string{"/.env", "/%2eenv", "/subdir/%2e%2e/.env", "/subdir/../.env"} {
		ctx := newTestCtx("GET", uri)
		fh.HandleRequest(ctx)
		if strings.Contains(string(ctx.Response.Body()), "hunter2") {
			t.Fatalf("GET %s: dotfile served from cache", uri)
		}
		if sc := ctx.Response.StatusCode(); sc != fasthttp.StatusForbidden {
			t.Errorf("GET %s: status = %d, want 403", uri, sc)
		}
	}
}

// P-F7: with a PathCache configured, a 404 must not validate the path twice
// (once in the middleware and again in the handler). Allocation counts are a
// deterministic proxy for the number of EvalSymlinks walks.
func TestNotFound_PathCacheDoesNotValidateTwice(t *testing.T) {
	_, cfg := setupTestDir(t)
	allocs := func(pc *security.PathCache) float64 {
		var h fasthttp.RequestHandler
		if pc != nil {
			h = handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes), pc)
		} else {
			h = handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
		}
		ctx := &fasthttp.RequestCtx{}
		run := func() {
			ctx.Request.Reset()
			ctx.Response.Reset()
			ctx.Request.Header.SetMethod("GET")
			ctx.Request.SetRequestURI("/nope/missing.js")
			h(ctx)
		}
		run()
		if ctx.Response.StatusCode() != 404 {
			t.Fatalf("status = %d, want 404", ctx.Response.StatusCode())
		}
		return testing.AllocsPerRun(50, run)
	}
	without := allocs(nil)
	with := allocs(security.NewPathCache(100))
	if with > without*1.2 {
		t.Errorf("404 allocations with PathCache = %.0f, without = %.0f: the path is validated twice", with, without)
	}
}
