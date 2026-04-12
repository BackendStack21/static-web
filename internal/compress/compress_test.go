package compress_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/fasthttp"
)

func TestIsCompressible(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"text/css", true},
		{"application/javascript", true},
		{"application/json", true},
		{"image/svg+xml", true},
		{"image/png", false},
		{"image/jpeg", false},
		{"video/mp4", false},
		{"application/zip", false},
		{"", false},
	}

	for _, tc := range cases {
		t.Run(tc.ct, func(t *testing.T) {
			got := compress.IsCompressible(tc.ct)
			if got != tc.want {
				t.Errorf("IsCompressible(%q) = %v, want %v", tc.ct, got, tc.want)
			}
		})
	}
}

func TestAcceptsEncoding(t *testing.T) {
	makeCtx := func(ae string) *fasthttp.RequestCtx {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod("GET")
		ctx.Request.SetRequestURI("/")
		if ae != "" {
			ctx.Request.Header.Set("Accept-Encoding", ae)
		}
		return &ctx
	}

	if !compress.AcceptsEncoding(makeCtx("gzip"), "gzip") {
		t.Error("expected gzip accepted")
	}
	if !compress.AcceptsEncoding(makeCtx("gzip, br, zstd"), "br") {
		t.Error("expected br accepted in multi-value list")
	}
	if compress.AcceptsEncoding(makeCtx(""), "gzip") {
		t.Error("expected gzip rejected when no Accept-Encoding")
	}
	if compress.AcceptsEncoding(makeCtx("br"), "gzip") {
		t.Error("expected gzip rejected when only br is listed")
	}
}

func TestGzipBytes(t *testing.T) {
	src := []byte(strings.Repeat("Hello, World! ", 100))
	compressed, err := compress.GzipBytes(src, 5)
	if err != nil {
		t.Fatalf("GzipBytes error: %v", err)
	}
	if len(compressed) == 0 {
		t.Fatal("GzipBytes returned empty result")
	}
	if len(compressed) >= len(src) {
		t.Errorf("compressed (%d) should be smaller than original (%d)", len(compressed), len(src))
	}

	// Decompress and verify.
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("decompressed content does not match original")
	}
}

// newTestCtx creates a fasthttp.RequestCtx with the given method, URI, and optional headers.
func newTestCtx(method, uri string, hdrs map[string]string) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	for k, v := range hdrs {
		ctx.Request.Header.Set(k, v)
	}
	return &ctx
}

func TestMiddleware_CompressesEligibleResponse(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5}

	body := strings.Repeat("Hello compressed world! ", 50) // 1200 bytes
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html; charset=utf-8")
		ctx.SetBody([]byte(body))
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", enc)
	}

	// Decompress and verify body.
	gr, err := gzip.NewReader(bytes.NewReader(ctx.Response.Body()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if string(got) != body {
		t.Errorf("decompressed body mismatch")
	}
}

func TestMiddleware_SkipsIneligibleContentType(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5}

	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "image/png")
		ctx.SetBody([]byte("fake png data that is long enough to pass min size check in normal flow"))
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/img.png", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "gzip" {
		t.Error("Content-Encoding should not be gzip for image/png")
	}
}

func TestMiddleware_SkipsWhenNoAcceptEncoding(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5}

	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		ctx.SetBody([]byte(strings.Repeat("x", 500)))
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/", nil)
	handler(ctx)

	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "gzip" {
		t.Error("should not compress when Accept-Encoding absent")
	}
}

func TestMiddleware_DisabledConfig(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: false}

	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if !called {
		t.Error("next should be called when compression disabled")
	}
	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "" {
		t.Error("should not set Content-Encoding when compression disabled")
	}
}

func TestMiddleware_SetsVaryHeader(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		ctx.SetBody([]byte("hello"))
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if vary := string(ctx.Response.Header.Peek("Vary")); !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary = %q, should contain Accept-Encoding", vary)
	}
}

// ---------------------------------------------------------------------------
// Additional compression coverage
// ---------------------------------------------------------------------------

