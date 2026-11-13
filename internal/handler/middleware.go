package handler

import (
	"errors"
	"io"
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

// Access-log buffering (P-F10).
const (
	// accessLogFlushInterval bounds how long an access-log line can wait in
	// the buffer before it is written out.
	accessLogFlushInterval = 500 * time.Millisecond
	// accessLogBufferSize is the amount of buffered output at which a write
	// is flushed at once instead of waiting for the timer.
	accessLogBufferSize = 64 << 10
)

// asyncLogWriter buffers log output in memory and writes it to dst in
// batches: when limit bytes are buffered, interval after the first unflushed
// write, or on Flush. Each batch is one dst.Write of whole lines (log.Logger
// writes one complete line per Write).
//
// A writer holds no goroutine. A time.AfterFunc timer is armed only while
// output is pending, and while it is armed the writer is listed in
// pendingAccessLogs so that FlushAccessLog can reach it.
type asyncLogWriter struct {
	mu       sync.Mutex
	dst      io.Writer
	buf      []byte
	limit    int
	interval time.Duration
	timer    *time.Timer
	armed    bool // timer pending; w is in pendingAccessLogs
}

func newAsyncLogWriter(dst io.Writer, interval time.Duration, limit int) *asyncLogWriter {
	return &asyncLogWriter{dst: dst, limit: limit, interval: interval}
}

// Write buffers p. It does not fail: an error from a write to dst that a
// full buffer triggers is dropped, as log.Logger drops write errors anyway.
func (w *asyncLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 && len(w.buf)+len(p) > w.limit {
		_ = w.flushLocked()
	}
	w.buf = append(w.buf, p...)
	if len(w.buf) >= w.limit {
		_ = w.flushLocked()
		return len(p), nil
	}
	if !w.armed {
		w.armed = true
		pendingAccessLogs.add(w)
		if w.timer == nil {
			w.timer = time.AfterFunc(w.interval, w.flushOnTimer)
		} else {
			w.timer.Reset(w.interval)
		}
	}
	return len(p), nil
}

// Flush writes out everything buffered so far.
func (w *asyncLogWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *asyncLogWriter) flushOnTimer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.flushLocked()
	w.armed = false
	pendingAccessLogs.remove(w)
}

func (w *asyncLogWriter) flushLocked() error {
	if len(w.buf) == 0 {
		return nil
	}
	_, err := w.dst.Write(w.buf)
	// The batch is dropped even when the write fails: retrying could repeat
	// lines a short write already delivered, and would let the buffer grow
	// without bound while dst stays broken.
	w.buf = w.buf[:0]
	if cap(w.buf) > 4*w.limit {
		w.buf = nil // do not keep an oversized buffer after a huge line
	}
	return err
}

// logWriterSet is the set of asyncLogWriters that have output pending.
type logWriterSet struct {
	mu sync.Mutex
	m  map[*asyncLogWriter]struct{}
}

// pendingAccessLogs lists every access-log writer with an armed flush timer.
// Lock order: asyncLogWriter.mu before logWriterSet.mu.
var pendingAccessLogs = &logWriterSet{m: make(map[*asyncLogWriter]struct{})}

func (s *logWriterSet) add(w *asyncLogWriter) {
	s.mu.Lock()
	s.m[w] = struct{}{}
	s.mu.Unlock()
}

func (s *logWriterSet) remove(w *asyncLogWriter) {
	s.mu.Lock()
	delete(s.m, w)
	s.mu.Unlock()
}

func (s *logWriterSet) snapshot() []*asyncLogWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := make([]*asyncLogWriter, 0, len(s.m))
	for w := range s.m {
		ws = append(ws, w)
	}
	return ws
}

// FlushAccessLog writes out the access-log lines that are still buffered,
// for every handler built with BuildHandler. Lines are otherwise written
// within accessLogFlushInterval (500 ms) or once 64 KiB are buffered.
//
// server.(*Server).Shutdown calls it after the connections have drained, so
// a graceful shutdown (including SIGTERM/SIGINT through
// server.RunSignalHandler) loses no lines. A program that serves a
// BuildHandler handler without that Shutdown must call FlushAccessLog before
// it exits. It is safe to call at any time and from any goroutine, and
// returns the write errors met, if any.
func FlushAccessLog() error {
	var errs []error
	for _, w := range pendingAccessLogs.snapshot() {
		if err := w.Flush(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// loggingMiddleware logs each request with method, path, status, duration, and bytes
// in the standard logger's format.
//
// P-F10: lines go through an asyncLogWriter instead of straight to the
// standard logger, which made one write(2) per request while holding its
// mutex. The destination, prefix and flags are those of the standard logger
// when the handler is built. Panic and server messages still use the
// standard logger directly and are not delayed.
func loggingMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	w := newAsyncLogWriter(log.Writer(), accessLogFlushInterval, accessLogBufferSize)
	return loggingMiddlewareWithWriter(next, log.New(w, log.Prefix(), log.Flags()))
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
		size := accessLogSize(ctx)

		method := string(ctx.Method())
		// SEC-008: Sanitize the URI before logging to prevent control-character
		// injection (e.g. \r\n) into log files which could enable log forgery.
		uri := sanitizeForLog(string(ctx.RequestURI()))
		_ = logger.Output(1, formatAccessLogLine(method, uri, status, size, duration))
	}
}

// accessLogSize returns the body size to record in the access log: the
// number of body bytes the response sends, or for HEAD the length it
// declares.
//
// A buffered body is measured directly. Content-Length is not reliable
// there: the compress middleware deletes it after gzipping on the fly, and
// SetBody does not set it, so ContentLength() reads 0. A body stream is never
// read here, because Response.Body() would drain the whole stream into
// memory; its declared Content-Length is used instead, or 0 when unknown
// (chunked).
func accessLogSize(ctx *fasthttp.RequestCtx) int64 {
	resp := &ctx.Response
	if !resp.IsBodyStream() {
		if n := len(resp.Body()); n > 0 || !ctx.IsHead() {
			return int64(n)
		}
	}
	if n := resp.Header.ContentLength(); n > 0 {
		return int64(n)
	}
	return 0
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
				// writeError, not ctx.Error: keep the security and CORS
				// headers on the 500 (HTTP-F4).
				writeError(ctx, "Internal Server Error", fasthttp.StatusInternalServerError)
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
