// Package compress provides HTTP response compression middleware and utilities.
// It supports brotli (pre-compressed only), gzip (pre-compressed + on-the-fly),
// and transparent passthrough for already-compressed content types.
package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/static-web/server/internal/config"
)

// compressibleTypes is the set of MIME types eligible for compression.
var compressibleTypes = map[string]bool{
	"text/html":              true,
	"text/css":               true,
	"text/plain":             true,
	"text/xml":               true,
	"text/javascript":        true,
	"application/javascript": true,
	"application/json":       true,
	"application/xml":        true,
	"application/wasm":       true,
	"image/svg+xml":          true,
	"font/ttf":               true,
	"font/otf":               true,
	"font/woff":              true,
	"application/font-woff":  true,
}

// gzipWriterPool pools gzip.Writers to amortise allocation costs.
var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return w
	},
}

// IsCompressible reports whether the given content type should be compressed.
func IsCompressible(contentType string) bool {
	// Strip parameters (e.g. "text/html; charset=utf-8").
	ct := contentType
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	return compressibleTypes[strings.ToLower(ct)]
}

// AcceptsEncoding reports whether the Accept-Encoding header includes enc.
// It parses the comma-separated list without allocating.
// Returns false if the encoding is explicitly rejected with q=0 (RFC 7231 §5.3.4).
func AcceptsEncoding(r *http.Request, enc string) bool {
	header := r.Header.Get("Accept-Encoding")
	if header == "" {
		return false
	}
	// Walk the comma-separated tokens without allocating.
	for {
		// Skip leading whitespace.
		i := 0
		for i < len(header) && (header[i] == ' ' || header[i] == '\t') {
			i++
		}
		header = header[i:]
		if header == "" {
			return false
		}
		// Find the end of the token (comma or end of string).
		end := strings.IndexByte(header, ',')
		var token string
		if end < 0 {
			token = header
			header = ""
		} else {
			token = header[:end]
			header = header[end+1:]
		}
		// Trim trailing whitespace from token.
		token = strings.TrimRight(token, " \t")
		// Extract optional weight parameter (e.g. "gzip;q=0.9" or "gzip;q=0").
		qValue := ""
		if semi := strings.IndexByte(token, ';'); semi >= 0 {
			qValue = strings.TrimSpace(token[semi+1:])
			token = strings.TrimRight(token[:semi], " \t")
		}
		if token == enc {
			// RFC 7231: q=0 means "not acceptable" — explicitly rejected by client.
			if strings.EqualFold(qValue, "q=0") || qValue == "q=0.0" || qValue == "q=0.00" || qValue == "q=0.000" {
				return false
			}
			return true
		}
	}
}

// responseWriter wraps http.ResponseWriter and captures the status code and
// written bytes so the gzip middleware can set correct headers.
type responseWriter struct {
	http.ResponseWriter
	gzipWriter *gzip.Writer
	statusCode int
	written    int64
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	if rw.gzipWriter != nil {
		n, err := rw.gzipWriter.Write(p)
		rw.written += int64(n)
		return n, err
	}
	n, err := rw.ResponseWriter.Write(p)
	rw.written += int64(n)
	return n, err
}

// Middleware returns an http.Handler that adds on-the-fly gzip compression
// for compressible content types when the client signals support.
// Pre-compressed serving (br/gz sidecar files) is handled in the file handler;
// this middleware only handles the on-the-fly gzip fallback for uncached or
// large files that bypass the cache.
func Middleware(cfg *config.CompressionConfig, next http.Handler) http.Handler {
	if !cfg.Enabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only compress GET and HEAD.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		// If the client doesn't accept gzip, pass through.
		if !AcceptsEncoding(r, "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		// Set Vary so proxies cache correctly.
		w.Header().Add("Vary", "Accept-Encoding")

		// We use a sniffing wrapper: only activate gzip once we know the
		// content type and size are eligible. Use a lazy-init approach via
		// a custom ResponseWriter that decides on first Write.
		lw := &lazyGzipWriter{
			ResponseWriter: w,
			request:        r,
			cfg:            cfg,
		}
		defer lw.close()

		next.ServeHTTP(lw, r)
	})
}

