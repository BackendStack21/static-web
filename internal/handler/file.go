// Package handler provides the core HTTP file-serving handler and middleware
// composition for the static web server.
package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/defaults"
	"github.com/BackendStack21/static-web/internal/headers"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// FileHandler serves static files from disk with caching and compression support.
type FileHandler struct {
	cfg                 *config.Config
	cache               *cache.Cache
	pathCache           *security.PathCache // optional, for zero-alloc path lookup (PERF-001)
	absRoot             string              // resolved once at construction time
	notFoundData        []byte
	notFoundContentType string
}

// NewFileHandler creates a new FileHandler.
// absRoot is resolved via filepath.Abs so per-request path arithmetic is free
// of OS syscalls. An optional *PathCache allows cache-hit requests to resolve
// the safe filesystem path without a context allocation (PERF-001).
func NewFileHandler(cfg *config.Config, c *cache.Cache, pc ...*security.PathCache) *FileHandler {
	absRoot, err := filepath.Abs(cfg.Files.Root)
	if err != nil {
		// Fall back to the raw root; PathSafe will catch any traversal attempts.
		absRoot = cfg.Files.Root
	}

	var pathCache *security.PathCache
	if len(pc) > 0 {
		pathCache = pc[0]
	}

	h := &FileHandler{cfg: cfg, cache: c, pathCache: pathCache, absRoot: absRoot}
	h.notFoundData, h.notFoundContentType = h.loadCustomNotFoundPage()
	return h
}

// HandleRequest handles a fasthttp request by resolving and serving the requested file.
func (h *FileHandler) HandleRequest(ctx *fasthttp.RequestCtx) {
	urlPath := string(ctx.Path())

	// PERF-001: prefer PathCache lookup (zero-alloc) over context value
	// (which requires SetUserValue per request).
	var absPath string
	var ok bool
	if h.pathCache != nil {
		absPath, ok = h.pathCache.Lookup(urlPath)
	}
	if !ok {
		// Fallback: try UserValue (for chains without PathCache) or recompute.
		absPath, ok = security.SafePathFromCtx(ctx)
	}
	if !ok {
		// Final fallback for tests or deployments that bypass security.Middleware.
		var err error
		absPath, err = security.PathSafe(urlPath, h.absRoot, h.cfg.Security.BlockDotfiles)
		if err != nil {
			h.handleSecurityError(ctx, err)
			return
		}
	}
	// Fast path: for a plain file URL that is already cached, skip os.Stat
	// entirely. os.Stat is deferred to the cache-miss branch below.
	cacheKey := headers.CacheKeyForPath(urlPath, h.cfg.Files.Index)
	if h.cfg.Cache.Enabled && h.cache != nil {
		if cached, ok := h.cache.Get(cacheKey); ok {
			if headers.CheckNotModified(ctx, cached, h.cfg.Headers.EnableETags) {
				return
			}
			h.serveFromCache(ctx, cacheKey, cached)
			return
		}
	}

	// Cache miss — determine whether this is a directory request (needs index
	// resolution) only now, when we actually need to hit the filesystem.
	resolvedPath, canonicalURL, info, statErr, serveDirList := h.resolveIndexPath(absPath, urlPath)

	// If the path is a directory and directory listing is enabled, serve the
	// listing immediately (skip index resolution and the cache lookup below).
	if serveDirList {
		h.serveDirectoryListing(ctx, absPath, urlPath)
		return
	}

	// Re-check cache with the canonical URL (e.g. "/subdir/index.html") in
	// case the directory-resolved key is cached even though the bare path isn't.
	if h.cfg.Cache.Enabled && h.cache != nil && canonicalURL != cacheKey {
		if cached, ok := h.cache.Get(canonicalURL); ok {
			if headers.CheckNotModified(ctx, cached, h.cfg.Headers.EnableETags) {
				return
			}
			h.serveFromCache(ctx, canonicalURL, cached)
			return
		}
	}

	// True cache miss — read from disk.
	h.serveFromDisk(ctx, resolvedPath, canonicalURL, info, statErr)
}

