// Package handler provides the core HTTP file-serving handler and middleware
// composition for the static web server.
package handler

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	mrandv2 "math/rand/v2"
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
	resolvedPath, canonicalURL, info, statErr, serveDirList, secErr := h.resolveIndexPath(absPath, urlPath)
	if secErr != nil {
		h.handleSecurityError(ctx, secErr)
		return
	}

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
func (h *FileHandler) resolveIndexPath(absPath, urlPath string) (resolvedPath, canonicalURL string, info os.FileInfo, statErr error, serveDirList bool, secErr error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return absPath, urlPath, nil, err, false, nil
	}
	if info.IsDir() {
		// Directory listing takes precedence over index resolution when enabled.
		if h.cfg.Security.DirectoryListing {
			return "", "", info, nil, true, nil
		}
		indexFile := h.cfg.Files.Index
		if indexFile == "" {
			indexFile = "index.html"
		}
		indexURL := strings.TrimRight(urlPath, "/") + "/" + indexFile
		// Validate the index file like any request path (symlink escape, dotfiles).
		indexPath, err := security.PathSafe(indexURL, h.absRoot, h.cfg.Security.BlockDotfiles)
		if err != nil {
			return "", "", nil, nil, false, err
		}
		indexInfo, indexErr := os.Stat(indexPath)
		return indexPath, indexURL, indexInfo, indexErr, false, nil
	}
	return absPath, urlPath, info, nil, false, nil
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
		headers.AddVary(&ctx.Response.Header, "Accept-Encoding")
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
			if encoding != "" {
				ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(data)))
			} else if f.CLHeader != "" {
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
		cached.GzipData = h.loadSidecarMax(absPath+".gz", int64(len(data)))
		cached.BrData = h.loadSidecarMax(absPath+".br", int64(len(data)))
		cached.ZstdData = h.loadSidecarMax(absPath+".zst", int64(len(data)))
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

// streamBody couples a reader with the file that must be closed when
// fasthttp has finished writing the body stream.
type streamBody struct {
	io.Reader
	io.Closer
}

// serveLargeFile serves a file that exceeds MaxFileSize directly from disk,
// bypassing the in-memory cache. It is stat-first: validators and conditional
// requests are answered from FileInfo alone, HEAD never opens the file, and
// bodies (full, single range, multipart) are streamed from disk with exact
// Content-Length rather than being buffered.
// SEC-011: Enforces MaxServeFileSize as a hard maximum.
func (h *FileHandler) serveLargeFile(ctx *fasthttp.RequestCtx, absPath, urlPath string, info os.FileInfo) {
	// SEC-011: Reject files that exceed the configured hard maximum.
	if h.cfg.Files.MaxServeFileSize > 0 && info.Size() > h.cfg.Files.MaxServeFileSize {
		log.Printf("handler: file %q (%d bytes) exceeds max serve size (%d bytes)",
			absPath, info.Size(), h.cfg.Files.MaxServeFileSize)
		ctx.Error(
			fmt.Sprintf("Payload Too Large: exceeds max_serve_file_size (%d bytes)", h.cfg.Files.MaxServeFileSize),
			fasthttp.StatusRequestEntityTooLarge,
		)
		return
	}

	size := info.Size()
	isHead := ctx.IsHead()

	var f *os.File
	closeFile := func() {
		if f != nil {
			f.Close()
			f = nil
		}
	}
	open := func() bool {
		var err error
		f, err = os.Open(absPath)
		if err == nil {
			return true
		}
		if os.IsPermission(err) {
			log.Printf("handler: permission denied opening %q", absPath)
			ctx.Error("Forbidden", fasthttp.StatusForbidden)
		} else {
			log.Printf("handler: error opening large file %q: %v", absPath, err)
			ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
		}
		f = nil
		return false
	}

	// Content type: by extension, sniffing the first bytes only when needed.
	ct := detectContentType(absPath, nil)
	if mime.TypeByExtension(strings.ToLower(filepath.Ext(absPath))) == "" {
		if !open() {
			return
		}
		var buf [512]byte
		n, _ := io.ReadFull(f, buf[:])
		ct = detectContentType(absPath, buf[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			closeFile()
			ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
			return
		}
	}
	defer closeFile()

	// Validators derived from size + mtime (weak, no content hashing).
	etag := strconv.FormatInt(size, 16) + "-" + strconv.FormatInt(info.ModTime().UnixNano(), 16)
	cf := &cache.CachedFile{
		ETag:         etag,
		ETagFull:     `W/"` + etag + `"`,
		LastModified: info.ModTime(),
		ContentType:  ct,
		Size:         size,
	}
	cf.InitHeaders()
	cf.InitCacheControl(urlPath, h.cfg.Headers.HTMLMaxAge, h.cfg.Headers.StaticMaxAge, h.cfg.Headers.ImmutablePattern)

	if headers.CheckNotModified(ctx, cf, h.cfg.Headers.EnableETags) {
		return
	}

	headers.SetFileHeaders(ctx, urlPath, cf, &h.cfg.Headers)
	ctx.Response.Header.Set("Content-Type", ct)
	ctx.Response.Header.Set("X-Cache", "MISS")
	ctx.Response.Header.Set("Accept-Ranges", "bytes")

	// Work out which bytes to send.
	var ranges []httpRange
	if rangeHeader := string(ctx.Request.Header.Peek("Range")); rangeHeader != "" && ifRangeAllowsRange(ctx) {
		rs, err := parseRange(rangeHeader, size)
		if errors.Is(err, errUnsatisfiableRange) {
			ctx.Response.Header.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
			ctx.SetStatusCode(fasthttp.StatusRequestedRangeNotSatisfiable)
			return
		}
		if err == nil {
			ranges = rs
		}
	}

	status := fasthttp.StatusOK
	total := size
	var readers []io.Reader
	switch len(ranges) {
	case 0:
		// Full content, read lazily below.
	case 1:
		r := ranges[0]
		status = fasthttp.StatusPartialContent
		total = r.length
		ctx.Response.Header.Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size))
	default:
		status = fasthttp.StatusPartialContent
		boundary := generateBoundary() // SEC-004: random boundary per response
		total = 0
		type part struct {
			head string
			r    httpRange
		}
		parts := make([]part, 0, len(ranges))
		for _, r := range ranges {
			hd := fmt.Sprintf("\r\n--%s\r\nContent-Type: %s\r\nContent-Range: bytes %d-%d/%d\r\n\r\n",
				boundary, ct, r.start, r.start+r.length-1, size)
			parts = append(parts, part{hd, r})
			total += int64(len(hd)) + r.length
		}
		tail := fmt.Sprintf("\r\n--%s--\r\n", boundary)
		total += int64(len(tail))
		ctx.Response.Header.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
		if !isHead {
			if f == nil && !open() {
				return
			}
			for _, p := range parts {
				readers = append(readers, strings.NewReader(p.head), io.NewSectionReader(f, p.r.start, p.r.length))
			}
			readers = append(readers, strings.NewReader(tail))
		}
	}

	ctx.SetStatusCode(status)
	if isHead {
		ctx.Response.Header.Set("Content-Length", strconv.FormatInt(total, 10))
		return
	}

	if f == nil && !open() {
		return
	}
	var body io.Reader
	switch len(ranges) {
	case 0:
		body = f
	case 1:
		body = io.NewSectionReader(f, ranges[0].start, ranges[0].length)
	default:
		body = io.MultiReader(readers...)
	}
	// fasthttp closes the stream (io.Closer) once the body is written.
	ctx.SetBodyStream(streamBody{Reader: body, Closer: f}, int(total))
	f = nil // ownership passed to the body stream
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
	headers.AddVary(&ctx.Response.Header, "Accept-Encoding")

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

// computeETag returns the first 16 hex characters of sha256(data), yielding a
// 64-bit fingerprint of the file content.
// SEC-013: The truncation to 8 bytes (64 bits) is intentional. With the birthday
// paradox, collision probability reaches 1% at ~190 million files — well beyond
// practical static-server workloads. The short ETag saves bandwidth on every
// conditional request/response. If a stronger fingerprint is ever needed, the
// full sha256 sum can be used instead.
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

// ValidateSidecarPath validates that a sidecar file path is within the root directory.
// It uses filepath.Clean() to normalize the path (recognized by CodeQL as a sanitizer),
// then verifies the canonical path remains within the root via symlink resolution and
// prefix checking. Returns the validated path or an error if validation fails.
// This function is designed to be recognized by static analyzers as a path sanitizer.
func (h *FileHandler) ValidateSidecarPath(sidecarPath string) (string, error) {
	// Step 1: Normalize the path using filepath.Clean().
	// This removes ".." and "." components and is recognized by CodeQL as a sanitizer.
	cleanPath := filepath.Clean(sidecarPath)

	// Step 2: Ensure the path is absolute (relative to root).
	// If it's relative, make it absolute relative to root.
	var absPath string
	if filepath.IsAbs(cleanPath) {
		absPath = cleanPath
	} else {
		absPath = filepath.Join(h.absRoot, cleanPath)
	}

	// Step 3: Resolve symlinks to get the canonical path.
	// This prevents symlink escape attacks where a sidecar could point outside root.
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// File doesn't exist or can't be resolved — return error.
		return "", err
	}

	// Step 4: Resolve the root directory to its canonical path for comparison.
	// This is important on platforms like macOS where /tmp → /private/tmp.
	realRoot := h.absRoot
	if r, err := filepath.EvalSymlinks(h.absRoot); err == nil {
		realRoot = r
	}

	// Step 5: Verify the resolved path is still within the root directory.
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

// LoadSidecar attempts to read a pre-compressed sidecar file.
// Returns nil if the sidecar does not exist, cannot be read, or fails validation.
// The path parameter must be constructed from a validated absolute filesystem path
// (e.g., absPath + ".gz") to ensure it remains within the root directory.
func (h *FileHandler) LoadSidecar(path string) []byte {
	return h.loadSidecarMax(path, 0)
}

// loadSidecarMax is LoadSidecar with an upper size bound: a sidecar larger
// than maxSize (when > 0) is skipped, since a "compressed" variant bigger than
// the source is pointless and an unbounded read is a memory hazard.
func (h *FileHandler) loadSidecarMax(path string, maxSize int64) []byte {
	// Validate the sidecar path to prevent path traversal attacks.
	validatedPath, err := h.ValidateSidecarPath(path)
	if err != nil {
		// Validation failed (symlink escape, doesn't exist, etc.) — return nil.
		return nil
	}

	if maxSize > 0 {
		if fi, err := os.Stat(validatedPath); err != nil || fi.Size() > maxSize {
			return nil
		}
	}

	// Path is validated and safe — read the file.
	data, err := os.ReadFile(validatedPath)
	if err != nil {
		// File doesn't exist or can't be read — return nil.
		return nil
	}
	return data
}

// generateBoundary returns a random MIME multipart boundary string.
// SEC-004: Using a unique boundary per response prevents attackers from
// predicting boundary values and crafting payloads that exploit multipart
// parsing in downstream proxies or clients.
//
// If crypto/rand fails (extremely unlikely on modern kernels), the function
// falls back to math/rand/v2 (auto-seeded from crypto/rand at process start)
// and logs a warning. The boundary is never all-zeroes.
func generateBoundary() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		log.Printf("WARNING: crypto/rand.Read failed (%v), using math/rand fallback for multipart boundary", err)
		binary.NativeEndian.PutUint64(buf[:8], mrandv2.Uint64())
		binary.NativeEndian.PutUint64(buf[8:], mrandv2.Uint64())
	}
	return hex.EncodeToString(buf[:])
}