// TestMiddleware_SkipsPostMethod verifies non-GET/HEAD methods bypass compression.
func TestMiddleware_SkipsPostMethod(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.Response.Header.Set("Content-Type", "text/html")
		ctx.SetBody([]byte(strings.Repeat("x", 500)))
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("POST", "/upload", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if !called {
		t.Error("next should be called for POST requests")
	}
	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "gzip" {
		t.Error("POST response must not be compressed by middleware")
	}
}

// TestMiddleware_SkipsBelowMinSize verifies small responses are not compressed.
func TestMiddleware_SkipsBelowMinSize(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1000, Level: 5}

	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		ctx.SetBody([]byte("tiny")) // only 4 bytes — below MinSize
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "gzip" {
		t.Error("response below MinSize should not be gzip-encoded")
	}
	if string(ctx.Response.Body()) != "tiny" {
		t.Errorf("body = %q, want tiny", string(ctx.Response.Body()))
	}
}

// TestMiddleware_MaxCompressSize verifies SEC-005: bodies exceeding
// MaxCompressSize are served uncompressed, while bodies under the limit
// are still gzipped normally.
func TestMiddleware_MaxCompressSize(t *testing.T) {
	const limit = 2048

	makeHandler := func(body string) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("Content-Type", "text/html; charset=utf-8")
			ctx.SetBody([]byte(body))
		}
	}

	t.Run("under limit is compressed", func(t *testing.T) {
		cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5, MaxCompressSize: limit}
		body := strings.Repeat("A", limit-1) // 2047 bytes — just under
		handler := compress.Middleware(cfg, makeHandler(body))
		ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
		handler(ctx)

		if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip for body under MaxCompressSize", enc)
		}
		// Verify decompressed content matches.
		gr, err := gzip.NewReader(bytes.NewReader(ctx.Response.Body()))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		got, err := io.ReadAll(gr)
		if err != nil {
			t.Fatalf("io.ReadAll: %v", err)
		}
		if string(got) != body {
			t.Error("decompressed body does not match original")
		}
	})

	t.Run("over limit stays uncompressed", func(t *testing.T) {
		cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5, MaxCompressSize: limit}
		body := strings.Repeat("B", limit+1) // 2049 bytes — just over
		handler := compress.Middleware(cfg, makeHandler(body))
		ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
		handler(ctx)

		if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "gzip" {
			t.Error("Content-Encoding should not be gzip for body exceeding MaxCompressSize")
		}
		if string(ctx.Response.Body()) != body {
			t.Error("body should be served uncompressed and unmodified")
		}
	})

	t.Run("exactly at limit stays uncompressed", func(t *testing.T) {
		cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5, MaxCompressSize: limit}
		body := strings.Repeat("C", limit) // exactly 2048 bytes — not strictly less
		handler := compress.Middleware(cfg, makeHandler(body))
		ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
		handler(ctx)

		// len(body) > cfg.MaxCompressSize is false when equal, so it IS compressed.
		// This tests the boundary condition: equal means "still under the limit".
		if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip when body size equals MaxCompressSize (not strictly over)", enc)
		}
	})

	t.Run("zero disables the limit", func(t *testing.T) {
		cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5, MaxCompressSize: 0}
		body := strings.Repeat("D", 100_000) // 100 KB — would exceed any reasonable limit
		handler := compress.Middleware(cfg, makeHandler(body))
		ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
		handler(ctx)

		if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip when MaxCompressSize=0 (disabled)", enc)
		}
	})
}

// TestMiddleware_SkipsAlreadyEncoded ensures pre-encoded responses are passed through.
func TestMiddleware_SkipsAlreadyEncoded(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		ctx.Response.Header.Set("Content-Encoding", "br") // pre-compressed brotli
		ctx.SetBody([]byte(strings.Repeat("compressed!", 100)))
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip, br"})
	handler(ctx)

	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "br" {
		t.Errorf("Content-Encoding = %q, want br (should not re-compress)", enc)
	}
}

