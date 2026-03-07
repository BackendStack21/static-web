package handler

import (
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/static-web/server/internal/cache"
	"github.com/static-web/server/internal/compress"
	"github.com/static-web/server/internal/config"
	"github.com/static-web/server/internal/headers"
	"github.com/static-web/server/internal/security"
)

// BuildHandler composes the full middleware chain and returns a ready-to-use
// http.Handler. The chain is (outer to inner):
//
//	recovery → logging → security → headers (304 check) → compress → file handler
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

	// Headers middleware: 304 checks + cache-control for cached files.
	withHeaders := headers.Middleware(c, &cfg.Headers, cfg.Files.Index, compressed)

	// Security middleware: path validation + security headers.
	withSecurity := security.Middleware(&cfg.Security, cfg.Files.Root, withHeaders)

	// Request logging (suppressed when quiet=true).
	var withLogging http.Handler
	if quiet {
		withLogging = loggingMiddlewareWithWriter(withSecurity, io.Discard)
	} else {
		withLogging = loggingMiddleware(withSecurity)
	}

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
	return loggingMiddlewareWithWriter(next, nil)
}

// loggingMiddlewareWithWriter is like loggingMiddleware but writes to w.
// When w is io.Discard (or any writer that discards output), access logging is
// effectively suppressed. When w is nil, the standard logger is used.
func loggingMiddlewareWithWriter(next http.Handler, w io.Writer) http.Handler {
	var logger *log.Logger
	if w != nil {
		logger = log.New(w, "", log.LstdFlags)
	}

	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		srw := srwPool.Get().(*statusResponseWriter)
		srw.ResponseWriter = rw
		srw.status = http.StatusOK
		srw.size = 0
		next.ServeHTTP(srw, r)
		duration := time.Since(start)
		if logger != nil {
			logger.Printf("%s %s %d %d %s",
				r.Method,
				r.URL.RequestURI(),
				srw.status,
				srw.size,
				duration.Round(time.Microsecond),
			)
		} else {
			log.Printf("%s %s %d %d %s",
				r.Method,
				r.URL.RequestURI(),
				srw.status,
				srw.size,
				duration.Round(time.Microsecond),
			)
		}
		srw.ResponseWriter = nil // release reference before returning to pool
		srwPool.Put(srw)
	})
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