// ---------------------------------------------------------------------------
// Range request handling (replacement for http.ServeContent)
// ---------------------------------------------------------------------------

// httpRange represents a single byte range from a Range header.
type httpRange struct {
	start, length int64
}

// maxRanges caps the number of byte ranges honoured in one request.
const maxRanges = 16

// errUnsatisfiableRange signals that no requested range overlaps the content
// (RFC 9110 §15.5.17: respond 416).
var errUnsatisfiableRange = errors.New("unsatisfiable range")

// serveRange serves a Range request for in-memory data. Malformed, excessive
// or amplifying range sets are ignored (200 full content, RFC 9110 §14.2);
// fully unsatisfiable sets yield 416 with Content-Range: bytes */size.
func serveRange(ctx *fasthttp.RequestCtx, data []byte, rangeHeader string) {
	size := int64(len(data))
	ctx.Response.Header.Set("Accept-Ranges", "bytes")

	var ranges []httpRange
	var err error
	if ifRangeAllowsRange(ctx) {
		ranges, err = parseRange(rangeHeader, size)
	} else {
		err = errors.New("If-Range validator mismatch") // ignore Range, serve full content
	}
	if errors.Is(err, errUnsatisfiableRange) {
		ctx.Response.Header.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		ctx.Response.Header.Del("Content-Length")
		ctx.SetStatusCode(fasthttp.StatusRequestedRangeNotSatisfiable)
		ctx.ResetBody()
		return
	}
	if err != nil || len(ranges) == 0 {
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
	boundary := generateBoundary() // SEC-004: random boundary per response

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
	if !ctx.IsHead() {
		ctx.SetBody(buf.Bytes())
	}
}

// ifRangeAllowsRange evaluates the If-Range precondition (RFC 9110 §13.1.5)
// against the validators already set on the response. It returns true when
// the header is absent or matches: a strong ETag (weak ones never match) by
// strong comparison, or an HTTP-date exactly equal to Last-Modified.
func ifRangeAllowsRange(ctx *fasthttp.RequestCtx) bool {
	v := strings.TrimSpace(string(ctx.Request.Header.Peek("If-Range")))
	if v == "" {
		return true
	}
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/") {
		if strings.HasPrefix(v, "W/") {
			return false
		}
		etag := string(ctx.Response.Header.Peek("ETag"))
		return etag != "" && !strings.HasPrefix(etag, "W/") && etag == v
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return false
	}
	lm, err := http.ParseTime(string(ctx.Response.Header.Peek("Last-Modified")))
	if err != nil {
		return false
	}
	return t.Equal(lm)
}

