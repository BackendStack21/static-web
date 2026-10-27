package handler_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

func largeSetup(t *testing.T) (string, *config.Config, string) {
	t.Helper()
	root, cfg := setupTestDir(t)
	cfg.Cache.MaxFileSize = 1024
	var sb strings.Builder
	for i := 0; sb.Len() < 64*1024; i++ {
		fmt.Fprintf(&sb, "line %06d: .rule { color: red; }\n", i)
	}
	content := sb.String()
	if err := os.WriteFile(filepath.Join(root, "large.css"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return root, cfg, content
}

func largeGet(h fasthttp.RequestHandler, method string, hdrs map[string]string) *fasthttp.RequestCtx {
	ctx := newTestCtx(method, "/large.css")
	for k, v := range hdrs {
		ctx.Request.Header.Set(k, v)
	}
	h(ctx)
	return ctx
}

// B5: large files are served with validators, conditionals and without full buffering.
func TestLargeFile_ValidatorsAndConditionals(t *testing.T) {
	_, cfg, content := largeSetup(t)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	ctx := largeGet(h, "GET", nil)
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("status = %d, want 200", ctx.Response.StatusCode())
	}
	etag := string(ctx.Response.Header.Peek("ETag"))
	lm := string(ctx.Response.Header.Peek("Last-Modified"))
	cc := string(ctx.Response.Header.Peek("Cache-Control"))
	if etag == "" || lm == "" || cc == "" {
		t.Fatalf("missing ETag/Last-Modified/Cache-Control: %q %q %q", etag, lm, cc)
	}
	if got := ctx.Response.Header.ContentLength(); got != len(content) {
		t.Errorf("Content-Length = %d, want %d", got, len(content))
	}
	if string(ctx.Response.Body()) != content {
		t.Error("body differs from file content")
	}

	inm := largeGet(h, "GET", map[string]string{"If-None-Match": etag})
	if inm.Response.StatusCode() != fasthttp.StatusNotModified {
		t.Errorf("If-None-Match: status = %d, want 304", inm.Response.StatusCode())
	}
	if len(inm.Response.Body()) != 0 {
		t.Errorf("304 body length = %d", len(inm.Response.Body()))
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(cache.HTTPTimeFormat)
	ims := largeGet(h, "GET", map[string]string{"If-Modified-Since": future})
	if ims.Response.StatusCode() != fasthttp.StatusNotModified {
		t.Errorf("If-Modified-Since: status = %d, want 304", ims.Response.StatusCode())
	}
}

// The handler must stream large bodies from disk instead of buffering them.
func TestLargeFile_BodyIsStreamed(t *testing.T) {
	_, cfg, content := largeSetup(t)
	fh := handler.NewFileHandler(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	ctx := newTestCtx("GET", "/large.css")
	fh.HandleRequest(ctx)
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("status = %d", ctx.Response.StatusCode())
	}
	if !ctx.Response.IsBodyStream() {
		t.Error("large file body was buffered, want a body stream")
	}
	if string(ctx.Response.Body()) != content {
		t.Error("streamed body differs from file content")
	}
}

// HEAD must be answered from stat() alone.
func TestLargeFile_HeadDoesNotReadFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read write-only files")
	}
	root, cfg, content := largeSetup(t)
	if err := os.Chmod(filepath.Join(root, "large.css"), 0200); err != nil {
		t.Fatal(err)
	}
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	ctx := largeGet(h, "HEAD", nil)
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("HEAD status = %d, want 200 without reading the file", ctx.Response.StatusCode())
	}
	if got := ctx.Response.Header.ContentLength(); got != len(content) {
		t.Errorf("HEAD Content-Length = %d, want %d", got, len(content))
	}
	if len(ctx.Response.Body()) != 0 {
		t.Errorf("HEAD body length = %d", len(ctx.Response.Body()))
	}
}

func TestLargeFile_Ranges(t *testing.T) {
	_, cfg, content := largeSetup(t)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	single := largeGet(h, "GET", map[string]string{"Range": "bytes=10-19"})
	if single.Response.StatusCode() != 206 || string(single.Response.Body()) != content[10:20] {
		t.Errorf("single range: status %d body %q", single.Response.StatusCode(), single.Response.Body())
	}
	if cr := string(single.Response.Header.Peek("Content-Range")); cr != fmt.Sprintf("bytes 10-19/%d", len(content)) {
		t.Errorf("Content-Range = %q", cr)
	}

	multi := largeGet(h, "GET", map[string]string{"Range": "bytes=0-9,100-109"})
	if multi.Response.StatusCode() != 206 {
		t.Fatalf("multi range status = %d", multi.Response.StatusCode())
	}
	mt, params, err := mime.ParseMediaType(string(multi.Response.Header.Peek("Content-Type")))
	if err != nil || mt != "multipart/byteranges" {
		t.Fatalf("Content-Type = %q (%v)", multi.Response.Header.Peek("Content-Type"), err)
	}
	mr := multipart.NewReader(bytes.NewReader(multi.Response.Body()), params["boundary"])
	want := []string{content[0:10], content[100:110]}
	for i, w := range want {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		b, _ := io.ReadAll(part)
		if string(b) != w {
			t.Errorf("part %d = %q, want %q", i, b, w)
		}
	}

	mismatch := largeGet(h, "GET", map[string]string{"Range": "bytes=0-9", "If-Range": `"stale"`})
	if mismatch.Response.StatusCode() != 200 || string(mismatch.Response.Body()) != content {
		t.Errorf("If-Range mismatch: status %d, body len %d", mismatch.Response.StatusCode(), len(mismatch.Response.Body()))
	}
}

func decodeBody(t *testing.T, ctx *fasthttp.RequestCtx) string {
	t.Helper()
	body := append([]byte(nil), ctx.Response.Body()...)
	if string(ctx.Response.Header.Peek("Content-Encoding")) == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("gzip body does not decode: %v", err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gzip body does not decode: %v", err)
		}
		return string(out)
	}
	return string(body)
}

func TestLargeFile_WithGzipAcceptIsCorrect(t *testing.T) {
	_, cfg, content := largeSetup(t)
	cfg.Compression.MaxCompressSize = 1024
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	ctx := largeGet(h, "GET", map[string]string{"Accept-Encoding": "gzip"})
	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("status = %d", ctx.Response.StatusCode())
	}
	if got := decodeBody(t, ctx); got != content {
		t.Errorf("decoded body differs (len %d vs %d)", len(got), len(content))
	}
	if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "" {
		t.Errorf("Content-Encoding = %q for body above max_compress_size", ce)
	}
}

// B15: a sidecar larger than its source file is never loaded.
func TestSidecar_LargerThanSourceIsIgnored(t *testing.T) {
	root, cfg := setupTestDir(t)
	src := strings.Repeat("function f(){return 1}\n", 200)
	if err := os.WriteFile(filepath.Join(root, "app2.js"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	junk := bytes.Repeat([]byte{0xAB}, 5*len(src))
	if err := os.WriteFile(filepath.Join(root, "app2.js.gz"), junk, 0644); err != nil {
		t.Fatal(err)
	}
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	for i := 0; i < 2; i++ {
		ctx := newTestCtx("GET", "/app2.js")
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		h(ctx)
		if ctx.Response.StatusCode() != 200 {
			t.Fatalf("status = %d", ctx.Response.StatusCode())
		}
		if got := decodeBody(t, ctx); got != src {
			t.Errorf("req %d: served body is not the source (len %d)", i, len(got))
		}
		if bytes.Equal(ctx.Response.Body(), junk) {
			t.Errorf("req %d: oversize sidecar was served", i)
		}
	}
}
