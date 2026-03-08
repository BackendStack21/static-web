// Package handler provides the core HTTP file-serving handler and middleware
// composition for the static web server.
package handler

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
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
)

// FileHandler serves static files from disk with caching and compression support.
type FileHandler struct {
	cfg                 *config.Config
	cache               *cache.Cache
	absRoot             string // resolved once at construction time
	notFoundData        []byte
	notFoundContentType string
}

// NewFileHandler creates a new FileHandler.
// absRoot is resolved via filepath.Abs so per-request path arithmetic is free
// of OS syscalls.
func NewFileHandler(cfg *config.Config, c *cache.Cache) *FileHandler {
	absRoot, err := filepath.Abs(cfg.Files.Root)
	if err != nil {
		// Fall back to the raw root; PathSafe will catch any traversal attempts.
		absRoot = cfg.Files.Root
	}

	h := &FileHandler{cfg: cfg, cache: c, absRoot: absRoot}
	h.notFoundData, h.notFoundContentType = h.loadCustomNotFoundPage()
	return h
}

// ServeHTTP handles an HTTP request by resolving and serving the requested file.
func (h *FileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path

	// Prefer the pre-validated path injected by security.Middleware — this
	// avoids a second PathSafe call and its two filepath.Abs syscalls.
	absPath, ok := security.SafePathFromContext(r.Context())
	if !ok {
		// Fallback for tests or deployments that bypass security.Middleware.
		var err error
		absPath, err = security.PathSafe(urlPath, h.absRoot, h.cfg.Security.BlockDotfiles)
		if err != nil {
			h.handleSecurityError(w, err)
			return
		}
	}
	// Fast path: for a plain file URL that is already cached, skip os.Stat
	// entirely. os.Stat is deferred to the cache-miss branch below.
	cacheKey := headers.CacheKeyForPath(urlPath, h.cfg.Files.Index)
	if h.cfg.Cache.Enabled && h.cache != nil {
		if cached, ok := h.cache.Get(cacheKey); ok {
			if headers.CheckNotModified(w, r, cached) {
				return
			}
			h.serveFromCache(w, r, cacheKey, cached)
			return
		}
	}

	// Cache miss — determine whether this is a directory request (needs index
	// resolution) only now, when we actually need to hit the filesystem.
	resolvedPath, canonicalURL, info, statErr, serveDirList := h.resolveIndexPath(absPath, urlPath)

	// If the path is a directory and directory listing is enabled, serve the
	// listing immediately (skip index resolution and the cache lookup below).
	if serveDirList {
		h.serveDirectoryListing(w, r, absPath, urlPath)
		return
	}

	// Re-check cache with the canonical URL (e.g. "/subdir/index.html") in
	// case the directory-resolved key is cached even though the bare path isn't.
	if h.cfg.Cache.Enabled && h.cache != nil && canonicalURL != cacheKey {
		if cached, ok := h.cache.Get(canonicalURL); ok {
			if headers.CheckNotModified(w, r, cached) {
				return
			}
			h.serveFromCache(w, r, canonicalURL, cached)
			return
		}
	}

	// True cache miss — read from disk.
	h.serveFromDisk(w, r, resolvedPath, canonicalURL, info, statErr)
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
// For ordinary GET requests (no Range header) it uses a direct w.Write() path
// that avoids the overhead of http.ServeContent (range parsing, content-type
// sniffing, conditional-header re-checking — all unnecessary when the file is
// already fully in memory and we've handled 304s ourselves).
//
// Range requests still go through http.ServeContent for correct multi-range and
// 206 Partial Content support.
func (h *FileHandler) serveFromCache(w http.ResponseWriter, r *http.Request, urlPath string, f *cache.CachedFile) {
	w.Header().Set("X-Cache", "HIT")

	// Set ETag and Cache-Control headers.
	headers.SetFileHeaders(w, urlPath, f, &h.cfg.Headers)

	// Negotiate content encoding using pre-compressed variants.
	data, encoding := h.negotiateEncoding(r, f)

	if encoding != "" {
		w.Header().Set("Content-Encoding", encoding)
		w.Header().Add("Vary", "Accept-Encoding")
	}

	// --- Fast path: non-Range GET/HEAD ----------------------------------
	// Assign pre-formatted header slices directly to the underlying map,
	// bypassing Header.Set() canonicalization overhead. Falls back to
	// Set() if InitHeaders() was never called (defensive).
	if r.Header.Get("Range") == "" {
		hdr := w.Header()
		if f.CTHeader != nil {
			hdr["Content-Type"] = f.CTHeader
		} else {
			hdr.Set("Content-Type", f.ContentType)
		}

		if r.Method == http.MethodHead {
			if f.CLHeader != nil {
				hdr["Content-Length"] = f.CLHeader
			} else {
				hdr.Set("Content-Length", fmt.Sprintf("%d", f.Size))
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		// For compressed data the Content-Length must reflect the encoded
		// size, not the original — compute it only when encoding differs.
		if encoding != "" {
			hdr.Set("Content-Length", strconv.Itoa(len(data)))
		} else if f.CLHeader != nil {
			hdr["Content-Length"] = f.CLHeader
		} else {
			hdr.Set("Content-Length", fmt.Sprintf("%d", f.Size))
		}

		w.WriteHeader(http.StatusOK)
		w.Write(data) //nolint:errcheck // best-effort write to network
		return
	}

	// --- Slow path: Range requests --------------------------------------
	// Content-Type must be set before ServeContent to prevent sniffing.
	w.Header().Set("Content-Type", f.ContentType)

	// For range requests with compressed data we must serve the raw bytes
	// because byte-range offsets apply to the uncompressed content.
	if encoding != "" {
		// Remove the Content-Encoding we set above — Range semantics
		// require uncompressed data.
		w.Header().Del("Content-Encoding")
		data = f.Data
	}

	reader := bytes.NewReader(data)
	http.ServeContent(w, r, "", f.LastModified, reader)
}

// negotiateEncoding selects the best pre-compressed variant for the client.
func (h *FileHandler) negotiateEncoding(r *http.Request, f *cache.CachedFile) ([]byte, string) {
	if !h.cfg.Compression.Enabled {
		return f.Data, ""
	}

	// Brotli preferred over gzip when available.
	if f.BrData != nil && compress.AcceptsEncoding(r, "br") {
		return f.BrData, "br"
	}
	if f.GzipData != nil && compress.AcceptsEncoding(r, "gzip") {
		return f.GzipData, "gzip"
	}
	return f.Data, ""
}

// serveFromDisk reads the file from disk, populates the cache, and serves it.
// If the file does not exist on disk, it falls back to the embedded default
// assets (index.html, 404.html, style.css) before returning a 404.
func (h *FileHandler) serveFromDisk(w http.ResponseWriter, r *http.Request, absPath, urlPath string, info os.FileInfo, statErr error) {
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// Try the embedded fallback assets before giving up.
			if h.serveEmbedded(w, r, urlPath) {
				return
			}
			h.serveNotFound(w, r)
			return
		}
		if os.IsPermission(statErr) {
			log.Printf("handler: permission denied accessing %q", absPath)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// For large files, bypass cache and serve directly.
	if info.Size() > h.cfg.Cache.MaxFileSize {
		h.serveLargeFile(w, r, absPath, urlPath, info)
		return
	}

	// Read file content.
	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsPermission(err) {
			log.Printf("handler: permission denied reading %q", absPath)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		log.Printf("handler: error reading %q: %v", absPath, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
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
		cached.GzipData = loadSidecar(absPath + ".gz")
		cached.BrData = loadSidecar(absPath + ".br")
	}

	// Generate on-the-fly gzip if no sidecar and content is compressible.
	if cached.GzipData == nil && h.cfg.Compression.Enabled &&
		compress.IsCompressible(ct) && len(data) >= h.cfg.Compression.MinSize {
		if gz, err := compress.GzipBytes(data, h.cfg.Compression.Level); err == nil {
			cached.GzipData = gz
		}
	}

	// Pre-format headers for the fast serving path.
	cached.InitHeaders()

	// Store in cache.
	if h.cfg.Cache.Enabled && h.cache != nil {
		h.cache.Put(urlPath, cached)
	}

	w.Header().Set("X-Cache", "MISS")
	h.serveFromCache(w, r, urlPath, cached)
}

// serveLargeFile serves a file that exceeds MaxFileSize directly via *os.File,
// bypassing the in-memory cache but still supporting Range requests.
func (h *FileHandler) serveLargeFile(w http.ResponseWriter, r *http.Request, absPath, urlPath string, info os.FileInfo) {
	f, err := os.Open(absPath)
	if err != nil {
		if os.IsPermission(err) {
			log.Printf("handler: permission denied opening %q", absPath)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		log.Printf("handler: error opening large file %q: %v", absPath, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	ct := detectContentType(absPath, nil)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Cache", "MISS")

	http.ServeContent(w, r, urlPath, info.ModTime(), f)
}

// serveEmbedded attempts to serve a file from the embedded default assets.
// It maps the request URL path to a "public/<filename>" key in defaults.FS.
// Only the base filename is considered (no sub-directory traversal), so this
// only matches the three known defaults: index.html, 404.html, style.css.
// Returns true if the response was written, false otherwise.
func (h *FileHandler) serveEmbedded(w http.ResponseWriter, r *http.Request, urlPath string) bool {
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
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
	return true
}

// serveNotFound serves a custom 404 page if configured, then falls back to the
// embedded default 404.html, and finally to a plain-text 404 response.
// The configured path is validated via PathSafe to prevent path traversal through
// a malicious config value (e.g. STATIC_FILES_NOT_FOUND=../../etc/passwd).
func (h *FileHandler) serveNotFound(w http.ResponseWriter, r *http.Request) {
	if h.notFoundData != nil {
		w.Header().Set("Content-Type", h.notFoundContentType)
		w.WriteHeader(http.StatusNotFound)
		w.Write(h.notFoundData)
		return
	}

	// Fall back to the embedded default 404.html.
	if data, err := fs.ReadFile(defaults.FS, "public/404.html"); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write(data)
		return
	}

	http.Error(w, "404 Not Found", http.StatusNotFound)
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
func (h *FileHandler) handleSecurityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, security.ErrNullByte):
		http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
	case errors.Is(err, security.ErrPathTraversal), errors.Is(err, security.ErrDotfile):
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
	default:
		http.Error(w, "Forbidden", http.StatusForbidden)
	}
}

// computeETag returns the first 16 hex characters of sha256(data).
func computeETag(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)[:16]
}

// detectContentType determines the MIME type of a file by extension, falling back
// to http.DetectContentType for unknown extensions.
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

// loadSidecar attempts to read a pre-compressed sidecar file.
// Returns nil if the sidecar does not exist or cannot be read.
func loadSidecar(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}
