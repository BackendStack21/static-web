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
	"github.com/BackendStack21/static-web/internal/headers"
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
		headers.AddVary(&ctx.Response.Header, "Accept-Encoding")

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

// gzipBytesLevel is the internal implementation of GzipBytes. It compresses
// into a pooled scratch buffer with a pooled gzip.Writer (building a writer
// allocates ~1 MB of compressor state) and returns an exact-size copy, so
// the cache never retains the scratch buffer's spare capacity.
func gzipBytesLevel(src []byte, level int) ([]byte, error) {
	buf := gzipBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	_, pool := gzipPoolFor(level)
	gz := pool.Get().(*gzip.Writer)
	gz.Reset(buf)

	_, err := gz.Write(src)
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	var out []byte
	if err == nil {
		out = make([]byte, buf.Len())
		copy(out, buf.Bytes())
	}

	gz.Reset(io.Discard)
	pool.Put(gz)
	buf.Reset()
	gzipBufPool.Put(buf)
	return out, err
}

// ZstdBytes compresses src with the default zstd level and returns the result.
// Used during cache population to pre-compress file contents.
func ZstdBytes(src []byte) ([]byte, error) {
	return ZstdBytesLevel(src, zstd.SpeedDefault)
}

// zstdEncoderPools holds one pool of single-goroutine encoders per level
// (map[zstd.EncoderLevel]*sync.Pool). A pool is used rather than one shared
// *zstd.Encoder: a shared encoder keeps GOMAXPROCS encoder states alive for
// the life of the process (~18 MB each at SpeedDefault once an input spans
// more than one block), whereas idle pooled encoders are reclaimed by the GC.
// Encoders built with concurrency 1 and used only via EncodeAll start no
// goroutines, so dropping them without Close is safe.
var zstdEncoderPools sync.Map

// zstdScratchPool pools the buffers EncodeAll appends into.
var zstdScratchPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

// zstdPoolFor returns the encoder pool for level. The first encoder is built
// eagerly so an invalid level reports its error instead of creating a pool
// whose New would fail.
func zstdPoolFor(level zstd.EncoderLevel) (*sync.Pool, error) {
	if p, ok := zstdEncoderPools.Load(level); ok {
		return p.(*sync.Pool), nil
	}
	opts := []zstd.EOption{zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1)}
	first, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, err
	}
	p := &sync.Pool{
		New: func() any {
			enc, _ := zstd.NewWriter(nil, opts...) // options already validated
			return enc
		},
	}
	actual, _ := zstdEncoderPools.LoadOrStore(level, p)
	pool := actual.(*sync.Pool)
	pool.Put(first)
	return pool, nil
}

// ZstdBytesLevel compresses src with the specified zstd level and returns the result.
// The level can be zstd.SpeedFastest, zstd.SpeedDefault, zstd.SpeedBetterCompression, or zstd.SpeedBestCompression.
// Encoders and scratch space are pooled per level; the returned slice is an
// exact-size copy that shares no memory with pooled state.
func ZstdBytesLevel(src []byte, level zstd.EncoderLevel) ([]byte, error) {
	pool, err := zstdPoolFor(level)
	if err != nil {
		return nil, err
	}
	enc := pool.Get().(*zstd.Encoder)
	bp := zstdScratchPool.Get().(*[]byte)

	tmp := enc.EncodeAll(src, (*bp)[:0])
	out := make([]byte, len(tmp))
	copy(out, tmp)

	*bp = tmp[:0]
	zstdScratchPool.Put(bp)
	pool.Put(enc)
	return out, nil
}