// resolveIndexPath maps a directory path to its index file and reuses stat
// results so the caller can avoid a second os.Stat on the cold-miss path.
// When directory listing is enabled for a directory path it returns
// serveDirList=true and the caller should invoke serveDirectoryListing.
func (h *FileHandler) resolveIndexPath(absPath, urlPath string) (resolvedPath, canonicalURL string, info os.FileInfo, statErr error, serveDirList bool) {
	info, err := os.Stat(absPath)
	if err != nil {
		return absPath, urlPath, nil, err, false
	}
	if info.IsDir() {
		// Directory listing takes precedence over index resolution when enabled.
		if h.cfg.Security.DirectoryListing {
			return "", "", info, nil, true
		}
		indexFile := h.cfg.Files.Index
		if indexFile == "" {
			indexFile = "index.html"
		}
		indexPath := filepath.Join(absPath, indexFile)
		indexInfo, indexErr := os.Stat(indexPath)
		return indexPath, strings.TrimRight(urlPath, "/") + "/" + indexFile, indexInfo, indexErr, false
	}
	return absPath, urlPath, info, nil, false
}

// serveFromCache writes a cached file to the response, respecting Accept-Encoding.
//
// For ordinary GET requests (no Range header) it uses a direct SetBody() path
// that avoids the overhead of Range parsing and content-type sniffing — all
// unnecessary when the file is already fully in memory and we've handled 304s
// ourselves.
//
// Range requests are handled with a custom implementation for correct 206
// Partial Content support.
func (h *FileHandler) serveFromCache(ctx *fasthttp.RequestCtx, urlPath string, f *cache.CachedFile) {
	ctx.Response.Header.Set("X-Cache", "HIT")

	// Set ETag and Cache-Control headers.
	headers.SetFileHeaders(ctx, urlPath, f, &h.cfg.Headers)

	// Negotiate content encoding using pre-compressed variants.
	data, encoding := h.negotiateEncoding(ctx, f)

	if encoding != "" {
		ctx.Response.Header.Set("Content-Encoding", encoding)
		ctx.Response.Header.Add("Vary", "Accept-Encoding")
	}

	// --- Fast path: non-Range GET/HEAD ----------------------------------
	rangeHeader := string(ctx.Request.Header.Peek("Range"))
	if rangeHeader == "" {
		if f.CTHeader != "" {
			ctx.Response.Header.Set("Content-Type", f.CTHeader)
		} else {
			ctx.Response.Header.Set("Content-Type", f.ContentType)
		}

		if ctx.IsHead() {
			if f.CLHeader != "" {
				ctx.Response.Header.Set("Content-Length", f.CLHeader)
			} else {
				ctx.Response.Header.Set("Content-Length", strconv.FormatInt(f.Size, 10))
			}
			ctx.SetStatusCode(fasthttp.StatusOK)
			return
		}

		// For compressed data the Content-Length must reflect the encoded
		// size, not the original — compute it only when encoding differs.
		if encoding != "" {
			ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(data)))
		} else if f.CLHeader != "" {
			ctx.Response.Header.Set("Content-Length", f.CLHeader)
		} else {
			ctx.Response.Header.Set("Content-Length", strconv.FormatInt(f.Size, 10))
		}

		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBody(data)
		return
	}

	// --- Slow path: Range requests --------------------------------------
	// Content-Type must be set before serving.
	ctx.Response.Header.Set("Content-Type", f.ContentType)

	// For range requests with compressed data we must serve the raw bytes
	// because byte-range offsets apply to the uncompressed content.
	if encoding != "" {
		// Remove the Content-Encoding we set above — Range semantics
		// require uncompressed data.
		ctx.Response.Header.Del("Content-Encoding")
		data = f.Data
	}

	serveRange(ctx, data, rangeHeader)
}

// negotiateEncoding selects the best pre-compressed variant for the client.
func (h *FileHandler) negotiateEncoding(ctx *fasthttp.RequestCtx, f *cache.CachedFile) ([]byte, string) {
	if !h.cfg.Compression.Enabled {
		return f.Data, ""
	}

	// Brotli preferred (best compression), then zstd (fastest decompression),
	// then gzip (universally supported fallback).
	if f.BrData != nil && compress.AcceptsEncoding(ctx, "br") {
		return f.BrData, "br"
	}
	if f.ZstdData != nil && compress.AcceptsEncoding(ctx, "zstd") {
		return f.ZstdData, "zstd"
	}
	if f.GzipData != nil && compress.AcceptsEncoding(ctx, "gzip") {
		return f.GzipData, "gzip"
	}
	return f.Data, ""
}

