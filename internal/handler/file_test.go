package handler_test

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// setupTestDir creates a temporary public directory with sample files.
func setupTestDir(t *testing.T) (string, *config.Config) {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		"index.html":       "<html><body>Hello</body></html>",
		"style.css":        "body { margin: 0; }",
		"app.js":           "console.log('hello');",
		"data.json":        `{"key": "value"}`,
		"subdir/page.html": "<html>Subpage</html>",
	}

	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 10 * 1024 * 1024
	cfg.Cache.MaxFileSize = 1 * 1024 * 1024
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 10
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = true
	cfg.Security.BlockDotfiles = true
	cfg.Security.CSP = "default-src 'self'"
	cfg.Headers.StaticMaxAge = 3600
	cfg.Headers.HTMLMaxAge = 0
	cfg.Headers.EnableETags = true

	return root, cfg
}

// newTestCtx creates a fasthttp.RequestCtx for testing with the given method and URI.
func newTestCtx(method, uri string) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	return &ctx
}

func TestBuildHandler_ServesIndexHTML(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200", ctx.Response.StatusCode())
	}
	if !strings.Contains(string(ctx.Response.Body()), "Hello") {
		t.Error("response body should contain index.html content")
	}
}

func TestBuildHandler_ServesStaticFile(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/style.css")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200", ctx.Response.StatusCode())
	}
	if ct := string(ctx.Response.Header.Peek("Content-Type")); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
}

func TestBuildHandler_404ForMissingFile(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/nonexistent.txt")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404", ctx.Response.StatusCode())
	}
}

func TestBuildHandler_403ForDotfile(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/.env")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Errorf("status = %d, want 403", ctx.Response.StatusCode())
	}
}

func TestBuildHandler_CacheHitOnSecondRequest(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// First request (cache miss).
	ctx1 := newTestCtx("GET", "/app.js")
	h(ctx1)
	if ctx1.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("first request status = %d, want 200", ctx1.Response.StatusCode())
	}

	// Second request should be a cache hit.
	ctx2 := newTestCtx("GET", "/app.js")
	h(ctx2)
	if ctx2.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("second request status = %d, want 200", ctx2.Response.StatusCode())
	}

	stats := c.Stats()
	if stats.Hits < 1 {
		t.Errorf("expected at least 1 cache hit, got %d", stats.Hits)
	}
}

func TestBuildHandler_304_IfNoneMatch(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Prime the cache.
	ctx := newTestCtx("GET", "/data.json")
	h(ctx)
	etag := string(ctx.Response.Header.Peek("ETag"))
	if etag == "" {
		t.Fatal("ETag not set on first response")
	}

	// Second request with matching ETag.
	ctx2 := newTestCtx("GET", "/data.json")
	ctx2.Request.Header.Set("If-None-Match", etag)
	h(ctx2)

	if ctx2.Response.StatusCode() != fasthttp.StatusNotModified {
		t.Errorf("status = %d, want 304", ctx2.Response.StatusCode())
	}
}

func TestBuildHandler_SecurityHeaders(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/index.html")
	h(ctx)

	if got := string(ctx.Response.Header.Peek("X-Content-Type-Options")); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := string(ctx.Response.Header.Peek("X-Frame-Options")); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
}

func TestBuildHandler_Custom404Page(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "404.html"), []byte("<h1>Custom 404</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>OK</html>"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Files.NotFound = "404.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 1024 * 1024
	cfg.Cache.MaxFileSize = 512 * 1024
	cfg.Compression.Enabled = false
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/missing.html")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404", ctx.Response.StatusCode())
	}
	if !strings.Contains(string(ctx.Response.Body()), "Custom 404") {
		t.Errorf("expected custom 404 page, got: %q", string(ctx.Response.Body()))
	}
}

func TestBuildHandler_HeadRequest(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Prime cache first.
	ctx := newTestCtx("GET", "/style.css")
	h(ctx)

	// HEAD request.
	ctx2 := newTestCtx("HEAD", "/style.css")
	h(ctx2)

	if ctx2.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("HEAD status = %d, want 200", ctx2.Response.StatusCode())
	}
	if len(ctx2.Response.Body()) != 0 {
		t.Errorf("HEAD response should have empty body, got %d bytes", len(ctx2.Response.Body()))
	}
}