// lazyGzipWriter defers the gzip activation decision until the response
// headers (specifically Content-Type and Content-Length) are known.
type lazyGzipWriter struct {
	http.ResponseWriter
	request    *http.Request
	cfg        *config.CompressionConfig
	gz         *gzip.Writer
	decided    bool
	compressed bool
	statusCode int
}

func (lw *lazyGzipWriter) WriteHeader(code int) {
	// For responses with no body, forward the status code immediately without
	// deferring to decide(): 1xx, 204 No Content, and 304 Not Modified must
	// never trigger gzip activation because their bodies are forbidden by the spec.
	if code == http.StatusNotModified || code == http.StatusNoContent ||
		(code >= 100 && code < 200) {
		lw.decided = true
		lw.compressed = false
		lw.ResponseWriter.WriteHeader(code)
		return
	}
	lw.statusCode = code
	// Don't call ResponseWriter.WriteHeader yet — decide() will do it on first Write.
}

func (lw *lazyGzipWriter) Write(p []byte) (int, error) {
	if !lw.decided {
		lw.decide(len(p))
	}
	if lw.compressed && lw.gz != nil {
		return lw.gz.Write(p)
	}
	// Flush status code if not yet written.
	if lw.statusCode != 0 {
		lw.ResponseWriter.WriteHeader(lw.statusCode)
		lw.statusCode = 0
	}
	return lw.ResponseWriter.Write(p)
}

func (lw *lazyGzipWriter) decide(firstChunkSize int) {
	lw.decided = true
	ct := lw.ResponseWriter.Header().Get("Content-Type")

	// Don't compress if already encoded or not a compressible type.
	if lw.ResponseWriter.Header().Get("Content-Encoding") != "" ||
		!IsCompressible(ct) ||
		firstChunkSize < lw.cfg.MinSize {
		if lw.statusCode != 0 {
			lw.ResponseWriter.WriteHeader(lw.statusCode)
			lw.statusCode = 0
		}
		return
	}

	// Activate gzip compression.
	lw.compressed = true
	lw.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	// Remove Content-Length — it's no longer valid after compression.
	lw.ResponseWriter.Header().Del("Content-Length")

	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(lw.ResponseWriter)
	lw.gz = gz

	if lw.statusCode != 0 {
		lw.ResponseWriter.WriteHeader(lw.statusCode)
		lw.statusCode = 0
	}
}

func (lw *lazyGzipWriter) close() {
	if lw.gz != nil {
		lw.gz.Close()
		// Reset and return to pool.
		lw.gz.Reset(io.Discard)
		gzipWriterPool.Put(lw.gz)
		lw.gz = nil
	}
	// If we never wrote anything, flush any buffered status code.
	if !lw.decided && lw.statusCode != 0 {
		lw.ResponseWriter.WriteHeader(lw.statusCode)
	}
}

// GzipBytes compresses src with the configured level and returns the result.
// Used during cache population to pre-compress file contents.
func GzipBytes(src []byte, level int) ([]byte, error) {
	if level < gzip.BestSpeed || level > gzip.BestCompression {
		level = gzip.DefaultCompression
	}
	return gzipBytesLevel(src, level)
}

// gzipBytesLevel is the internal implementation of GzipBytes.
func gzipBytesLevel(src []byte, level int) ([]byte, error) {
	// Use a pre-allocated byte slice writer.
	out := make([]byte, 0, len(src)/2+512)
	bw := &byteWriter{buf: &out}

	gz, err := gzip.NewWriterLevel(bw, level)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(src); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return *bw.buf, nil
}

// byteWriter is a minimal io.Writer that appends to a byte slice.
type byteWriter struct {
	buf *[]byte
}

func (bw *byteWriter) Write(p []byte) (int, error) {
	*bw.buf = append(*bw.buf, p...)
	return len(p), nil
}