// serveFromDisk reads the file from disk, populates the cache, and serves it.
// If the file does not exist on disk, it falls back to the embedded default
// assets (index.html, 404.html, style.css) before returning a 404.
func (h *FileHandler) serveFromDisk(ctx *fasthttp.RequestCtx, absPath, urlPath string, info os.FileInfo, statErr error) {
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// Try the embedded fallback assets before giving up.
			if h.serveEmbedded(ctx, urlPath) {
				return
			}
			h.serveNotFound(ctx)
			return
		}
		if os.IsPermission(statErr) {
			log.Printf("handler: permission denied accessing %q", absPath)
			ctx.Error("Forbidden", fasthttp.StatusForbidden)
			return
		}
		ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
		return
	}

	// For large files, bypass cache and serve directly.
	if info.Size() > h.cfg.Cache.MaxFileSize {
		h.serveLargeFile(ctx, absPath, urlPath, info)
		return
	}

	// Read file content.
	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsPermission(err) {
			log.Printf("handler: permission denied reading %q", absPath)
			ctx.Error("Forbidden", fasthttp.StatusForbidden)
			return
		}
		log.Printf("handler: error reading %q: %v", absPath, err)
		ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
		return
	}

	ct := detectContentType(absPath, data)
	etag := computeETag(data)

	cached := &cache.CachedFile{
		Data:         data,
		ETag:         etag,
		ETagFull:     `W/"` + etag + `"`,
		LastModified: info.ModTime(),
		ContentType:  ct,
		Size:         info.Size(),
	}

	// Load pre-compressed sidecar files only for files that are actually
	// compressible and large enough to benefit from compression.
	if h.cfg.Compression.Enabled && h.cfg.Compression.Precompressed &&
		compress.IsCompressible(ct) && len(data) >= h.cfg.Compression.MinSize {
		cached.GzipData = h.loadSidecar(absPath + ".gz")
		cached.BrData = h.loadSidecar(absPath + ".br")
		cached.ZstdData = h.loadSidecar(absPath + ".zst")
	}

	// Generate on-the-fly gzip if no sidecar and content is compressible.
	if cached.GzipData == nil && h.cfg.Compression.Enabled &&
		compress.IsCompressible(ct) && len(data) >= h.cfg.Compression.MinSize {
		if gz, err := compress.GzipBytes(data, h.cfg.Compression.Level); err == nil {
			cached.GzipData = gz
		}
	}

	// Generate on-the-fly zstd if no sidecar and content is compressible.
	if cached.ZstdData == nil && h.cfg.Compression.Enabled &&
		compress.IsCompressible(ct) && len(data) >= h.cfg.Compression.MinSize {
		if zst, err := compress.ZstdBytes(data); err == nil {
			cached.ZstdData = zst
		}
	}

	// Pre-format headers for the fast serving path.
	cached.InitHeaders()
	cached.InitCacheControl(urlPath, h.cfg.Headers.HTMLMaxAge, h.cfg.Headers.StaticMaxAge, h.cfg.Headers.ImmutablePattern)

	// Store in cache.
	if h.cfg.Cache.Enabled && h.cache != nil {
		h.cache.Put(urlPath, cached)
	}

	ctx.Response.Header.Set("X-Cache", "MISS")
	h.serveFromCache(ctx, urlPath, cached)
}

// serveLargeFile serves a file that exceeds MaxFileSize directly from disk,
// bypassing the in-memory cache but still supporting Range requests.
// The file is read into memory to avoid issues with fasthttp's lazy body
// evaluation closing the file descriptor before the body is consumed.
func (h *FileHandler) serveLargeFile(ctx *fasthttp.RequestCtx, absPath, urlPath string, info os.FileInfo) {
	f, err := os.Open(absPath)
	if err != nil {
		if os.IsPermission(err) {
			log.Printf("handler: permission denied opening %q", absPath)
			ctx.Error("Forbidden", fasthttp.StatusForbidden)
			return
		}
		log.Printf("handler: error opening large file %q: %v", absPath, err)
		ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
		return
	}

	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		log.Printf("handler: error reading large file %q: %v", absPath, err)
		ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
		return
	}

	ct := detectContentType(absPath, data)
	ctx.Response.Header.Set("Content-Type", ct)
	ctx.Response.Header.Set("X-Cache", "MISS")
	ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(cache.HTTPTimeFormat))
	ctx.Response.Header.Set("Accept-Ranges", "bytes")

	rangeHeader := string(ctx.Request.Header.Peek("Range"))
	if rangeHeader == "" {
		// No range — serve the full file.
		ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(data)))
		ctx.SetStatusCode(fasthttp.StatusOK)
		if !ctx.IsHead() {
			ctx.SetBody(data)
		}
		return
	}

	// Parse and serve the range for large files.
	serveLargeFileRange(ctx, data, int64(len(data)), rangeHeader)
}

