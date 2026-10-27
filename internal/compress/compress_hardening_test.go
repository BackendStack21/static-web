package compress_test

import (
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// B16: coding names are case-insensitive and q is parsed numerically (RFC 9110 §12.5.3, §12.4.2).
func TestAcceptsEncodingStr_Hardening(t *testing.T) {
	cases := []struct {
		header string
		enc    string
		want   bool
	}{
		{"GZIP", "gzip", true},
		{"Gzip, br", "gzip", true},
		{"br, GZip;q=0.5", "gzip", true},
		{"gzip", "GZIP", true},
		{"gzip;q=0", "gzip", false},
		{"gzip;q=0.0", "gzip", false},
		{"gzip;q=0.00", "gzip", false},
		{"gzip;q=0.000", "gzip", false},
		{"gzip;Q=0", "gzip", false},
		{"gzip ; q=0", "gzip", false},
		{"gzip; q = 0", "gzip", false},
		{"GZIP;q=0", "gzip", false},
		{"gzip;q=0.001", "gzip", true},
		{"gzip;q=0.5", "gzip", true},
		{"gzip;q=1", "gzip", true},
		{"deflate, gzip ; q=0 , br", "gzip", false},
		{"deflate, gzip ; q=0 , br", "br", true},
	}
	for _, tc := range cases {
		if got := compress.AcceptsEncodingStr(tc.header, tc.enc); got != tc.want {
			t.Errorf("AcceptsEncodingStr(%q, %q) = %v, want %v", tc.header, tc.enc, got, tc.want)
		}
	}
}

func hardeningCfg() *config.CompressionConfig {
	return &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5}
}

// B2: only complete (200) responses may be compressed on the fly.
func TestMiddleware_DoesNotCompressPartialContent(t *testing.T) {
	body := strings.Repeat("hello world ", 200)
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/plain")
		ctx.Response.Header.Set("Content-Range", "bytes 0-2399/5000")
		ctx.SetStatusCode(fasthttp.StatusPartialContent)
		ctx.SetBodyString(body)
	}
	h := compress.Middleware(hardeningCfg(), next)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/x.txt")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	h(&ctx)

	if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "" {
		t.Errorf("Content-Encoding = %q on 206, want none", ce)
	}
	if string(ctx.Response.Body()) != body {
		t.Errorf("206 body was modified (len %d, want %d)", len(ctx.Response.Body()), len(body))
	}
}

func TestMiddleware_DoesNotCompressWhenContentRangePresent(t *testing.T) {
	body := strings.Repeat("hello world ", 200)
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/plain")
		ctx.Response.Header.Set("Content-Range", "bytes 0-2399/5000")
		ctx.SetBodyString(body) // status 200 but Content-Range set
	}
	h := compress.Middleware(hardeningCfg(), next)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/x.txt")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	h(&ctx)

	if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "" {
		t.Errorf("Content-Encoding = %q with Content-Range present, want none", ce)
	}
}

// Streamed bodies must never be buffered/compressed by the middleware.
func TestMiddleware_DoesNotTouchStreamedBody(t *testing.T) {
	body := strings.Repeat("hello world ", 200)
	next := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/plain")
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyStream(strings.NewReader(body), len(body))
	}
	h := compress.Middleware(hardeningCfg(), next)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/x.txt")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	h(&ctx)

	if ce := string(ctx.Response.Header.Peek("Content-Encoding")); ce != "" {
		t.Errorf("Content-Encoding = %q on streamed body, want none", ce)
	}
	if !ctx.Response.IsBodyStream() {
		t.Error("streamed body was consumed by the middleware")
	}
}
