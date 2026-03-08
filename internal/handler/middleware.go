package handler

import (
	"log"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// BuildHandler composes the full middleware chain and returns a ready-to-use
// fasthttp.RequestHandler. The chain is (outer to inner):
//
//	recovery → logging → security → compress → file handler
//
// An optional *security.PathCache may be provided to cache path validation
// results and skip per-request filesystem syscalls for repeated URL paths.
func BuildHandler(cfg *config.Config, c *cache.Cache, pc ...*security.PathCache) fasthttp.RequestHandler {
	var pathCache *security.PathCache
	if len(pc) > 0 {
		pathCache = pc[0]
	}
	return buildHandlerWithLogger(cfg, c, false, pathCache)
}

// BuildHandlerQuiet is like BuildHandler but suppresses per-request access logging.
// Use this when the --quiet flag is set.
func BuildHandlerQuiet(cfg *config.Config, c *cache.Cache, pc ...*security.PathCache) fasthttp.RequestHandler {
	var pathCache *security.PathCache
	if len(pc) > 0 {
		pathCache = pc[0]
	}
	return buildHandlerWithLogger(cfg, c, true, pathCache)
}

// buildHandlerWithLogger is the shared implementation. quiet=true discards access logs.
func buildHandlerWithLogger(cfg *config.Config, c *cache.Cache, quiet bool, pathCache *security.PathCache) fasthttp.RequestHandler {
	// Core file handler — pass PathCache for zero-alloc path lookup (PERF-001).
	fileHandler := NewFileHandler(cfg, c, pathCache)

	// Compression middleware (on-the-fly gzip for uncached/large files).
	compressed := compress.Middleware(&cfg.Compression, fileHandler.HandleRequest)

	// Security middleware: path validation + security headers.
	withSecurity := security.Middleware(&cfg.Security, cfg.Files.Root, compressed, pathCache)

	// Request logging (suppressed when quiet=true).
	if quiet {
		return recoveryMiddleware(withSecurity)
	}
	withLogging := loggingMiddleware(withSecurity)

	// Panic recovery (outermost).
	withRecovery := recoveryMiddleware(withLogging)

	return withRecovery
}

// logBufPool pools byte buffers for access log line formatting (PERF-008).
var logBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 128)
		return &b
	},
}

func formatAccessLogLine(method, uri string, status int, size int64, duration time.Duration) string {
	bp := logBufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = append(buf, method...)
	buf = append(buf, ' ')
	buf = append(buf, uri...)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, int64(status), 10)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, size, 10)
	buf = append(buf, ' ')
	buf = append(buf, duration.Round(time.Microsecond).String()...)
	s := string(buf)
	*bp = buf
	logBufPool.Put(bp)
	return s
}

// loggingMiddleware logs each request with method, path, status, duration, and bytes
// using the standard logger.
func loggingMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return loggingMiddlewareWithWriter(next, log.Default())
}

// loggingMiddlewareWithWriter is like loggingMiddleware but writes to the provided logger.
func loggingMiddlewareWithWriter(next fasthttp.RequestHandler, logger *log.Logger) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		start := time.Now()
		next(ctx)
		duration := time.Since(start)

		// With fasthttp, status and body size are available directly from the
		// response after the handler has run — no wrapper needed.
		status := ctx.Response.StatusCode()
		size := int64(ctx.Response.Header.ContentLength())
		if size < 0 {
			// ContentLength() returns -1 when chunked/unknown; use body length.
			size = int64(len(ctx.Response.Body()))
		}

		method := string(ctx.Method())
		uri := string(ctx.RequestURI())
		logger.Print(formatAccessLogLine(method, uri, status, size, duration))
	}
}

// recoveryMiddleware catches panics in the handler chain and returns a 500.
// It logs the panic value and the full stack trace.
func recoveryMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				log.Printf("PANIC recovered: %v\n%s", rec, stack)
				// Only write the header if not already sent.
				ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
			}
		}()
		next(ctx)
	}
}