// serveEmbedded attempts to serve a file from the embedded default assets.
// It maps the request URL path to a "public/<filename>" key in defaults.FS.
// Only the base filename is considered (no sub-directory traversal), so this
// only matches the three known defaults: index.html, 404.html, style.css.
// Returns true if the response was written, false otherwise.
func (h *FileHandler) serveEmbedded(ctx *fasthttp.RequestCtx, urlPath string) bool {
	name := strings.TrimLeft(urlPath, "/")
	if name == "" {
		name = "index.html"
	}
	// Guard: only serve flat filenames, never sub-paths.
	if strings.ContainsRune(name, '/') {
		return false
	}
	data, err := fs.ReadFile(defaults.FS, "public/"+name)
	if err != nil {
		return false
	}
	ct := detectContentType(name, data)
	etag := computeETag(data)

	ctx.Response.Header.Set("Content-Type", ct)
	ctx.Response.Header.Set("X-Cache", "MISS")

	// Set ETag and Cache-Control headers for embedded assets.
	if h.cfg.Headers.EnableETags {
		ctx.Response.Header.Set("ETag", `W/"`+etag+`"`)
	}
	ctx.Response.Header.Set("Cache-Control", "public, max-age="+strconv.Itoa(h.cfg.Headers.StaticMaxAge))
	ctx.Response.Header.Add("Vary", "Accept-Encoding")

	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody(data)
	return true
}

// serveNotFound serves a custom 404 page if configured, then falls back to the
// embedded default 404.html, and finally to a plain-text 404 response.
// The configured path is validated via PathSafe to prevent path traversal through
// a malicious config value (e.g. STATIC_FILES_NOT_FOUND=../../etc/passwd).
func (h *FileHandler) serveNotFound(ctx *fasthttp.RequestCtx) {
	if h.notFoundData != nil {
		ctx.Response.Header.Set("Content-Type", h.notFoundContentType)
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		ctx.SetBody(h.notFoundData)
		return
	}

	// Fall back to the embedded default 404.html.
	if data, err := fs.ReadFile(defaults.FS, "public/404.html"); err == nil {
		ctx.Response.Header.Set("Content-Type", "text/html; charset=utf-8")

		// Set ETag for embedded 404 page.
		if h.cfg.Headers.EnableETags {
			etag := computeETag(data)
			ctx.Response.Header.Set("ETag", `W/"`+etag+`"`)
		}

		ctx.SetStatusCode(fasthttp.StatusNotFound)
		ctx.SetBody(data)
		return
	}

	ctx.Error("404 Not Found", fasthttp.StatusNotFound)
}

func (h *FileHandler) loadCustomNotFoundPage() ([]byte, string) {
	if h.cfg.Files.NotFound == "" {
		return nil, ""
	}
	safeNotFound, err := security.PathSafe(h.cfg.Files.NotFound, h.absRoot, false)
	if err != nil {
		return nil, ""
	}
	data, err := os.ReadFile(safeNotFound)
	if err != nil {
		return nil, ""
	}
	ct := detectContentType(safeNotFound, data)
	if ct == "application/octet-stream" {
		ct = "text/html; charset=utf-8"
	}
	return data, ct
}

// handleSecurityError maps security sentinel errors to HTTP responses.
func (h *FileHandler) handleSecurityError(ctx *fasthttp.RequestCtx, err error) {
	switch {
	case errors.Is(err, security.ErrNullByte):
		ctx.Error("Bad Request: "+err.Error(), fasthttp.StatusBadRequest)
	case errors.Is(err, security.ErrPathTraversal), errors.Is(err, security.ErrDotfile):
		ctx.Error("Forbidden: "+err.Error(), fasthttp.StatusForbidden)
	default:
		ctx.Error("Forbidden", fasthttp.StatusForbidden)
	}
}

