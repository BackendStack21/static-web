package headers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/headers"
)

func makeCache(path string, data []byte, ct string) *cache.Cache {
	c := cache.NewCache(10 * 1024 * 1024)
	f := &cache.CachedFile{
		Data:         data,
		ETag:         "abcdef1234567890",
		LastModified: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		ContentType:  ct,
		Size:         int64(len(data)),
	}
	c.Put(path, f)
	return c
}

func TestMiddleware_SetsETagHeader(t *testing.T) {
	c := makeCache("/style.css", []byte("body{}"), "text/css")
	cfg := &config.HeadersConfig{StaticMaxAge: 3600, HTMLMaxAge: 0}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Error("ETag header should be set")
	}
	if etag != `W/"abcdef1234567890"` {
		t.Errorf("ETag = %q, want W/\"abcdef1234567890\"", etag)
	}
}

func TestMiddleware_304_IfNoneMatch(t *testing.T) {
	c := makeCache("/app.js", []byte("console.log(1)"), "application/javascript")
	cfg := &config.HeadersConfig{StaticMaxAge: 3600}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	req.Header.Set("If-None-Match", `W/"abcdef1234567890"`)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rr.Code)
	}
}

func TestMiddleware_304_IfModifiedSince(t *testing.T) {
	c := makeCache("/page.html", []byte("<html>"), "text/html")
	cfg := &config.HeadersConfig{HTMLMaxAge: 0}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/page.html", nil)
	// Send a time after LastModified to trigger 304.
	req.Header.Set("If-Modified-Since", time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rr.Code)
	}
}

func TestMiddleware_200_ETagMismatch(t *testing.T) {
	c := makeCache("/data.json", []byte(`{}`), "application/json")
	cfg := &config.HeadersConfig{StaticMaxAge: 3600}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/data.json", nil)
	req.Header.Set("If-None-Match", `W/"differentetag0000"`)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 on ETag mismatch", rr.Code)
	}
}

func TestMiddleware_CacheControlHTML(t *testing.T) {
	c := makeCache("/index.html", []byte("<html>"), "text/html")
	cfg := &config.HeadersConfig{HTMLMaxAge: 0, StaticMaxAge: 3600}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	cc := rr.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache for HTML with MaxAge=0", cc)
	}
}

func TestMiddleware_CacheControlStatic(t *testing.T) {
	c := makeCache("/logo.png", []byte("PNG"), "image/png")
	cfg := &config.HeadersConfig{StaticMaxAge: 86400}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/logo.png", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	cc := rr.Header().Get("Cache-Control")
	if cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want public, max-age=86400", cc)
	}
}

func TestMiddleware_VaryHeader(t *testing.T) {
	c := makeCache("/main.css", []byte("h1{}"), "text/css")
	cfg := &config.HeadersConfig{StaticMaxAge: 3600}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/main.css", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if vary := rr.Header().Get("Vary"); vary == "" {
		t.Error("Vary header should be set")
	}
}

func TestMiddleware_PassthroughOnCacheMiss(t *testing.T) {
	c := cache.NewCache(1024)
	cfg := &config.HeadersConfig{StaticMaxAge: 3600}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/notcached.txt", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next should be called on cache miss")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestMiddleware_WildcardIfNoneMatch(t *testing.T) {
	c := makeCache("/wild.js", []byte("x=1"), "application/javascript")
	cfg := &config.HeadersConfig{StaticMaxAge: 3600}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/wild.js", nil)
	req.Header.Set("If-None-Match", "*")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304 for If-None-Match: *", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Additional headers coverage
// ---------------------------------------------------------------------------

// TestMiddleware_PostMethodPassthrough verifies non-GET/HEAD methods bypass the
// conditional-request logic entirely.
func TestMiddleware_PostMethodPassthrough(t *testing.T) {
	c := makeCache("/api.json", []byte(`{}`), "application/json")
	cfg := &config.HeadersConfig{StaticMaxAge: 3600}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodPost, "/api.json", nil)
	req.Header.Set("If-None-Match", `W/"abcdef1234567890"`)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next should be called for non-GET/HEAD methods")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for POST passthrough", rr.Code)
	}
}