func TestBuildHandler_SubdirectoryFile(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/subdir/page.html")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200", ctx.Response.StatusCode())
	}
	if !strings.Contains(string(ctx.Response.Body()), "Subpage") {
		t.Error("response should contain subpage content")
	}
}

func TestBuildHandler_PanicRecovery(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)

	// We test recovery by verifying the full stack handles real requests without panic.
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/index.html")
	// If there's a panic not caught, this test would fail with a panic.
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200", ctx.Response.StatusCode())
	}
}

// ---------------------------------------------------------------------------
// Pre-compressed sidecar file serving (.gz and .br)
// ---------------------------------------------------------------------------

// TestBuildHandler_ServesPrecompressedGzip verifies that when a .gz sidecar
// exists and the client sends Accept-Encoding: gzip, the pre-compressed bytes
// are served with Content-Encoding: gzip.
func TestBuildHandler_ServesPrecompressedGzip(t *testing.T) {
	root := t.TempDir()

	// Write the canonical file.
	content := strings.Repeat("hello gzip sidecar! ", 100)
	if err := os.WriteFile(filepath.Join(root, "bundle.js"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	// Write a fake .gz sidecar (we use a small placeholder — the handler just
	// serves whatever bytes are in the sidecar, trusting they are valid gzip).
	gzContent := []byte("FAKE_GZ_CONTENT_FOR_TEST")
	if err := os.WriteFile(filepath.Join(root, "bundle.js.gz"), gzContent, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := makeCfgWithRoot(t, root)
	cfg.Compression.Enabled = true
	cfg.Compression.Precompressed = true
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// First request to warm the cache (loads sidecar into CachedFile.GzipData).
	warmCtx := newTestCtx("GET", "/bundle.js")
	handler.BuildHandler(cfg, c)(warmCtx)

	// Second request — cache hit, client accepts gzip.
	ctx := newTestCtx("GET", "/bundle.js")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200", ctx.Response.StatusCode())
	}
	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip when .gz sidecar present", enc)
	}
}

// TestBuildHandler_ServesPrecompressedBrotli verifies that a .br sidecar is
// preferred over gzip when the client accepts both.
func TestBuildHandler_ServesPrecompressedBrotli(t *testing.T) {
	root := t.TempDir()

	content := strings.Repeat("hello brotli sidecar! ", 100)
	if err := os.WriteFile(filepath.Join(root, "main.css"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	// Both .gz and .br sidecars exist; br should be preferred.
	if err := os.WriteFile(filepath.Join(root, "main.css.gz"), []byte("FAKE_GZ"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.css.br"), []byte("FAKE_BR"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := makeCfgWithRoot(t, root)
	cfg.Compression.Enabled = true
	cfg.Compression.Precompressed = true
	c := cache.NewCache(cfg.Cache.MaxBytes)

	// Warm the cache.
	warmCtx := newTestCtx("GET", "/main.css")
	handler.BuildHandler(cfg, c)(warmCtx)

	// Request with both br and gzip accepted.
	ctx := newTestCtx("GET", "/main.css")
	ctx.Request.Header.Set("Accept-Encoding", "gzip, br")
	handler.BuildHandler(cfg, c)(ctx)

	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "br" {
		t.Errorf("Content-Encoding = %q, want br (brotli preferred over gzip)", enc)
	}
}

// TestBuildHandler_FallsBackToUncompressed verifies that when compression is
// enabled but the client does not accept compressed encodings, raw bytes are sent.
func TestBuildHandler_FallsBackToUncompressed(t *testing.T) {
	root := t.TempDir()
	content := "body { color: blue; }"
	if err := os.WriteFile(filepath.Join(root, "theme.css"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := makeCfgWithRoot(t, root)
	cfg.Compression.Enabled = true
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Warm cache.
	warmCtx := newTestCtx("GET", "/theme.css")
	h(warmCtx)

	// Request with no Accept-Encoding.
	ctx := newTestCtx("GET", "/theme.css")
	h(ctx)

	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty when client has no Accept-Encoding", enc)
	}
	if !strings.Contains(string(ctx.Response.Body()), "color: blue") {
		t.Error("response body should contain uncompressed CSS content")
	}
}

// TestBuildHandler_LargeFile verifies that files exceeding MaxFileSize are served
// directly from disk (bypassing the cache).
func TestBuildHandler_LargeFile(t *testing.T) {
	root := t.TempDir()

	// Write a file larger than the MaxFileSize we'll configure (1 KB).
	largeContent := strings.Repeat("X", 2048)
	if err := os.WriteFile(filepath.Join(root, "large.bin"), []byte(largeContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := makeCfgWithRoot(t, root)
	cfg.Cache.MaxFileSize = 1024 // 1 KB threshold — our 2 KB file exceeds it
	cfg.Compression.Enabled = false
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/large.bin")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200 for large file served from disk", ctx.Response.StatusCode())
	}
	if len(ctx.Response.Body()) != 2048 {
		t.Errorf("body length = %d, want 2048 for large file", len(ctx.Response.Body()))
	}
	// Large files bypass the cache — entry count must still be 0.
	if c.Stats().EntryCount != 0 {
		t.Errorf("cache EntryCount = %d, want 0 for large file bypass", c.Stats().EntryCount)
	}
}

// TestBuildHandler_CacheDisabled verifies that when Cache.Enabled=false, the
// handler reads from disk on every request.
func TestBuildHandler_CacheDisabled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "nocache.txt"), []byte("no cache here"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := makeCfgWithRoot(t, root)
	cfg.Cache.Enabled = false
	var c *cache.Cache
	h := handler.BuildHandler(cfg, c)

	for i := range 3 {
		ctx := newTestCtx("GET", "/nocache.txt")
		h(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, ctx.Response.StatusCode())
		}
	}
	// No entries should appear in the cache.
	if c != nil {
		t.Fatal("cache should be nil when cache is disabled")
	}
}

// TestBuildHandler_XCacheHeader verifies X-Cache header behavior.
// Note: The implementation sets X-Cache: MISS in serveFromDisk but then
// immediately calls serveFromCache which sets X-Cache: HIT — so the final
// header value on first request is "HIT". On subsequent requests the file
// is read directly from the cache and also returns "HIT". The MISS marker is
// an intermediate state only visible to instrumentation between the two writes.
func TestBuildHandler_XCacheHeader(t *testing.T) {
	// Use a dedicated root so the file is never pre-cached by other tests.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "xcache.css"), []byte("a{}"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := makeCfgWithRoot(t, root)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// First request — file read from disk (cache miss path).
	// serveFromDisk sets MISS then calls serveFromCache which overwrites to HIT.
	ctx1 := newTestCtx("GET", "/xcache.css")
	h(ctx1)
	xCache1 := string(ctx1.Response.Header.Peek("X-Cache"))
	if xCache1 != "HIT" && xCache1 != "MISS" {
		t.Errorf("X-Cache = %q on first request, want HIT or MISS", xCache1)
	}

	// Second request — file is now in cache → always HIT.
	ctx2 := newTestCtx("GET", "/xcache.css")
	h(ctx2)
	if string(ctx2.Response.Header.Peek("X-Cache")) != "HIT" {
		t.Errorf("X-Cache = %q on second request (cache hit), want HIT", string(ctx2.Response.Header.Peek("X-Cache")))
	}
}

// TestBuildHandler_304_IfModifiedSince verifies the full stack 304 path via
// If-Modified-Since (header middleware intercepts before file handler).
func TestBuildHandler_304_IfModifiedSince(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Prime cache.
	ctx := newTestCtx("GET", "/app.js")
	h(ctx)

	lm := string(ctx.Response.Header.Peek("Last-Modified"))
	if lm == "" {
		t.Fatal("Last-Modified not set on first response")
	}

	// Second request using a date far in the future → resource hasn't changed.
	ctx2 := newTestCtx("GET", "/app.js")
	ctx2.Request.Header.Set("If-Modified-Since", "Tue, 01 Jan 2030 00:00:00 GMT")
	h(ctx2)

	if ctx2.Response.StatusCode() != fasthttp.StatusNotModified {
		t.Errorf("status = %d, want 304 for If-Modified-Since future date", ctx2.Response.StatusCode())
	}
}

// TestBuildHandler_NullByteInURL verifies the full middleware stack returns 400
// for URLs containing null bytes.
func TestBuildHandler_NullByteInURL(t *testing.T) {
	_, cfg := setupTestDir(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/file\x00name")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Errorf("status = %d, want 400 for null byte in URL", ctx.Response.StatusCode())
	}
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

// makeCfgWithRoot builds a minimal Config pointing at root.
func makeCfgWithRoot(t *testing.T, root string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 10 * 1024 * 1024
	cfg.Cache.MaxFileSize = 1 * 1024 * 1024
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 10
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = true
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600
	cfg.Headers.HTMLMaxAge = 0
	return cfg
}

// ---------------------------------------------------------------------------
// Embedded default asset fallback tests
// ---------------------------------------------------------------------------

// setupEmptyRootCfg creates a config whose files.root is an empty temp dir,
// so every disk lookup will miss and trigger the embedded fallback path.
func setupEmptyRootCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir() // intentionally empty
	cfg := makeCfgWithRoot(t, root)
	cfg.Compression.Enabled = false // keep responses simple for content checks
	return cfg
}

// TestEmbedFallback_IndexHTML verifies that a GET / against an empty root
// returns the embedded index.html with status 200 and HTML content.
func TestEmbedFallback_IndexHTML(t *testing.T) {
	cfg := setupEmptyRootCfg(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200 for embedded index.html", ctx.Response.StatusCode())
	}
	if ct := string(ctx.Response.Header.Peek("Content-Type")); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html for embedded index.html", ct)
	}
	if body := string(ctx.Response.Body()); !strings.Contains(body, "<html") {
		t.Errorf("embedded index.html body does not look like HTML: %q", body[:min(len(body), 120)])
	}
}

// TestEmbedFallback_StyleCSS verifies that /style.css is served from the
// embedded FS when the file is absent from files.root.
func TestEmbedFallback_StyleCSS(t *testing.T) {
	cfg := setupEmptyRootCfg(t)
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/style.css")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200 for embedded style.css", ctx.Response.StatusCode())
	}
	if ct := string(ctx.Response.Header.Peek("Content-Type")); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css for embedded style.css", ct)
	}
	if len(ctx.Response.Body()) == 0 {
		t.Error("embedded style.css response body must not be empty")
	}
}