// computeETag returns the first 16 hex characters of sha256(data).
// Uses hex.EncodeToString on the first 8 bytes instead of fmt.Sprintf
// to avoid formatting the full 32-byte hash and then truncating (PERF-004).
func computeETag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// detectContentType determines the MIME type of a file by extension, falling back
// to http.DetectContentType for unknown extensions.
// NOTE: net/http is imported solely for http.DetectContentType, which is a
// standalone content-sniffing utility not related to HTTP request handling.
func detectContentType(path string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	if data != nil {
		snippet := data
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return http.DetectContentType(snippet)
	}
	return "application/octet-stream"
}

// validateSidecarPath validates that a sidecar file path is within the root directory.
// It resolves symlinks to prevent escape attacks and ensures the canonical path
// remains within the root. Returns the validated path or an error if validation fails.
// This function is designed to be recognized by static analyzers as a path sanitizer.
func (h *FileHandler) validateSidecarPath(sidecarPath string) (string, error) {
	// Resolve symlinks to get the canonical path.
	// This prevents symlink escape attacks where a sidecar could point outside root.
	realPath, err := filepath.EvalSymlinks(sidecarPath)
	if err != nil {
		// File doesn't exist or can't be resolved — return error.
		return "", err
	}

	// Resolve the root directory to its canonical path for comparison.
	// This is important on platforms like macOS where /tmp → /private/tmp.
	realRoot := h.absRoot
	if r, err := filepath.EvalSymlinks(h.absRoot); err == nil {
		realRoot = r
	}

	// Ensure the resolved sidecar path is still within the root directory.
	// Add a trailing separator to prevent prefix collisions like "/root" matching "/rootsuffix".
	rootWithSep := realRoot
	if !strings.HasSuffix(rootWithSep, string(filepath.Separator)) {
		rootWithSep += string(filepath.Separator)
	}

	// Reject if the sidecar path escapes the root directory.
	if realPath != realRoot && !strings.HasPrefix(realPath, rootWithSep) {
		// Sidecar path escapes the root — reject it.
		return "", fmt.Errorf("sidecar path escapes root directory")
	}

	return realPath, nil
}

// loadSidecar attempts to read a pre-compressed sidecar file.
// Returns nil if the sidecar does not exist, cannot be read, or fails validation.
// The path parameter must be constructed from a validated absolute filesystem path
// (e.g., absPath + ".gz") to ensure it remains within the root directory.
func (h *FileHandler) loadSidecar(path string) []byte {
	// Validate the sidecar path to prevent path traversal attacks.
	validatedPath, err := h.validateSidecarPath(path)
	if err != nil {
		// Validation failed (symlink escape, doesn't exist, etc.) — return nil.
		return nil
	}

	// Path is validated and safe — read the file.
	data, err := os.ReadFile(validatedPath)
	if err != nil {
		// File doesn't exist or can't be read — return nil.
		return nil
	}
	return data
}

// ---------------------------------------------------------------------------
// Range request handling (replacement for http.ServeContent)
// ---------------------------------------------------------------------------

// httpRange represents a single byte range from a Range header.
type httpRange struct {
	start, length int64
}

// serveRange handles Range requests for in-memory data (cached files).
// It supports single-range requests with proper 206 Partial Content responses,
// and falls back to serving the full content for invalid or unsatisfiable ranges.
func serveRange(ctx *fasthttp.RequestCtx, data []byte, rangeHeader string) {
	size := int64(len(data))

	// Announce range support.
	ctx.Response.Header.Set("Accept-Ranges", "bytes")

	ranges, err := parseRange(rangeHeader, size)
	if err != nil || len(ranges) == 0 {
		// Malformed or empty range — serve the full content (RFC 7233 §4.4).
		ctx.Response.Header.Set("Content-Length", strconv.FormatInt(size, 10))
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBody(data)
		return
	}

	if len(ranges) == 1 {
		// Single range — the common case.
		r := ranges[0]
		ctx.SetStatusCode(fasthttp.StatusPartialContent)
		ctx.Response.Header.Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size))
		ctx.Response.Header.Set("Content-Length", strconv.FormatInt(r.length, 10))
		ctx.SetBody(data[r.start : r.start+r.length])
		return
	}

	// Multiple ranges — use multipart/byteranges.
	contentType := string(ctx.Response.Header.Peek("Content-Type"))
	boundary := "static_web_range_boundary"

	var buf bytes.Buffer
	for _, r := range ranges {
		fmt.Fprintf(&buf, "\r\n--%s\r\n", boundary)
		fmt.Fprintf(&buf, "Content-Type: %s\r\n", contentType)
		fmt.Fprintf(&buf, "Content-Range: bytes %d-%d/%d\r\n\r\n", r.start, r.start+r.length-1, size)
		buf.Write(data[r.start : r.start+r.length])
	}
	fmt.Fprintf(&buf, "\r\n--%s--\r\n", boundary)

	ctx.SetStatusCode(fasthttp.StatusPartialContent)
	ctx.Response.Header.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	ctx.Response.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
	ctx.SetBody(buf.Bytes())
}

