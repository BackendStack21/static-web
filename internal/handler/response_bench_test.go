package handler_test

// Benchmarks for the response path findings of the 2026-11 review:
// ignored Range headers on cached files (HTTP-F2), requests with the file
// cache disabled (P-F4) and large-file streaming over a real TCP connection
// (P-F6 / HTTP-F1). They use only the public API so that the same file runs
// against the code before and after the fixes.

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// rbJS returns n bytes of JavaScript-like text that compresses about as well
// as real minified code.
func rbJS(n int) []byte {
	var b strings.Builder
	r := rand.New(rand.NewSource(1))
	for b.Len() < n {
		fmt.Fprintf(&b, "function f%d(a,b){return a*%d+b;}\n", r.Int(), r.Int())
	}
	return []byte(b.String()[:n])
}

// BenchmarkIgnoredRange measures cached 1 MiB JavaScript requests whose Range
// header is ignored, so a full 200 is served. Before HTTP-F2 these were
// gzipped on the fly on every request.
func BenchmarkIgnoredRange(b *testing.B) {
	root := benchRoot(b)
	if err := os.WriteFile(filepath.Join(root, "big.js"), rbJS(1<<20), 0o644); err != nil {
		b.Fatal(err)
	}
	cfg := benchConfig(root)
	cfg.Compression.MaxCompressSize = 10 << 20
	h := benchBuild(cfg, false)

	// The weak cached ETag never satisfies If-Range, so the range is ignored.
	ctx := &fasthttp.RequestCtx{}
	benchReset(ctx, "GET", "/big.js", "Accept-Encoding", "gzip")
	h(ctx)
	etag := string(ctx.Response.Header.Peek("ETag"))

	for _, tc := range []struct {
		name string
		hdrs []string
	}{
		{"none", nil},
		{"malformed", []string{"Range", "bytes=x"}},
		{"overlapping", []string{"Range", "bytes=0-,0-"}},
		{"if-range-mismatch", []string{"Range", "bytes=0-99", "If-Range", etag}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			hdrs := append([]string{"Accept-Encoding", "gzip"}, tc.hdrs...)
			benchRun(b, h, 200, "GET", "/big.js", hdrs...)
		})
	}
}

// BenchmarkCacheDisabled measures requests for an 11 KB script with the file
// cache disabled, with and without compression. Before P-F4 every one of
// them built gzip and zstd variants that were thrown away.
func BenchmarkCacheDisabled(b *testing.B) {
	root := benchRoot(b)
	for _, comp := range []bool{true, false} {
		name := "compression-on"
		if !comp {
			name = "compression-off"
		}
		b.Run(name, func(b *testing.B) {
			cfg := benchConfig(root)
			cfg.Cache.Enabled = false
			cfg.Compression.Enabled = comp
			h := handler.BuildHandlerQuiet(cfg, nil)
			for _, m := range []string{"GET", "HEAD"} {
				b.Run(m, func(b *testing.B) {
					benchRun(b, h, 200, m, "/app.js", "Accept-Encoding", benchAcceptEncoding)
				})
				b.Run(m+"-identity", func(b *testing.B) {
					benchRun(b, h, 200, m, "/app.js")
				})
			}
		})
	}
}

// BenchmarkLargeFileTCP measures a 64 MiB large-file download over loopback
// TCP through fasthttp.Server, which is where sendfile(2) applies.
func BenchmarkLargeFileTCP(b *testing.B) {
	const size = 64 << 20
	root := benchRoot(b)
	f, err := os.Create(filepath.Join(root, "big.bin"))
	if err != nil {
		b.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		b.Fatal(err)
	}
	f.Close()

	cfg := benchConfig(root)
	cfg.Server.WriteTimeout = 10 * time.Second
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	srv := &fasthttp.Server{Handler: h, WriteTimeout: cfg.Server.WriteTimeout, NoDefaultServerHeader: true}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	b.Cleanup(func() { _ = srv.Shutdown() })

	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReaderSize(c, 1<<20)
	buf := make([]byte, 1<<20)
	sink := struct{ io.Writer }{io.Discard} // no ReadFrom: use buf

	get := func() {
		if _, err := io.WriteString(c, "GET /big.bin HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			b.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.CopyBuffer(sink, resp.Body, buf)
		resp.Body.Close()
		if err != nil || n != size || resp.StatusCode != 200 {
			b.Fatalf("status %d, read %d bytes, err %v", resp.StatusCode, n, err)
		}
	}
	get() // warm up
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		get()
	}
}
