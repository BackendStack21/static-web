package handler_test

// Benchmarks for the request path-validation and lookup flow, run both with
// and without a security.PathCache. The older BenchmarkHandler_* benchmarks
// build the chain without a PathCache only, so they could not see costs that
// depend on it (e.g. the 404 double validation found in the 2026-11 review).
// Keep both variants of every case here.

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

const benchAcceptEncoding = "gzip, deflate, br, zstd"

// benchConfig mirrors the production defaults that matter on the hot path.
func benchConfig(root string) *config.Config {
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Files.MaxServeFileSize = 1 << 30
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 256 << 20
	cfg.Cache.MaxFileSize = 10 << 20
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 1024
	cfg.Compression.Level = 5
	cfg.Compression.Precompressed = true
	cfg.Security.BlockDotfiles = true
	cfg.Security.CSP = "default-src 'self'"
	cfg.Security.ReferrerPolicy = "strict-origin-when-cross-origin"
	cfg.Security.PermissionsPolicy = "geolocation=(), microphone=(), camera=()"
	cfg.Headers.StaticMaxAge = 3600
	cfg.Headers.EnableETags = true
	return cfg
}

// benchRoot creates a root holding app.js (~11 KB) and silences the logger.
func benchRoot(b *testing.B) string {
	b.Helper()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	root := b.TempDir()
	js := strings.Repeat("console.log('hello world, this is a benchmark asset');\n", 200)
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte(js), 0o644); err != nil {
		b.Fatal(err)
	}
	return root
}

func benchBuild(cfg *config.Config, withPathCache bool) fasthttp.RequestHandler {
	c := cache.NewCache(cfg.Cache.MaxBytes)
	if withPathCache {
		return handler.BuildHandlerQuiet(cfg, c, security.NewPathCache(0))
	}
	return handler.BuildHandlerQuiet(cfg, c)
}

func benchReset(ctx *fasthttp.RequestCtx, method, uri string, hdrs ...string) {
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.ResetUserValues()
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	for i := 0; i+1 < len(hdrs); i += 2 {
		ctx.Request.Header.Set(hdrs[i], hdrs[i+1])
	}
}

// benchRun warms the caches with three requests, checks the status, then
// measures the same request.
func benchRun(b *testing.B, h fasthttp.RequestHandler, want int, method, uri string, hdrs ...string) {
	b.Helper()
	ctx := &fasthttp.RequestCtx{}
	for i := 0; i < 3; i++ {
		benchReset(ctx, method, uri, hdrs...)
		h(ctx)
	}
	if got := ctx.Response.StatusCode(); got != want {
		b.Fatalf("%s %s: status %d, want %d", method, uri, got, want)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchReset(ctx, method, uri, hdrs...)
		h(ctx)
	}
}

var benchPathCacheModes = []struct {
	name string
	on   bool
}{
	{"PathCache", true},
	{"NoPathCache", false},
}

// BenchmarkPathFlow_Hit measures file-cache hits through the full chain.
func BenchmarkPathFlow_Hit(b *testing.B) {
	root := benchRoot(b)
	for _, mode := range benchPathCacheModes {
		b.Run(mode.name, func(b *testing.B) {
			b.Run("identity", func(b *testing.B) {
				benchRun(b, benchBuild(benchConfig(root), mode.on), 200, "GET", "/app.js")
			})
			b.Run("encoded", func(b *testing.B) {
				benchRun(b, benchBuild(benchConfig(root), mode.on), 200, "GET", "/app.js",
					"Accept-Encoding", benchAcceptEncoding)
			})
			b.Run("304", func(b *testing.B) {
				h := benchBuild(benchConfig(root), mode.on)
				ctx := &fasthttp.RequestCtx{}
				benchReset(ctx, "GET", "/app.js")
				h(ctx)
				etag := string(ctx.Response.Header.Peek("ETag"))
				if etag == "" {
					b.Fatal("warm-up response has no ETag")
				}
				benchRun(b, h, 304, "GET", "/app.js", "If-None-Match", etag)
			})
			b.Run("head", func(b *testing.B) {
				benchRun(b, benchBuild(benchConfig(root), mode.on), 200, "HEAD", "/app.js")
			})
		})
	}
}

// BenchmarkPathFlow_NotFound measures a 404 for a path that does not exist.
func BenchmarkPathFlow_NotFound(b *testing.B) {
	root := benchRoot(b)
	for _, mode := range benchPathCacheModes {
		b.Run(mode.name, func(b *testing.B) {
			benchRun(b, benchBuild(benchConfig(root), mode.on), 404, "GET", "/nope/missing.js")
		})
	}
}

// BenchmarkPathFlow_HitParallel measures cache hits on 64 distinct files from
// all procs; run with -cpu 1,4,10 to see how hits scale across cores.
func BenchmarkPathFlow_HitParallel(b *testing.B) {
	root := benchRoot(b)
	const nFiles = 64
	body := strings.Repeat("x", 2048)
	uris := make([]string, nFiles)
	for i := range uris {
		name := fmt.Sprintf("f%d.js", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
		uris[i] = "/" + name
	}
	for _, mode := range benchPathCacheModes {
		b.Run(mode.name, func(b *testing.B) {
			h := benchBuild(benchConfig(root), mode.on)
			ctx := &fasthttp.RequestCtx{}
			for _, u := range uris {
				benchReset(ctx, "GET", u)
				h(ctx)
				if ctx.Response.StatusCode() != 200 {
					b.Fatalf("warm-up %s: status %d", u, ctx.Response.StatusCode())
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				ctx := &fasthttp.RequestCtx{}
				i := 0
				for pb.Next() {
					benchReset(ctx, "GET", uris[i%nFiles])
					h(ctx)
					i++
				}
			})
		})
	}
}