// serveLargeFileRange handles Range requests for large files loaded into memory.
func serveLargeFileRange(ctx *fasthttp.RequestCtx, data []byte, _ int64, rangeHeader string) {
	serveRange(ctx, data, rangeHeader)
}

// parseRange parses a Range header value (e.g. "bytes=0-499") per RFC 9110 §14.
// It returns:
//   - errUnsatisfiableRange when syntactically valid ranges exist but none is satisfiable;
//   - another error for malformed syntax, more than maxRanges ranges, or ranges whose
//     summed length exceeds size (callers ignore Range and serve 200 full content).
//
// Individually unsatisfiable ranges are skipped.
func parseRange(s string, size int64) ([]httpRange, error) {
	if !strings.HasPrefix(s, "bytes=") {
		return nil, errors.New("invalid range")
	}
	var ranges []httpRange
	var total int64
	specs := 0
	for _, ra := range strings.Split(s[len("bytes="):], ",") {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}
		specs++
		if specs > maxRanges {
			return nil, errors.New("too many ranges")
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
			n, err := strconv.ParseInt(end, 10, 64)
			if err != nil || n < 0 {
				return nil, errors.New("invalid range")
			}
			if n == 0 || size == 0 {
				continue // unsatisfiable
			}
			if n > size {
				n = size
			}
			r.start = size - n
			r.length = n
		} else {
			first, err := strconv.ParseInt(start, 10, 64)
			if err != nil || first < 0 {
				return nil, errors.New("invalid range")
			}
			last := size - 1
			if end != "" {
				j, err := strconv.ParseInt(end, 10, 64)
				if err != nil || j < first {
					return nil, errors.New("invalid range")
				}
				if j < last {
					last = j
				}
			}
			if first >= size {
				continue // unsatisfiable
			}
			r.start = first
			r.length = last - first + 1
		}
		total += r.length
		if total > size {
			return nil, errors.New("ranges exceed content size")
		}
		ranges = append(ranges, r)
	}
	if specs == 0 {
		return nil, errors.New("invalid range")
	}
	if len(ranges) == 0 {
		return nil, errUnsatisfiableRange
	}
	return ranges, nil
}
