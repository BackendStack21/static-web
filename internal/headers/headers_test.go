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

func makeCachedFile(data []byte, ct string) *cache.CachedFile {
	return &cache.CachedFile{
		Data:         data,
		ETag:         "abcdef1234567890",
		ETagFull:     `W/"abcdef1234567890"`,
		LastModified: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		ContentType:  ct,
		Size:         int64(len(data)),
	}
}

func TestCacheKeyForPath(t *testing.T) {
	tests := []struct {
		name      string
		urlPath   string
		indexFile string
		want      string
	}{
		{name: "root", urlPath: "/", indexFile: "index.html", want: "/index.html"},
		{name: "directory", urlPath: "/docs/", indexFile: "home.html", want: "/docs/home.html"},
		{name: "file", urlPath: "/app.js", indexFile: "index.html", want: "/app.js"},
		{name: "default index", urlPath: "/", indexFile: "", want: "/index.html"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := headers.CacheKeyForPath(tt.urlPath, tt.indexFile); got != tt.want {
				t.Fatalf("CacheKeyForPath(%q, %q) = %q, want %q", tt.urlPath, tt.indexFile, got, tt.want)
			}
		})
	}
}

func TestCheckNotModifiedIfNoneMatch(t *testing.T) {
	f := makeCachedFile([]byte("console.log(1)"), "application/javascript")
	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	req.Header.Set("If-None-Match", `W/"abcdef1234567890"`)
	rr := httptest.NewRecorder()

	if !headers.CheckNotModified(rr, req, f) {
		t.Fatal("CheckNotModified returned false, want true")
	}
	if rr.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rr.Code)
	}
}

func TestCheckNotModifiedIfModifiedSince(t *testing.T) {
	f := makeCachedFile([]byte("<html>"), "text/html")
	req := httptest.NewRequest(http.MethodGet, "/page.html", nil)
	req.Header.Set("If-Modified-Since", time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	rr := httptest.NewRecorder()

	if !headers.CheckNotModified(rr, req, f) {
		t.Fatal("CheckNotModified returned false, want true")
	}
	if rr.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rr.Code)
	}
}

func TestCheckNotModifiedReturnsFalseOnMismatch(t *testing.T) {
	f := makeCachedFile([]byte(`{}`), "application/json")
	req := httptest.NewRequest(http.MethodGet, "/data.json", nil)
	req.Header.Set("If-None-Match", `W/"differentetag0000"`)
	rr := httptest.NewRecorder()

	if headers.CheckNotModified(rr, req, f) {
		t.Fatal("CheckNotModified returned true, want false")
	}
}

func TestSetCacheHeadersHTML(t *testing.T) {
	f := makeCachedFile([]byte("<html>"), "text/html")
	cfg := &config.HeadersConfig{HTMLMaxAge: 0, StaticMaxAge: 3600}
	rr := httptest.NewRecorder()

	headers.SetCacheHeaders(rr, "/index.html", f, cfg)

	if etag := rr.Header().Get("ETag"); etag != `W/"abcdef1234567890"` {
		t.Fatalf("ETag = %q, want W/\"abcdef1234567890\"", etag)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	if vary := rr.Header().Get("Vary"); vary == "" {
		t.Fatal("Vary header should be set")
	}
}

func TestSetCacheHeadersStaticImmutable(t *testing.T) {
	f := makeCachedFile([]byte("console.log(1)"), "application/javascript")
	cfg := &config.HeadersConfig{StaticMaxAge: 31536000, ImmutablePattern: "*.js"}
	rr := httptest.NewRecorder()

	headers.SetCacheHeaders(rr, "/assets/app.abc123.js", f, cfg)

	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "public") || !strings.Contains(cc, "immutable") {
		t.Fatalf("Cache-Control = %q, want public + immutable", cc)
	}
}

func TestETagMatches(t *testing.T) {
	if !headers.ETagMatches("*", `W/"abc"`) {
		t.Fatal("ETagMatches wildcard = false, want true")
	}
	if !headers.ETagMatches(`W/"abc", W/"def"`, `W/"def"`) {
		t.Fatal("ETagMatches list = false, want true")
	}
	if headers.ETagMatches(`W/"abc"`, `W/"def"`) {
		t.Fatal("ETagMatches mismatch = true, want false")
	}
}
