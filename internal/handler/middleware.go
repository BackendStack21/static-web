package handler

import (
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// debugStackTraces controls whether full goroutine stack traces are logged on panic.
// When false (default), only the panic value is logged, preventing sensitive internal
// details from leaking. Set STATIC_DEBUG=1 to enable verbose stack traces.
// SEC-003: Configurable panic stack trace verbosity.
var debugStackTraces = os.Getenv("STATIC_DEBUG") == "1"

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
		// SEC-008: Sanitize the URI before logging to prevent control-character
		// injection (e.g. \r\n) into log files which could enable log forgery.
		uri := sanitizeForLog(string(ctx.RequestURI()))
		logger.Print(formatAccessLogLine(method, uri, status, size, duration))
	}
}

// recoveryMiddleware catches panics in the handler chain and returns a 500.
// SEC-003: Full stack traces are only logged when STATIC_DEBUG=1 is set,
// preventing sensitive internal details from leaking into production logs.
func recoveryMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		defer func() {
			if rec := recover(); rec != nil {
				if debugStackTraces {
					stack := debug.Stack()
					log.Printf("PANIC recovered: %v\n%s", rec, stack)
				} else {
					log.Printf("PANIC recovered: %v (set STATIC_DEBUG=1 for stack trace)", rec)
				}
				// Only write the header if not already sent.
				ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
			}
		}()
		next(ctx)
	}
}

// sanitizeForLog replaces ASCII control characters (0x00–0x1F, 0x7F) with
// their hex-escaped form (e.g. \x0a) to prevent log injection attacks where
// crafted URIs containing \r\n could forge log entries.
// SEC-008: Log sanitization for untrusted request data.
func sanitizeForLog(s string) string {
	// Fast path: no control characters → return as-is.
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			b.WriteString(`\x`)
			b.WriteByte("0123456789abcdef"[c>>4])
			b.WriteByte("0123456789abcdef"[c&0x0f])
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}