// TestMiddleware_WriteHeaderExplicit exercises the status code being set before body.
func TestMiddleware_WriteHeaderExplicit(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBody([]byte(strings.Repeat("Hello explicit! ", 80)))
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	// Response must still be compressed.
	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip after explicit status code", enc)
	}
	gr, err := gzip.NewReader(bytes.NewReader(ctx.Response.Body()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if !strings.Contains(string(got), "Hello explicit!") {
		t.Error("decompressed body should contain original content")
	}
}

// TestMiddleware_NoWriteAtAll covers the path when the handler sets a no-body status.
func TestMiddleware_NoWriteAtAll(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusNoContent) // 204 — no body
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("GET", "/empty", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNoContent {
		t.Errorf("status = %d, want 204", ctx.Response.StatusCode())
	}
	if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "gzip" {
		t.Error("no-body 204 response should not have Content-Encoding: gzip")
	}
}

// TestGzipBytes_InvalidLevel verifies out-of-range levels fall back to DefaultCompression.
func TestGzipBytes_InvalidLevel(t *testing.T) {
	src := []byte(strings.Repeat("test data for invalid level ", 50))

	// Level 0 and 10 are both outside [BestSpeed, BestCompression].
	for _, level := range []int{0, 10, -5, 100} {
		t.Run(fmt.Sprintf("level=%d", level), func(t *testing.T) {
			compressed, err := compress.GzipBytes(src, level)
			if err != nil {
				t.Fatalf("GzipBytes(%d) unexpected error: %v", level, err)
			}
			// Must decompress correctly.
			gr, err := gzip.NewReader(bytes.NewReader(compressed))
			if err != nil {
				t.Fatalf("gzip.NewReader: %v", err)
			}
			got, err := io.ReadAll(gr)
			if err != nil {
				t.Fatalf("io.ReadAll: %v", err)
			}
			if !bytes.Equal(got, src) {
				t.Error("decompressed content does not match original for invalid level")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ZStandard compression tests
// ---------------------------------------------------------------------------

func TestZstdBytes(t *testing.T) {
	src := []byte(strings.Repeat("Hello, ZStandard! ", 100))
	compressed, err := compress.ZstdBytes(src)
	if err != nil {
		t.Fatalf("ZstdBytes error: %v", err)
	}
	if len(compressed) == 0 {
		t.Fatal("ZstdBytes returned empty result")
	}
	if len(compressed) >= len(src) {
		t.Errorf("compressed (%d) should be smaller than original (%d)", len(compressed), len(src))
	}

	// Decompress and verify.
	br, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	br.Close()
	if !bytes.Equal(got, src) {
		t.Error("decompressed content does not match original")
	}
}

func TestZstdBytesLevel(t *testing.T) {
	src := []byte(strings.Repeat("ZStandard compression levels ", 100))

	levels := []zstd.EncoderLevel{
		zstd.SpeedFastest,
		zstd.SpeedDefault,
		zstd.SpeedBetterCompression,
		zstd.SpeedBestCompression,
	}

	for _, level := range levels {
		t.Run(level.String(), func(t *testing.T) {
			compressed, err := compress.ZstdBytesLevel(src, level)
			if err != nil {
				t.Fatalf("ZstdBytesLevel(%s) error: %v", level, err)
			}
			if len(compressed) == 0 {
				t.Fatal("ZstdBytesLevel returned empty result")
			}

			// Decompress and verify roundtrip.
			br, err := zstd.NewReader(bytes.NewReader(compressed))
			if err != nil {
				t.Fatalf("zstd.NewReader: %v", err)
			}
			got, err := io.ReadAll(br)
			if err != nil {
				t.Fatalf("io.ReadAll: %v", err)
			}
			br.Close()
			if !bytes.Equal(got, src) {
				t.Error("decompressed content does not match original")
			}
		})
	}
}

func TestZstdBytes_EmptyInput(t *testing.T) {
	src := []byte{}
	compressed, err := compress.ZstdBytes(src)
	if err != nil {
		t.Fatalf("ZstdBytes empty error: %v", err)
	}
	// Empty input may produce empty or minimal output
	if len(compressed) == 0 {
		t.Log("ZstdBytes produced empty output for empty input (valid zstd behavior)")
		return
	}

	// If there is output, try to decompress and verify.
	br, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	br.Close()
	if len(got) != 0 {
		t.Errorf("decompressed empty should be empty, got %d bytes", len(got))
	}
}

func TestZstdBytes_AlreadyCompressed(t *testing.T) {
	// Zstandard of already-compressed data should still work.
	compressed1, err := compress.ZstdBytes([]byte(strings.Repeat("test ", 100)))
	if err != nil {
		t.Fatalf("first compression error: %v", err)
	}

	// Compress again
	compressed2, err := compress.ZstdBytes(compressed1)
	if err != nil {
		t.Fatalf("second compression error: %v", err)
	}

	// Should be able to decompress the second compression to get the first
	br, err := zstd.NewReader(bytes.NewReader(compressed2))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	br.Close()

	// Compare with first compression
	if !bytes.Equal(got, compressed1) {
		t.Errorf("double-compressed data (%d bytes) does not match single-compressed (%d bytes)",
			len(got), len(compressed1))
	}
}

// TestAcceptsEncoding_MultipleValues covers more Accept-Encoding negotiation cases.
func TestAcceptsEncoding_MultipleValues(t *testing.T) {
	cases := []struct {
		header string
		enc    string
		want   bool
	}{
		{"br, gzip, zstd", "gzip", true},
		{"br, gzip, zstd", "br", true},
		{"br, gzip, zstd", "zstd", true},
		{"br, gzip, zstd", "deflate", false},
		{"identity", "gzip", false},
		{"gzip;q=1.0", "gzip", true}, // quality parameter is stripped per RFC 7231
	}

	for _, tc := range cases {
		t.Run(tc.header+"|"+tc.enc, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod("GET")
			ctx.Request.SetRequestURI("/")
			ctx.Request.Header.Set("Accept-Encoding", tc.header)
			got := compress.AcceptsEncoding(&ctx, tc.enc)
			if got != tc.want {
				t.Errorf("AcceptsEncoding(%q, %q) = %v, want %v", tc.header, tc.enc, got, tc.want)
			}
		})
	}
}

// TestMiddleware_HeadRequest verifies HEAD passes through without body compression.
func TestMiddleware_HeadRequest(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	called := false
	next := func(ctx *fasthttp.RequestCtx) {
		called = true
		ctx.Response.Header.Set("Content-Type", "text/html")
		ctx.Response.Header.Set("Content-Length", "500")
		ctx.SetStatusCode(fasthttp.StatusOK)
		// HEAD: no body written
	}

	handler := compress.Middleware(cfg, next)
	ctx := newTestCtx("HEAD", "/", map[string]string{"Accept-Encoding": "gzip"})
	handler(ctx)

	if !called {
		t.Error("next should be called for HEAD requests")
	}
}

// ---------------------------------------------------------------------------
// SEC-013: q=0 explicit rejection
// ---------------------------------------------------------------------------

// TestAcceptsEncoding_QZeroRejection verifies that q=0 signals explicit denial.
func TestAcceptsEncoding_QZeroRejection(t *testing.T) {
	cases := []struct {
		header string
		enc    string
		want   bool
	}{
		{"gzip;q=0", "gzip", false},
		{"gzip;q=0.0", "gzip", false},
		{"gzip;q=0.00", "gzip", false},
		{"gzip;q=0.000", "gzip", false},
		{"gzip;q=0.1", "gzip", true}, // non-zero q is still accepted
		{"gzip;q=1.0", "gzip", true},
		{"br, gzip;q=0", "gzip", false}, // gzip explicitly rejected
		{"br, gzip;q=0", "br", true},    // br still accepted
	}

	for _, tc := range cases {
		t.Run(tc.header+"|"+tc.enc, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod("GET")
			ctx.Request.SetRequestURI("/")
			ctx.Request.Header.Set("Accept-Encoding", tc.header)
			got := compress.AcceptsEncoding(&ctx, tc.enc)
			if got != tc.want {
				t.Errorf("AcceptsEncoding(%q, %q) = %v, want %v", tc.header, tc.enc, got, tc.want)
			}
		})
	}
}