// TestMiddleware_RootPathResolvesToIndexHTML verifies that "/" is mapped to
// "/index.html" for cache look-up purposes.
func TestMiddleware_RootPathResolvesToIndexHTML(t *testing.T) {
	c := cache.NewCache(10 * 1024 * 1024)
	indexFile := &cache.CachedFile{
		Data:         []byte("<html>Index</html>"),
		ETag:         "indexetag1234567",
		LastModified: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		ContentType:  "text/html",
		Size:         18,
	}
	// Populate cache under the canonical "/index.html" key.
	c.Put("/index.html", indexFile)

	cfg := &config.HeadersConfig{HTMLMaxAge: 0, StaticMaxAge: 3600}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// If the middleware correctly maps "/" → "/index.html", the ETag should be set.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if etag := rr.Header().Get("ETag"); etag == "" {
		t.Error("ETag should be set when / maps to cached /index.html")
	}
}

// TestMiddleware_ImmutablePattern verifies that a matching glob pattern adds the
// "immutable" directive to Cache-Control.
func TestMiddleware_ImmutablePattern(t *testing.T) {
	c := makeCache("/assets/app.abc123.js", []byte("console.log(1)"), "application/javascript")
	cfg := &config.HeadersConfig{
		StaticMaxAge:     31536000,
		ImmutablePattern: "*.js",
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/assets/app.abc123.js", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want it to contain 'immutable' for matched pattern", cc)
	}
	if !strings.Contains(cc, "public") {
		t.Errorf("Cache-Control = %q, want it to contain 'public'", cc)
	}
}

// TestMiddleware_ImmutablePatternNoMatch verifies that a non-matching glob does NOT
// add the "immutable" directive.
func TestMiddleware_ImmutablePatternNoMatch(t *testing.T) {
	c := makeCache("/assets/image.png", []byte("PNG"), "image/png")
	cfg := &config.HeadersConfig{
		StaticMaxAge:     3600,
		ImmutablePattern: "*.js", // pattern targets .js only
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/assets/image.png", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	cc := rr.Header().Get("Cache-Control")
	if strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, must NOT contain 'immutable' for non-matching pattern", cc)
	}
}

// TestSetFileHeaders verifies that SetFileHeaders (called from the file handler)
// sets the same caching headers as the middleware path.
func TestSetFileHeaders(t *testing.T) {
	f := &cache.CachedFile{
		Data:         []byte("body { color: red }"),
		ETag:         "deadbeef01234567",
		LastModified: time.Date(2024, 3, 20, 12, 0, 0, 0, time.UTC),
		ContentType:  "text/css",
		Size:         19,
	}
	cfg := &config.HeadersConfig{StaticMaxAge: 7200}

	rr := httptest.NewRecorder()
	headers.SetFileHeaders(rr, "/theme.css", f, cfg)

	if etag := rr.Header().Get("ETag"); etag != `W/"deadbeef01234567"` {
		t.Errorf("ETag = %q, want W/\"deadbeef01234567\"", etag)
	}
	if lm := rr.Header().Get("Last-Modified"); lm == "" {
		t.Error("Last-Modified should be set by SetFileHeaders")
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=7200" {
		t.Errorf("Cache-Control = %q, want public, max-age=7200", cc)
	}
}

// TestMiddleware_IfModifiedSince_ResourceNewer verifies 200 is returned when the
// resource has been modified after the If-Modified-Since date.
func TestMiddleware_IfModifiedSince_ResourceNewer(t *testing.T) {
	c := makeCache("/newer.html", []byte("<html>fresh</html>"), "text/html")
	cfg := &config.HeadersConfig{HTMLMaxAge: 0}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/newer.html", nil)
	// Cached LastModified is 2024-01-15; send an IMS of 2024-01-10 (older).
	req.Header.Set("If-Modified-Since", time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when resource is newer than IMS", rr.Code)
	}
}

// TestMiddleware_InvalidIfModifiedSince verifies that an unparseable IMS header
// is ignored and the resource is served normally.
func TestMiddleware_InvalidIfModifiedSince(t *testing.T) {
	c := makeCache("/page.html", []byte("<html>"), "text/html")
	cfg := &config.HeadersConfig{HTMLMaxAge: 0}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/page.html", nil)
	req.Header.Set("If-Modified-Since", "not-a-valid-date")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for invalid IMS date", rr.Code)
	}
}

// TestMiddleware_HTMLExtension verifies .html files get HTMLMaxAge Cache-Control.
func TestMiddleware_HTMLExtension(t *testing.T) {
	c := makeCache("/about.html", []byte("<html>About</html>"), "text/html")
	cfg := &config.HeadersConfig{HTMLMaxAge: 300, StaticMaxAge: 86400}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := headers.Middleware(c, cfg, "index.html", next)
	req := httptest.NewRequest(http.MethodGet, "/about.html", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	cc := rr.Header().Get("Cache-Control")
	if cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want public, max-age=300 for HTML with HTMLMaxAge=300", cc)
	}
}
