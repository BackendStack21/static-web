package handler_test

import (
	"fmt"
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

// Cache-hit benchmarks for the full middleware chain (P-F3 Vary merging and
// P-F10 access logging). They mirror the 2026-11 review harness.

const hpbAcceptEncoding = "gzip, deflate, br, zstd"

func hpbConfig(root string) *config.Config {
	cfg := &config.Config{}
	cfg.Files.Root, cfg.Files.Index, cfg.Files.MaxServeFileSize = root, "index.html", 1<<30
	cfg.Cache.Enabled, cfg.Cache.MaxBytes, cfg.Cache.MaxFileSize = true, 256<<20, 10<<20
	cfg.Compression.Enabled, cfg.Compression.MinSize, cfg.Compression.Level, cfg.Compression.Precompressed = true, 1024, 5, true
	cfg.Security.BlockDotfiles = true
	cfg.Security.CSP = "default-src 'self'"
	cfg.Security.ReferrerPolicy = "strict-origin-when-cross-origin"
	cfg.Security.PermissionsPolicy = "geolocation=(), microphone=(), camera=()"
	cfg.Headers.StaticMaxAge, cfg.Headers.EnableETags = 3600, true
	return cfg
}

// hpbRoot creates a root with app.js (~11 KB) and 64 small f<i>.js files.
func hpbRoot(b *testing.B) string {
	b.Helper()
	root := b.TempDir()
	js := strings.Repeat("console.log('hello world, this is a benchmark asset');\n", 200)
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte(js), 0o644); err != nil {
		b.Fatal(err)
	}
	small := strings.Repeat("var a = 1;\n", 190) // ~2 KB
	for i := 0; i < 64; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.js", i)), []byte(small), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return root
}

// hpbDevNullLog sends the standard logger to an os.DevNull *os.File for the
// rest of the benchmark, so that access-log writes cost a real write(2).
func hpbDevNullLog(b *testing.B) {
	b.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	prev := log.Writer()
	log.SetOutput(f)
	b.Cleanup(func() {
		hpbFlushLog()
		log.SetOutput(prev)
		_ = f.Close()
	})
}

func hpbReset(ctx *fasthttp.RequestCtx, method, uri string, hdrs ...string) {
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.ResetUserValues()
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	for i := 0; i+1 < len(hdrs); i += 2 {
		ctx.Request.Header.Set(hdrs[i], hdrs[i+1])
	}
}

func hpbRun(b *testing.B, h fasthttp.RequestHandler, want int, method, uri string, hdrs ...string) {
	b.Helper()
	ctx := &fasthttp.RequestCtx{}
	for i := 0; i < 3; i++ {
		hpbReset(ctx, method, uri, hdrs...)
		h(ctx)
	}
	if ctx.Response.StatusCode() != want {
		b.Fatalf("status %d, want %d", ctx.Response.StatusCode(), want)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hpbReset(ctx, method, uri, hdrs...)
		h(ctx)
	}
}

// BenchmarkHitPath measures cached hits through the quiet chain with and
// without a PathCache.
func BenchmarkHitPath(b *testing.B) {
	for _, withPC := range []bool{true, false} {
		name := "NoPathCache"
		if withPC {
			name = "PathCache"
		}
		b.Run(name, func(b *testing.B) {
			root := hpbRoot(b)
			cfg := hpbConfig(root)
			cfg.Security.CORSOrigins = []string{"https://a.example", "https://b.example"}
			var h fasthttp.RequestHandler
			if withPC {
				h = handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes), security.NewPathCache(0))
			} else {
				h = handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
			}
			b.Run("identity", func(b *testing.B) { hpbRun(b, h, 200, "GET", "/app.js") })
			b.Run("encoded", func(b *testing.B) { hpbRun(b, h, 200, "GET", "/app.js", "Accept-Encoding", hpbAcceptEncoding) })
			b.Run("cors", func(b *testing.B) {
				hpbRun(b, h, 200, "GET", "/app.js", "Accept-Encoding", hpbAcceptEncoding, "Origin", "https://b.example")
			})
		})
	}
}

// BenchmarkAccessLogHit measures a cached hit with and without access
// logging, the log going to an os.DevNull file (P-F10).
func BenchmarkAccessLogHit(b *testing.B) {
	for _, quiet := range []bool{true, false} {
		b.Run(fmt.Sprintf("quiet=%v", quiet), func(b *testing.B) {
			hpbDevNullLog(b)
			root := hpbRoot(b)
			cfg := hpbConfig(root)
			c := cache.NewCache(cfg.Cache.MaxBytes)
			pc := security.NewPathCache(0)
			var h fasthttp.RequestHandler
			if quiet {
				h = handler.BuildHandlerQuiet(cfg, c, pc)
			} else {
				h = handler.BuildHandler(cfg, c, pc)
			}
			b.Run("serial", func(b *testing.B) { hpbRun(b, h, 200, "GET", "/app.js") })
			b.Run("parallel", func(b *testing.B) {
				ctx := &fasthttp.RequestCtx{}
				for i := 0; i < 64; i++ {
					hpbReset(ctx, "GET", fmt.Sprintf("/f%d.js", i))
					h(ctx)
				}
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					ctx := &fasthttp.RequestCtx{}
					i := 0
					for pb.Next() {
						hpbReset(ctx, "GET", hpbSmallPaths[i&63])
						h(ctx)
						i++
					}
				})
			})
		})
	}
}

var hpbSmallPaths = func() (s [64]string) {
	for i := range s {
		s[i] = fmt.Sprintf("/f%d.js", i)
	}
	return
}()

// hpbFlushLog flushes buffered access-log lines before the log output is
// restored and the os.DevNull file closed.
func hpbFlushLog() { _ = handler.FlushAccessLog() }
