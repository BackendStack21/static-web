package handler

import (
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
)

// BuildHandler composes the full middleware chain and returns a ready-to-use
// http.Handler. The chain is (outer to inner):
//
//	recovery → logging → security → compress → file handler
func BuildHandler(cfg *config.Config, c *cache.Cache) http.Handler {
	return buildHandlerWithLogger(cfg, c, false)
}

// BuildHandlerQuiet is like BuildHandler but suppresses per-request access logging.
// Use this when the --quiet flag is set.
func BuildHandlerQuiet(cfg *config.Config, c *cache.Cache) http.Handler {
	return buildHandlerWithLogger(cfg, c, true)
}

// buildHandlerWithLogger is the shared implementation. quiet=true discards access logs.
func buildHandlerWithLogger(cfg *config.Config, c *cache.Cache, quiet bool) http.Handler {
	// Core file handler.
	fileHandler := NewFileHandler(cfg, c)

	// Compression middleware (on-the-fly gzip for uncached/large files).
	compressed := compress.Middleware(&cfg.Compression, fileHandler)

	// Security middleware: path validation + security headers.
	withSecurity := security.Middleware(&cfg.Security, cfg.Files.Root, compressed)

	// Request logging (suppressed when quiet=true).
	if quiet {
		return recoveryMiddleware(withSecurity)
	}
	withLogging := loggingMiddleware(withSecurity)

	// Panic recovery (outermost).
	withRecovery := recoveryMiddleware(withLogging)

	return withRecovery
}

// statusResponseWriter wraps http.ResponseWriter to capture the written status code
// and response size for access logging.
type statusResponseWriter struct {
	http.ResponseWriter
	status int
	size   int64
}

func (srw *statusResponseWriter) WriteHeader(code int) {
	srw.status = code
	srw.ResponseWriter.WriteHeader(code)
}

func (srw *statusResponseWriter) Write(p []byte) (int, error) {
	n, err := srw.ResponseWriter.Write(p)
	srw.size += int64(n)
	return n, err
}

// Unwrap exposes the underlying ResponseWriter for middleware that performs
// type assertions (e.g. http.Flusher).
func (srw *statusResponseWriter) Unwrap() http.ResponseWriter {
	return srw.ResponseWriter
}

// srwPool pools statusResponseWriter instances to avoid per-request allocation.
var srwPool = sync.Pool{
	New: func() any { return &statusResponseWriter{} },
}

// loggingMiddleware logs each request with method, path, status, duration, and bytes
// using the standard logger.
func loggingMiddleware(next http.Handler) http.Handler {
	return loggingMiddlewareWithWriter(next, log.Default())
}

// loggingMiddlewareWithWriter is like loggingMiddleware but writes to the provided logger.
func loggingMiddlewareWithWriter(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		srw := srwPool.Get().(*statusResponseWriter)
		srw.ResponseWriter = rw
		srw.status = http.StatusOK
		srw.size = 0
		next.ServeHTTP(srw, r)
		duration := time.Since(start)
		logger.Print(formatAccessLogLine(r.Method, r.URL.RequestURI(), srw.status, srw.size, duration))
		srw.ResponseWriter = nil // release reference before returning to pool
		srwPool.Put(srw)
	})
}

func formatAccessLogLine(method, uri string, status int, size int64, duration time.Duration) string {
	buf := make([]byte, 0, len(method)+len(uri)+48)
	buf = append(buf, method...)
	buf = append(buf, ' ')
	buf = append(buf, uri...)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, int64(status), 10)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, size, 10)
	buf = append(buf, ' ')
	buf = append(buf, duration.Round(time.Microsecond).String()...)
	return string(buf)
}

// recoveryMiddleware catches panics in the handler chain and returns a 500.
// It logs the panic value and the full stack trace.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				log.Printf("PANIC recovered: %v\n%s", rec, stack)
				// Only write the header if not already sent.
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
