package handler_test

import (
	"bufio"
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// irIgnoredRanges are Range requests that the server ignores (RFC 9110
// §14.2), answering with the full representation. Each entry is a list of
// header name/value pairs.
func irIgnoredRanges(weakETag string) map[string][]string {
	tooMany := "bytes=" + strings.TrimSuffix(strings.Repeat("0-0,", 17), ",")
	return map[string][]string{
		"malformed":                  {"Range", "bytes=x"},
		"overlapping":                {"Range", "bytes=0-,0-"},
		"too many ranges":            {"Range", tooMany},
		"If-Range weak ETag":         {"Range", "bytes=0-99", "If-Range", weakETag},
		"If-Range other ETag":        {"Range", "bytes=0-99", "If-Range", `"other"`},
		"If-Range non-matching date": {"Range", "bytes=0-99", "If-Range", "Mon, 01 Jan 2001 00:00:00 GMT"},
	}
}

func irDo(h fasthttp.RequestHandler, method, uri string, hdrs ...string) *fasthttp.RequestCtx {
	ctx := newTestCtx(method, uri)
	for i := 0; i+1 < len(hdrs); i += 2 {
		ctx.Request.Header.Set(hdrs[i], hdrs[i+1])
	}
	h(ctx)
	return ctx
}

// irHeadContentLength returns the Content-Length the HEAD response in ctx
// carries on the wire. fasthttp serialises a HEAD response like the server
// does (SkipBody): an explicit Content-Length is kept, and a body that was
// set (e.g. one the compress middleware encoded) only determines it.
func irHeadContentLength(t *testing.T, ctx *fasthttp.RequestCtx) int {
	t.Helper()
	var resp fasthttp.Response
	ctx.Response.CopyTo(&resp)
	resp.SkipBody = true
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := resp.Write(bw); err != nil {
		t.Fatalf("serialise HEAD response: %v", err)
	}
	_ = bw.Flush()
	hr, err := http.ReadResponse(bufio.NewReader(&buf), &http.Request{Method: "HEAD"})
	if err != nil {
		t.Fatalf("parse HEAD response: %v", err)
	}
	return int(hr.ContentLength)
}

// HTTP-F2: when a Range header is ignored the full 200 must be the same
// representation a request without Range gets: the cached variant the client
// negotiated, with its Content-Encoding. Serving the identity bytes instead
// made the compress middleware gzip the whole file on every such request,
// and HEAD (identity, uncompressed length) disagree with GET (gzip).
func TestIgnoredRange_ServesCachedEncodedVariant(t *testing.T) {
	root, cfg := setupTestDir(t)
	cfg.Cache.MaxFileSize = 4 << 20
	cfg.Compression.MaxCompressSize = 10 << 20
	src := rbJS(256 << 10)
	if err := os.WriteFile(filepath.Join(root, "big.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	brSidecar := bytes.Repeat([]byte("BR-SIDECAR "), 100)
	if err := os.WriteFile(filepath.Join(root, "big.js.br"), brSidecar, 0o644); err != nil {
		t.Fatal(err)
	}
	c := cache.NewCache(cfg.Cache.MaxBytes)
	h := handler.BuildHandlerQuiet(cfg, c)

	warm := irDo(h, "GET", "/big.js", "Accept-Encoding", "gzip")
	if warm.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("warm-up status = %d", warm.Response.StatusCode())
	}
	etag := string(warm.Response.Header.Peek("ETag"))
	f, ok := c.Get("/big.js")
	if !ok || f.GzipData == nil || f.ZstdData == nil || !bytes.Equal(f.BrData, brSidecar) {
		t.Fatalf("cache entry missing variants: ok=%v gzip=%d zstd=%d br=%d", ok,
			len(f.GzipData), len(f.ZstdData), len(f.BrData))
	}

	encodings := []struct {
		ae, ce string
		want   []byte
	}{
		{"gzip", "gzip", f.GzipData},
		{"zstd, gzip", "zstd", f.ZstdData},
		{"gzip, br, zstd", "br", f.BrData},
		{"", "", f.Data},
	}
	for name, rh := range irIgnoredRanges(etag) {
		for _, enc := range encodings {
			hdrs := append([]string{"Accept-Encoding", enc.ae}, rh...)
			label := name + " / Accept-Encoding " + strconv.Quote(enc.ae)

			get := irDo(h, "GET", "/big.js", hdrs...)
			if got := get.Response.StatusCode(); got != fasthttp.StatusOK {
				t.Errorf("%s: GET status = %d, want 200", label, got)
				continue
			}
			if got := string(get.Response.Header.Peek("Content-Encoding")); got != enc.ce {
				t.Errorf("%s: GET Content-Encoding = %q, want %q", label, got, enc.ce)
			}
			if !bytes.Equal(get.Response.Body(), enc.want) {
				t.Errorf("%s: GET body (%d bytes) is not the cached %q variant (%d bytes)",
					label, len(get.Response.Body()), enc.ce, len(enc.want))
			}

			head := irDo(h, "HEAD", "/big.js", hdrs...)
			if got := head.Response.StatusCode(); got != fasthttp.StatusOK {
				t.Errorf("%s: HEAD status = %d, want 200", label, got)
			}
			if got := string(head.Response.Header.Peek("Content-Encoding")); got != enc.ce {
				t.Errorf("%s: HEAD Content-Encoding = %q, want %q", label, got, enc.ce)
			}
			if got := irHeadContentLength(t, head); got != len(enc.want) {
				t.Errorf("%s: HEAD Content-Length = %d, want %d (GET body length)", label, got, len(enc.want))
			}
		}
	}
}

// A Range header that is honoured still gets the identity bytes (206), and an
// unsatisfiable one still gets 416 without a Content-Encoding.
func TestIgnoredRange_HonouredRangesStayIdentity(t *testing.T) {
	root, cfg := setupTestDir(t)
	src := rbJS(64 << 10)
	if err := os.WriteFile(filepath.Join(root, "big.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	irDo(h, "GET", "/big.js") // cache it

	lm := string(irDo(h, "GET", "/big.js").Response.Header.Peek("Last-Modified"))
	for _, hdrs := range [][]string{
		{"Range", "bytes=0-99"},
		{"Range", "bytes=0-99", "If-Range", lm},
	} {
		ctx := irDo(h, "GET", "/big.js", append([]string{"Accept-Encoding", "gzip, br, zstd"}, hdrs...)...)
		if got := ctx.Response.StatusCode(); got != fasthttp.StatusPartialContent {
			t.Fatalf("%v: status = %d, want 206", hdrs, got)
		}
		if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "" {
			t.Errorf("%v: Content-Encoding = %q on 206, want none", hdrs, ce)
		}
		if !bytes.Equal(ctx.Response.Body(), src[:100]) {
			t.Errorf("%v: 206 body is not identity bytes 0-99", hdrs)
		}
	}

	ctx := irDo(h, "GET", "/big.js", "Accept-Encoding", "gzip", "Range", "bytes=999999-")
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable: status = %d, want 416", got)
	}
	if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "" {
		t.Errorf("unsatisfiable: Content-Encoding = %q on 416, want none", ce)
	}
}

// With the file cache disabled an ignored Range must not make HEAD and GET
// disagree either.
func TestIgnoredRange_CacheDisabledHeadMatchesGet(t *testing.T) {
	root, cfg := setupTestDir(t)
	cfg.Cache.Enabled = false
	if err := os.WriteFile(filepath.Join(root, "big.js"), rbJS(64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	h := handler.BuildHandlerQuiet(cfg, nil)
	for name, rh := range irIgnoredRanges(`W/"x"`) {
		hdrs := append([]string{"Accept-Encoding", "gzip"}, rh...)
		get := irDo(h, "GET", "/big.js", hdrs...)
		head := irDo(h, "HEAD", "/big.js", hdrs...)
		if get.Response.StatusCode() != 200 || head.Response.StatusCode() != 200 {
			t.Errorf("%s: status GET %d HEAD %d, want 200", name, get.Response.StatusCode(), head.Response.StatusCode())
			continue
		}
		gce, hce := string(get.Response.Header.Peek("Content-Encoding")), string(head.Response.Header.Peek("Content-Encoding"))
		if gce != hce {
			t.Errorf("%s: Content-Encoding GET %q, HEAD %q", name, gce, hce)
		}
		if got, want := irHeadContentLength(t, head), len(get.Response.Body()); got != want {
			t.Errorf("%s: HEAD Content-Length = %d, GET body = %d bytes", name, got, want)
		}
	}
}