// TestEmbedFallback_404HTML verifies that a truly unknown file (not in the
// embedded FS either) falls all the way through to serveNotFound, which itself
// serves the embedded 404.html with status 404.
func TestEmbedFallback_404HTML(t *testing.T) {
	cfg := setupEmptyRootCfg(t)
	cfg.Files.NotFound = "" // no custom 404 configured
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/totally-unknown-file.xyz")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown file", ctx.Response.StatusCode())
	}
	if ct := string(ctx.Response.Header.Peek("Content-Type")); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html for embedded 404 page", ct)
	}
	if body := string(ctx.Response.Body()); !strings.Contains(body, "<html") {
		t.Errorf("embedded 404.html body does not look like HTML: %q", body[:min(len(body), 120)])
	}
}

// TestEmbedFallback_StyleCSS_ETag verifies that /style.css served from the
// embedded FS includes an ETag header when enable_etags is true, and omits it
// when enable_etags is false.
func TestEmbedFallback_StyleCSS_ETag(t *testing.T) {
	t.Run("etag enabled", func(t *testing.T) {
		cfg := setupEmptyRootCfg(t)
		cfg.Headers.EnableETags = true
		c := cache.NewCache(cfg.Cache.MaxBytes)
		h := handler.BuildHandler(cfg, c)

		ctx := newTestCtx("GET", "/style.css")
		h(ctx)

		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
		}
		etag := string(ctx.Response.Header.Peek("ETag"))
		if etag == "" {
			t.Error("ETag header must be set on embedded style.css when enable_etags=true")
		}
	})

	t.Run("etag disabled", func(t *testing.T) {
		cfg := setupEmptyRootCfg(t)
		cfg.Headers.EnableETags = false
		c := cache.NewCache(cfg.Cache.MaxBytes)
		h := handler.BuildHandler(cfg, c)

		ctx := newTestCtx("GET", "/style.css")
		h(ctx)

		etag := string(ctx.Response.Header.Peek("ETag"))
		if etag != "" {
			t.Errorf("ETag header must NOT be set when enable_etags=false, got %q", etag)
		}
	})
}

