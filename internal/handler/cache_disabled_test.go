package handler_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// cdSetup writes a compressible script, plus the given sidecars, into a
// fresh root and returns a handler built with the file cache disabled.
func cdSetup(t *testing.T, sidecars map[string][]byte) (fasthttp.RequestHandler, []byte) {
	t.Helper()
	root, cfg := setupTestDir(t)
	cfg.Cache.Enabled = false
	src := rbJS(16 << 10)
	if err := os.WriteFile(filepath.Join(root, "app2.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	for ext, data := range sidecars {
		if err := os.WriteFile(filepath.Join(root, "app2.js"+ext), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return handler.BuildHandlerQuiet(cfg, nil), src
}

// P-F4: with the file cache disabled every request read the file and then
// built gzip and zstd variants that were thrown away after one use,
// whatever the client accepted and for HEAD too. Only the sidecar the client
// is served may be loaded; otherwise the compress middleware encodes the
// response on demand, which it only does for gzip.
func TestCacheDisabled_ServesSidecarOrLeavesEncodingToMiddleware(t *testing.T) {
	br := bytes.Repeat([]byte("BR"), 100)
	zst := bytes.Repeat([]byte("ZS"), 100)
	gz := bytes.Repeat([]byte("GZ"), 100)

	for _, tc := range []struct {
		name     string
		sidecars map[string][]byte
		ae       string
		wantCE   string
		wantBody []byte // nil: the source, gzipped by the middleware when wantCE is gzip
	}{
		{"zstd only, no sidecar: identity", nil, "zstd", "", nil},
		{"br only, no sidecar: identity", nil, "br", "", nil},
		{"gzip and zstd, no sidecar: middleware gzip", nil, "gzip, zstd", "gzip", nil},
		{"br sidecar", map[string][]byte{".br": br, ".zst": zst, ".gz": gz}, "gzip, br, zstd", "br", br},
		{"zst sidecar", map[string][]byte{".zst": zst, ".gz": gz}, "gzip, br, zstd", "zstd", zst},
		{"gz sidecar", map[string][]byte{".gz": gz}, "gzip, br, zstd", "gzip", gz},
		{"br sidecar not accepted", map[string][]byte{".br": br}, "zstd", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, src := cdSetup(t, tc.sidecars)
			get := irDo(h, "GET", "/app2.js", "Accept-Encoding", tc.ae)
			if get.Response.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("GET status = %d", get.Response.StatusCode())
			}
			if got := string(get.Response.Header.Peek("Content-Encoding")); got != tc.wantCE {
				t.Errorf("GET Content-Encoding = %q, want %q", got, tc.wantCE)
			}
			switch {
			case tc.wantBody != nil:
				if !bytes.Equal(get.Response.Body(), tc.wantBody) {
					t.Errorf("GET body is not the %s sidecar", tc.wantCE)
				}
			default:
				if got := decodeBody(t, get); got != string(src) {
					t.Errorf("GET body does not decode to the source (len %d)", len(got))
				}
			}

			head := irDo(h, "HEAD", "/app2.js", "Accept-Encoding", tc.ae)
			if got := string(head.Response.Header.Peek("Content-Encoding")); got != tc.wantCE {
				t.Errorf("HEAD Content-Encoding = %q, want %q (as GET)", got, tc.wantCE)
			}
			if got, want := irHeadContentLength(t, head), len(get.Response.Body()); got != want {
				t.Errorf("HEAD Content-Length = %d, want %d (GET body length)", got, want)
			}
		})
	}
}

// The uncached HEAD carries the identity body only when the compress
// middleware may encode it, so that HEAD and GET agree. Otherwise copying
// the whole file into the response for a body that is never sent is waste:
// e.g. a cache-off HEAD of a large video, or of anything without
// Accept-Encoding: gzip. Such a HEAD keeps the explicit Content-Length.
func TestCacheDisabled_HeadCopiesBodyOnlyWhenMiddlewareMayEncode(t *testing.T) {
	root, cfg := setupTestDir(t)
	cfg.Cache.Enabled = false
	cfg.Compression.MaxCompressSize = 32 << 10
	files := map[string][]byte{
		"clip.bin": make([]byte, 64<<10), // not compressible
		"app2.js":  rbJS(16 << 10),       // compressible
		"huge.js":  rbJS(64 << 10),       // compressible, above max_compress_size
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := handler.BuildHandlerQuiet(cfg, nil)

	for _, tc := range []struct {
		uri, ae string
		encoded bool
	}{
		{"/clip.bin", "gzip", false},
		{"/app2.js", "", false},
		{"/app2.js", "br", false},
		{"/huge.js", "gzip", false},
		{"/app2.js", "gzip", true},
	} {
		head := irDo(h, "HEAD", tc.uri, "Accept-Encoding", tc.ae)
		get := irDo(h, "GET", tc.uri, "Accept-Encoding", tc.ae)
		label := tc.uri + " Accept-Encoding " + tc.ae
		if got, want := irHeadContentLength(t, head), len(get.Response.Body()); got != want {
			t.Errorf("%s: HEAD Content-Length = %d, GET body = %d bytes", label, got, want)
		}
		gotCE := string(head.Response.Header.Peek("Content-Encoding"))
		if tc.encoded != (gotCE == "gzip") {
			t.Errorf("%s: HEAD Content-Encoding = %q", label, gotCE)
		}
		if !tc.encoded {
			if n := len(head.Response.Body()); n != 0 {
				t.Errorf("%s: HEAD response holds a %d-byte body the middleware never encodes", label, n)
			}
			if got, want := head.Response.Header.ContentLength(), len(files[tc.uri[1:]]); got != want {
				t.Errorf("%s: HEAD Content-Length header = %d, want %d", label, got, want)
			}
		}
	}
}

// P-F9: the handler loads sidecars through cache.LoadSidecar, the code
// preload uses, so an empty sidecar is never served. An empty app.js.br was
// served as a zero-byte br body.
func TestSidecar_EmptyIsIgnored(t *testing.T) {
	for _, cacheOn := range []bool{true, false} {
		root, cfg := setupTestDir(t)
		cfg.Cache.Enabled = cacheOn
		src := rbJS(16 << 10)
		if err := os.WriteFile(filepath.Join(root, "app2.js"), src, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "app2.js.br"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
		for i := 0; i < 2; i++ {
			ctx := irDo(h, "GET", "/app2.js", "Accept-Encoding", "br, gzip")
			if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce == "br" {
				t.Errorf("cache=%v req %d: empty br sidecar served (body %d bytes)", cacheOn, i, len(ctx.Response.Body()))
				continue
			}
			if got := decodeBody(t, ctx); got != string(src) {
				t.Errorf("cache=%v req %d: body does not decode to the source", cacheOn, i)
			}
		}
	}
}
