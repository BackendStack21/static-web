package headers_test

import (
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/headers"
	"github.com/valyala/fasthttp"
)

// Benchmarks for the helpers that run on every cache hit (P-F3): Vary merging
// and cache-key normalisation.

func benchCachedFile() *cache.CachedFile {
	f := makeCachedFile([]byte("console.log('x');"), "application/javascript")
	f.InitHeaders()
	f.InitCacheControl("/app.js", 0, 3600, "")
	return f
}

// BenchmarkAddVary_TokenAlreadyPresent is the compress-middleware-then-handler
// case: Vary: Accept-Encoding is already set when the handler adds it again.
func BenchmarkAddVary_TokenAlreadyPresent(b *testing.B) {
	var h fasthttp.ResponseHeader
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.Reset()
		h.Set("Vary", "Accept-Encoding")
		headers.AddVary(&h, "Accept-Encoding")
	}
}

// BenchmarkAddVary_MergeNewToken is the CORS case: Origin is present and
// Accept-Encoding is merged in.
func BenchmarkAddVary_MergeNewToken(b *testing.B) {
	var h fasthttp.ResponseHeader
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.Reset()
		h.Set("Vary", "Origin")
		headers.AddVary(&h, "Accept-Encoding")
	}
}

// BenchmarkSetCacheHeaders_Hit measures the per-hit header writes for a
// cached file whose pre-formatted headers are populated.
func BenchmarkSetCacheHeaders_Hit(b *testing.B) {
	f := benchCachedFile()
	cfg := &config.HeadersConfig{EnableETags: true, StaticMaxAge: 3600}
	var ctx fasthttp.RequestCtx
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx.Response.Header.Reset()
		headers.SetCacheHeaders(&ctx, "/app.js", f, cfg)
	}
}

// BenchmarkSetCacheHeaders_HitVaryPreset is SetCacheHeaders after the
// compress middleware has already set Vary: Accept-Encoding.
func BenchmarkSetCacheHeaders_HitVaryPreset(b *testing.B) {
	f := benchCachedFile()
	cfg := &config.HeadersConfig{EnableETags: true, StaticMaxAge: 3600}
	var ctx fasthttp.RequestCtx
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx.Response.Header.Reset()
		ctx.Response.Header.Set("Vary", "Accept-Encoding")
		headers.SetCacheHeaders(&ctx, "/app.js", f, cfg)
	}
}

// BenchmarkCacheKeyForPath_CleanFile is the common already-canonical file path.
func BenchmarkCacheKeyForPath_CleanFile(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = headers.CacheKeyForPath("/assets/js/app.min.js", "index.html")
	}
}