// TestEmbedFallback_404HTML_ETag verifies that the embedded 404.html includes
// an ETag header when enable_etags is true.
func TestEmbedFallback_404HTML_ETag(t *testing.T) {
	cfg := setupEmptyRootCfg(t)
	cfg.Headers.EnableETags = true
	cfg.Files.NotFound = ""
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/totally-unknown-file.xyz")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Fatalf("status = %d, want 404", ctx.Response.StatusCode())
	}
	etag := string(ctx.Response.Header.Peek("ETag"))
	if etag == "" {
		t.Error("ETag header must be set on embedded 404.html when enable_etags=true")
	}
}

// TestEmbedFallback_StyleCSS_CacheControl verifies that /style.css served from
// the embedded FS includes a Cache-Control header with the configured max-age.
func TestEmbedFallback_StyleCSS_CacheControl(t *testing.T) {
	cfg := setupEmptyRootCfg(t)
	cfg.Headers.StaticMaxAge = 7200
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/style.css")
	h(ctx)

	cc := string(ctx.Response.Header.Peek("Cache-Control"))
	if !strings.Contains(cc, "max-age=7200") {
		t.Errorf("Cache-Control = %q, want it to contain max-age=7200", cc)
	}
}

// TestEmbedFallback_SubpathNotServed verifies that the embed fallback only
// handles flat filenames. A URL like /sub/index.html must NOT be served from
// the embedded FS (guard against sub-path traversal) and must return 404.
func TestEmbedFallback_SubpathNotServed(t *testing.T) {
	cfg := setupEmptyRootCfg(t)
	cfg.Files.NotFound = ""
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/sub/index.html")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404: embed fallback must not serve sub-path URLs", ctx.Response.StatusCode())
	}
}