// serveLargeFileRange handles Range requests for large files loaded into memory.
func serveLargeFileRange(ctx *fasthttp.RequestCtx, data []byte, size int64, rangeHeader string) {
	ranges, err := parseRange(rangeHeader, size)
	if err != nil || len(ranges) == 0 {
		// Malformed or empty range — serve the full content.
		ctx.Response.Header.Set("Content-Length", strconv.FormatInt(size, 10))
		ctx.SetStatusCode(fasthttp.StatusOK)
		if !ctx.IsHead() {
			ctx.SetBody(data)
		}
		return
	}

	if len(ranges) == 1 {
		r := ranges[0]
		ctx.SetStatusCode(fasthttp.StatusPartialContent)
		ctx.Response.Header.Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size))
		ctx.Response.Header.Set("Content-Length", strconv.FormatInt(r.length, 10))
		if !ctx.IsHead() {
			ctx.SetBody(data[r.start : r.start+r.length])
		}
		return
	}

	// Multiple ranges — use multipart/byteranges.
	contentType := string(ctx.Response.Header.Peek("Content-Type"))
	boundary := "static_web_range_boundary"

	var buf bytes.Buffer
	for _, r := range ranges {
		fmt.Fprintf(&buf, "\r\n--%s\r\n", boundary)
		fmt.Fprintf(&buf, "Content-Type: %s\r\n", contentType)
		fmt.Fprintf(&buf, "Content-Range: bytes %d-%d/%d\r\n\r\n", r.start, r.start+r.length-1, size)
		buf.Write(data[r.start : r.start+r.length])
	}
	fmt.Fprintf(&buf, "\r\n--%s--\r\n", boundary)

	ctx.SetStatusCode(fasthttp.StatusPartialContent)
	ctx.Response.Header.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	ctx.Response.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
	ctx.SetBody(buf.Bytes())
}

// parseRange parses a Range header value (e.g. "bytes=0-499") per RFC 7233.
// Returns the parsed ranges or an error for invalid syntax.
// Returns nil ranges for unsatisfiable ranges (416 would be appropriate but
// we fall back to 200 full content for compatibility).
func parseRange(s string, size int64) ([]httpRange, error) {
	if !strings.HasPrefix(s, "bytes=") {
		return nil, errors.New("invalid range")
	}
	var ranges []httpRange
	for _, ra := range strings.Split(s[len("bytes="):], ",") {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}
		i := strings.Index(ra, "-")
		if i < 0 {
			return nil, errors.New("invalid range")
		}
		start, end := strings.TrimSpace(ra[:i]), strings.TrimSpace(ra[i+1:])
		var r httpRange
		if start == "" {
			// Suffix range: -N means last N bytes.
			if end == "" {
				return nil, errors.New("invalid range")
			}
			i, err := strconv.ParseInt(end, 10, 64)
			if err != nil || i <= 0 {
				return nil, errors.New("invalid range")
			}
			if i > size {
				i = size
			}
			r.start = size - i
			r.length = i
		} else {
			i, err := strconv.ParseInt(start, 10, 64)
			if err != nil || i < 0 || i >= size {
				return nil, errors.New("invalid range")
			}
			r.start = i
			if end == "" {
				// Range to end of file.
				r.length = size - i
			} else {
				j, err := strconv.ParseInt(end, 10, 64)
				if err != nil || j < i {
					return nil, errors.New("invalid range")
				}
				if j >= size {
					j = size - 1
				}
				r.length = j - i + 1
			}
		}
		ranges = append(ranges, r)
	}
	if len(ranges) == 0 {
		return nil, errors.New("invalid range")
	}
	return ranges, nil
}
