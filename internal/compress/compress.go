// Package compress provides HTTP response compression middleware and utilities.
// It supports brotli (pre-compressed only), gzip (pre-compressed + on-the-fly),
// and transparent passthrough for already-compressed content types.
package compress

import (
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/fasthttp"
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

// gzipWriterPools holds one gzip.Writer pool per compression level so the
// configured level is honoured while still amortising allocations.
var gzipWriterPools sync.Map // map[int]*sync.Pool

// gzipPoolFor returns the writer pool for level (invalid levels map to default).
func gzipPoolFor(level int) (int, *sync.Pool) {
	if level < gzip.BestSpeed || level > gzip.BestCompression {
		level = gzip.DefaultCompression
	}
	if p, ok := gzipWriterPools.Load(level); ok {
		return level, p.(*sync.Pool)
	}
	p, _ := gzipWriterPools.LoadOrStore(level, &sync.Pool{
		New: func() any {
			w, _ := gzip.NewWriterLevel(io.Discard, level)
			return w
		},
	})
	return level, p.(*sync.Pool)
}

// gzipBufPool pools bytes.Buffers used for on-the-fly gzip output.
var gzipBufPool = sync.Pool{
	New: func() any {
		return &bytes.Buffer{}
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
func AcceptsEncoding(ctx *fasthttp.RequestCtx, enc string) bool {
	header := string(ctx.Request.Header.Peek("Accept-Encoding"))
	if header == "" {
		return false
	}
	return AcceptsEncodingStr(header, enc)
}

// AcceptsEncodingStr reports whether the given Accept-Encoding header value
// includes enc. It parses the comma-separated list without allocating.
// Returns false if the encoding is explicitly rejected with q=0 (RFC 7231 §5.3.4).
func AcceptsEncodingStr(header, enc string) bool {
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
		// Extract optional weight parameter (e.g. "gzip;q=0.9" or "gzip ; q=0").
		params := ""
		if semi := strings.IndexByte(token, ';'); semi >= 0 {
			params = token[semi+1:]
			token = strings.TrimRight(token[:semi], " \t")
		}
		if strings.EqualFold(token, enc) {
			// RFC 9110 §12.5.3: q=0 means "not acceptable".
			return qValueOf(params) > 0
		}
	}
}

// qValueOf returns the numeric q weight in a parameter list (default 1).
// An unparsable q value is treated as 1 (lenient).
func qValueOf(params string) float64 {
	for params != "" {
		var p string
		if i := strings.IndexByte(params, ';'); i >= 0 {
			p, params = params[:i], params[i+1:]
		} else {
			p, params = params, ""
		}
		eq := strings.IndexByte(p, '=')
		if eq < 0 || !strings.EqualFold(strings.TrimSpace(p[:eq]), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(p[eq+1:]), 64)
		if err != nil {
			return 1
		}
		return q
	}
	return 1
}

// Middleware returns a fasthttp.RequestHandler that adds on-the-fly gzip
// compression for compressible content types when the client signals support.
// Pre-compressed serving (br/gz sidecar files) is handled in the file handler;
// this middleware only handles the on-the-fly gzip fallback for uncached or
// large files that bypass the cache.
//
// With fasthttp, the response body is fully buffered, so we apply compression
// as a post-processing step after the inner handler writes the response.
func Middleware(cfg *config.CompressionConfig, next fasthttp.RequestHandler) fasthttp.RequestHandler {
	if !cfg.Enabled {
		return next
	}

	return func(ctx *fasthttp.RequestCtx) {
		// Only compress GET and HEAD.
		if !ctx.IsGet() && !ctx.IsHead() {
			next(ctx)
			return
		}

		// If the client doesn't accept gzip, pass through.
		if !AcceptsEncoding(ctx, "gzip") {
			next(ctx)
			return
		}

		// Set Vary so proxies cache correctly.
		ctx.Response.Header.Add("Vary", "Accept-Encoding")

		// Call the inner handler — it writes the response body to ctx.
		next(ctx)

		// Post-process: decide whether to compress the response body.
		statusCode := ctx.Response.StatusCode()

		// For responses with no body, skip compression entirely:
		// 1xx, 204 No Content, and 304 Not Modified.
		if statusCode == fasthttp.StatusNotModified || statusCode == fasthttp.StatusNoContent ||
			(statusCode >= 100 && statusCode < 200) {
			return
		}

		// Only complete (200) responses are compressed: 206 and anything
		// carrying Content-Range describes identity bytes (RFC 9110 §15.3.7).
		if statusCode != fasthttp.StatusOK || len(ctx.Response.Header.Peek("Content-Range")) > 0 {
			return
		}

		// Never buffer streamed bodies.
		if ctx.Response.IsBodyStream() {
			return
		}

		// Don't compress if already encoded.
		if len(ctx.Response.Header.Peek("Content-Encoding")) > 0 {
			return
		}

		ct := string(ctx.Response.Header.Peek("Content-Type"))
		if !IsCompressible(ct) {
			return
		}

		body := ctx.Response.Body()
		if len(body) < cfg.MinSize {
			return
		}

		// SEC-005: Skip on-the-fly compression for bodies that exceed the
		// configured maximum. This prevents excessive memory allocation and
		// CPU usage from compressing very large responses in-flight.
		if cfg.MaxCompressSize > 0 && len(body) > cfg.MaxCompressSize {
			return
		}

		// Compress the body using pooled gzip.Writer and bytes.Buffer.
		buf := gzipBufPool.Get().(*bytes.Buffer)
		buf.Reset()
		buf.Grow(len(body) / 2)

		_, pool := gzipPoolFor(cfg.Level)
		gz := pool.Get().(*gzip.Writer)
		gz.Reset(buf)
		gz.Write(body) //nolint:errcheck
		gz.Close()

		ctx.Response.Header.Set("Content-Encoding", "gzip")
		ctx.Response.Header.Del("Content-Length")
		ctx.SetBody(buf.Bytes())

		// Reset and return to pools.
		gz.Reset(io.Discard)
		pool.Put(gz)
		buf.Reset()
		gzipBufPool.Put(buf)
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

// ZstdBytes compresses src with the default zstd level and returns the result.
// Used during cache population to pre-compress file contents.
func ZstdBytes(src []byte) ([]byte, error) {
	return ZstdBytesLevel(src, zstd.SpeedDefault)
}

// ZstdBytesLevel compresses src with the specified zstd level and returns the result.
// The level can be zstd.SpeedFastest, zstd.SpeedDefault, zstd.SpeedBetter, or zstd.SpeedBest.
func ZstdBytesLevel(src []byte, level zstd.EncoderLevel) ([]byte, error) {
	out := make([]byte, 0, len(src)/2+512)
	bw := &byteWriter{buf: &out}

	w, err := zstd.NewWriter(bw, zstd.WithEncoderLevel(level))
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return *bw.buf, nil
}