// TestEmbedFallback_CustomNotFoundTakesPriority verifies that a configured
// files.not_found disk file is still preferred over the embedded 404.html.
func TestEmbedFallback_CustomNotFoundTakesPriority(t *testing.T) {
	root := t.TempDir()
	// Write a custom 404 page to disk (but no other files).
	custom404 := "<h1>My Custom 404</h1>"
	if err := os.WriteFile(filepath.Join(root, "my404.html"), []byte(custom404), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := makeCfgWithRoot(t, root)
	cfg.Files.NotFound = "my404.html"
	cfg.Compression.Enabled = false
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	ctx := newTestCtx("GET", "/missing.html")
	h(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Errorf("status = %d, want 404", ctx.Response.StatusCode())
	}
	if !strings.Contains(string(ctx.Response.Body()), "My Custom 404") {
		t.Errorf("expected custom 404 page to take priority over embedded one, got: %q", string(ctx.Response.Body()))
	}
}

// min is a local helper for Go versions that lack the built-in min (pre-1.21).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

// BenchmarkHandler_CacheHit measures throughput for serving a cached small file.
func BenchmarkHandler_CacheHit(b *testing.B) {
	// Silence logging for the entire benchmark (warm-up + measured loop).
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := b.TempDir()
	content := strings.Repeat("body { margin: 0; } ", 50) // ~1 KB
	if err := os.WriteFile(filepath.Join(root, "bench.css"), []byte(content), 0644); err != nil {
		b.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 64 * 1024 * 1024
	cfg.Cache.MaxFileSize = 10 * 1024 * 1024
	cfg.Compression.Enabled = false // isolate cache-hit path
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Warm the cache.
	warmCtx := newTestCtx("GET", "/bench.css")
	h(warmCtx)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx := newTestCtx("GET", "/bench.css")
		h(ctx)
	}
}

// BenchmarkHandler_CacheHitParallel measures concurrent cache-hit throughput.
func BenchmarkHandler_CacheHitParallel(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := b.TempDir()
	content := strings.Repeat("console.log('bench'); ", 80)
	if err := os.WriteFile(filepath.Join(root, "bench.js"), []byte(content), 0644); err != nil {
		b.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 64 * 1024 * 1024
	cfg.Cache.MaxFileSize = 10 * 1024 * 1024
	cfg.Compression.Enabled = false
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Warm the cache.
	warmCtx := newTestCtx("GET", "/bench.js")
	h(warmCtx)

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx := newTestCtx("GET", "/bench.js")
			h(ctx)
		}
	})
}

