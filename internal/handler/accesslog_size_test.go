package handler

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

func alServeWith(h fasthttp.RequestHandler, method, uri string, hdrs ...string) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	for i := 0; i+1 < len(hdrs); i += 2 {
		ctx.Request.Header.Set(hdrs[i], hdrs[i+1])
	}
	h(&ctx)
	return &ctx
}

// alLoggedSize returns the size field of the single access-log line for
// "METHOD uri STATUS".
func alLoggedSize(t *testing.T, dst *alRecordingWriter, method, uri string, status int) int {
	t.Helper()
	if err := FlushAccessLog(); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(` ` + method + ` ` + regexp.QuoteMeta(uri) + ` ` + strconv.Itoa(status) + ` (\d+) `)
	var sizes []string
	for _, l := range dst.lines() {
		if m := re.FindStringSubmatch(l); m != nil {
			sizes = append(sizes, m[1])
		}
	}
	if len(sizes) != 1 {
		t.Fatalf("found %d log lines for %s %s %d, want 1: %q", len(sizes), method, uri, status, dst.lines())
	}
	n, _ := strconv.Atoi(sizes[0])
	return n
}

// The compress middleware gzips some responses on the fly and deletes
// Content-Length, so ContentLength() reads 0. The log must record the
// bytes actually sent.
func TestAccessLog_SizeOfOnTheFlyGzippedResponseIsBodyLength(t *testing.T) {
	dst := &alRecordingWriter{}
	alWithLogOutput(t, dst)
	cfg := alConfig(t)
	cfg.Security.DirectoryListing = true // the listing is gzipped by the middleware
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 64
	cfg.Compression.Level = 5
	for i := 0; i < 20; i++ {
		name := filepath.Join(cfg.Files.Root, "file-with-a-long-name-"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := BuildHandler(cfg, nil)

	var ctx = alServeWith(h, "GET", "/", "Accept-Encoding", "gzip")
	if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want on-the-fly gzip", ce)
	}
	sent := len(ctx.Response.Body())
	if sent == 0 {
		t.Fatal("empty gzipped body")
	}
	if got := alLoggedSize(t, dst, "GET", "/", 200); got != sent {
		t.Errorf("logged size = %d, want the %d gzipped bytes sent", got, sent)
	}
}

// Buffered bodies set without an explicit Content-Length (404 page, error
// text) are logged with their length too.
func TestAccessLog_SizeOfBufferedBodyWithoutContentLength(t *testing.T) {
	dst := &alRecordingWriter{}
	alWithLogOutput(t, dst)
	h := BuildHandler(alConfig(t), nil)
	ctx := alServe(h, "GET", "/missing.txt")
	if ctx.Response.StatusCode() != 404 || len(ctx.Response.Body()) == 0 {
		t.Fatalf("status %d body %d bytes, want a 404 page", ctx.Response.StatusCode(), len(ctx.Response.Body()))
	}
	if got, want := alLoggedSize(t, dst, "GET", "/missing.txt", 404), len(ctx.Response.Body()); got != want {
		t.Errorf("logged size = %d, want %d", got, want)
	}
}

// A streamed body must not be read by the logger (Response.Body() would
// drain the stream into memory); its declared length is logged. HEAD logs
// the declared Content-Length as before.
func TestAccessLog_SizeOfStreamedAndHeadResponsesUsesContentLength(t *testing.T) {
	dst := &alRecordingWriter{}
	alWithLogOutput(t, dst)
	cfg := alConfig(t)
	cfg.Cache.MaxFileSize = 1024 // big.bin is served as a stream
	big := strings.Repeat("b", 64<<10)
	if err := os.WriteFile(filepath.Join(cfg.Files.Root, "big.bin"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	h := BuildHandler(cfg, nil)

	ctx := alServe(h, "GET", "/big.bin")
	if !ctx.Response.IsBodyStream() {
		t.Fatal("large file body is no longer a stream after the logging middleware ran")
	}
	if got := alLoggedSize(t, dst, "GET", "/big.bin", 200); got != len(big) {
		t.Errorf("logged size for streamed GET = %d, want %d", got, len(big))
	}
	_ = ctx.Response.CloseBodyStream()

	alServe(h, "HEAD", "/a.txt")
	if got := alLoggedSize(t, dst, "HEAD", "/a.txt", 200); got != len("hello") {
		t.Errorf("logged size for HEAD = %d, want the declared %d", got, len("hello"))
	}
}