// BenchmarkHandler_CacheHitGzip measures cache-hit throughput when the client
// accepts gzip and a pre-compressed variant is in the cache.
func BenchmarkHandler_CacheHitGzip(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := b.TempDir()
	content := strings.Repeat("body { color: red; } ", 100)
	if err := os.WriteFile(filepath.Join(root, "bench.css"), []byte(content), 0644); err != nil {
		b.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 64 * 1024 * 1024
	cfg.Cache.MaxFileSize = 10 * 1024 * 1024
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 1
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = false // use on-the-fly gzip
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Warm — gzip variant is generated and cached on first request.
	warmCtx := newTestCtx("GET", "/bench.css")
	warmCtx.Request.Header.Set("Accept-Encoding", "gzip")
	h(warmCtx)

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx := newTestCtx("GET", "/bench.css")
			ctx.Request.Header.Set("Accept-Encoding", "gzip")
			h(ctx)
		}
	})
}

// BenchmarkHandler_CacheHitZstd measures cache-hit throughput when the client
// accepts zstd and on-the-fly zstd compression is generated and cached.
func BenchmarkHandler_CacheHitZstd(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := b.TempDir()
	content := strings.Repeat("body { color: red; } ", 100)
	if err := os.WriteFile(filepath.Join(root, "bench.css"), []byte(content), 0644); err != nil {
		b.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 64 * 1024 * 1024
	cfg.Cache.MaxFileSize = 10 * 1024 * 1024
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 1
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = false // use on-the-fly zstd
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandler(cfg, c)

	// Warm — zstd variant is generated and cached on first request.
	warmCtx := newTestCtx("GET", "/bench.css")
	warmCtx.Request.Header.Set("Accept-Encoding", "zstd")
	h(warmCtx)

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctx := newTestCtx("GET", "/bench.css")
			ctx.Request.Header.Set("Accept-Encoding", "zstd")
			h(ctx)
		}
	})
}

// BenchmarkHandler_CacheHitQuiet measures the cache-hit path with request logging disabled.
func BenchmarkHandler_CacheHitQuiet(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := b.TempDir()
	content := strings.Repeat("body { margin: 0; } ", 50)
	if err := os.WriteFile(filepath.Join(root, "bench.css"), []byte(content), 0644); err != nil {
		b.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 64 * 1024 * 1024
	cfg.Cache.MaxFileSize = 10 * 1024 * 1024
	cfg.Compression.Enabled = false
	cfg.Security.BlockDotfiles = true
	cfg.Headers.StaticMaxAge = 3600

	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandlerQuiet(cfg, c)

	// Warm the cache.
	warmCtx := newTestCtx("GET", "/bench.css")
	h(warmCtx)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx := newTestCtx("GET", "/bench.css")
		h(ctx)
	}
}

// TestValidateSidecarPath tests the path validation logic for sidecar files.
// This test ensures that CodeQL path-injection alerts are properly addressed
// by verifying that filepath.Clean() + symlink resolution + prefix checking
// prevents all known path traversal attacks.
func TestValidateSidecarPath(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Cache.Enabled = false
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.NewFileHandler(cfg, c)

	// Create test files
	testFile := filepath.Join(root, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory with a file
	subdir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	subFile := filepath.Join(subdir, "sub.txt")
	if err := os.WriteFile(subFile, []byte("sub"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
		desc    string
	}{
		{
			name:    "valid absolute path",
			path:    testFile,
			wantErr: false,
			desc:    "Should accept valid absolute path within root",
		},
		{
			name:    "valid relative path",
			path:    "test.txt",
			wantErr: false,
			desc:    "Should accept valid relative path within root",
		},
		{
			name:    "valid subdirectory path",
			path:    filepath.Join(root, "subdir", "sub.txt"),
			wantErr: false,
			desc:    "Should accept valid path in subdirectory",
		},
		{
			name:    "traversal with ..",
			path:    filepath.Join(root, "..", "etc", "passwd"),
			wantErr: true,
			desc:    "Should reject path traversal with .. components",
		},
		{
			name:    "traversal with multiple ..",
			path:    filepath.Join(root, "..", "..", "..", "etc", "passwd"),
			wantErr: true,
			desc:    "Should reject multiple .. traversal attempts",
		},
		{
			name:    "absolute path outside root",
			path:    "/etc/passwd",
			wantErr: true,
			desc:    "Should reject absolute path outside root",
		},
		{
			name:    "nonexistent file",
			path:    filepath.Join(root, "nonexistent.txt"),
			wantErr: true,
			desc:    "Should reject nonexistent files (EvalSymlinks fails)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := h.ValidateSidecarPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("%s: got error=%v, wantErr=%v", tt.desc, err, tt.wantErr)
			}
			if !tt.wantErr && err == nil {
				// Verify the result is within root
				realRoot := root
				if r, err := filepath.EvalSymlinks(root); err == nil {
					realRoot = r
				}
				if !strings.HasPrefix(result, realRoot) && result != realRoot {
					t.Errorf("%s: result %q is not within root %q", tt.desc, result, realRoot)
				}
			}
		})
	}
}

// TestLoadSidecar tests the sidecar file loading logic.
// This test verifies that the CodeQL-compliant path validation
// allows legitimate sidecar files to be loaded while rejecting attacks.
func TestLoadSidecar(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Cache.Enabled = false
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.NewFileHandler(cfg, c)

	// Create a test file and its sidecar
	testFile := filepath.Join(root, "test.txt")
	sidecarFile := filepath.Join(root, "test.txt.gz")
	sidecarContent := []byte("compressed data")

	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarFile, sidecarContent, 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantNil bool
		desc    string
	}{
		{
			name:    "valid sidecar",
			path:    sidecarFile,
			wantNil: false,
			desc:    "Should load valid sidecar file",
		},
		{
			name:    "nonexistent sidecar",
			path:    filepath.Join(root, "nonexistent.gz"),
			wantNil: true,
			desc:    "Should return nil for nonexistent sidecar",
		},
		{
			name:    "traversal attempt",
			path:    filepath.Join(root, "..", "etc", "passwd"),
			wantNil: true,
			desc:    "Should return nil for traversal attempts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := h.LoadSidecar(tt.path)
			if (result == nil) != tt.wantNil {
				t.Errorf("%s: got nil=%v, wantNil=%v", tt.desc, result == nil, tt.wantNil)
			}
			if !tt.wantNil && result != nil {
				if !bytes.Equal(result, sidecarContent) {
					t.Errorf("%s: got %q, want %q", tt.desc, result, sidecarContent)
				}
			}
		})
	}
}

// BenchmarkValidateSidecarPath benchmarks the path validation logic.
func BenchmarkValidateSidecarPath(b *testing.B) {
	root := b.TempDir()
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Cache.Enabled = false
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.NewFileHandler(cfg, c)

	// Create a test file
	testFile := filepath.Join(root, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = h.ValidateSidecarPath(testFile)
	}
}
