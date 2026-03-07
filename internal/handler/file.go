// Package handler provides the core HTTP file-serving handler and middleware
// composition for the static web server.
package handler

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/static-web/server/internal/cache"
	"github.com/static-web/server/internal/compress"
	"github.com/static-web/server/internal/config"
	"github.com/static-web/server/internal/headers"
	"github.com/static-web/server/internal/security"
)

// FileHandler serves static files from disk with caching and compression support.
type FileHandler struct {
	cfg     *config.Config
	cache   *cache.Cache
	absRoot string // resolved once at construction time
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
	return &FileHandler{cfg: cfg, cache: c, absRoot: absRoot}
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
	cacheKey := urlPath
	if h.cfg.Cache.Enabled {
		if cached, ok := h.cache.Get(cacheKey); ok {
			h.serveFromCache(w, r, cacheKey, cached)
			return
		}
	}

	// Cache miss — determine whether this is a directory request (needs index
	// resolution) only now, when we actually need to hit the filesystem.
	resolvedPath, canonicalURL := h.resolveIndexPath(absPath, urlPath)

	// If the path is a directory and directory listing is enabled, serve the
	// listing immediately (skip index resolution and the cache lookup below).
	if resolvedPath == "" {
		h.serveDirectoryListing(w, r, absPath, urlPath)
		return
	}

	// Re-check cache with the canonical URL (e.g. "/subdir/index.html") in
	// case the directory-resolved key is cached even though the bare path isn't.
	if h.cfg.Cache.Enabled && canonicalURL != cacheKey {
		if cached, ok := h.cache.Get(canonicalURL); ok {
			h.serveFromCache(w, r, canonicalURL, cached)
			return
		}
	}

	// True cache miss — read from disk.
	h.serveFromDisk(w, r, resolvedPath, canonicalURL)
}

// resolveIndexPath maps a directory path to its index file.
// Returns ("", "") when the path is a directory and directory listing is
// enabled — the caller should invoke serveDirectoryListing instead.
// Returns the resolved absolute path and the canonical URL key otherwise.
func (h *FileHandler) resolveIndexPath(absPath, urlPath string) (string, string) {
	info, err := os.Stat(absPath)
	if err == nil && info.IsDir() {
		// Directory listing takes precedence over index resolution when enabled.
		if h.cfg.Security.DirectoryListing {
			return "", ""
		}
		indexFile := h.cfg.Files.Index
		if indexFile == "" {
			indexFile = "index.html"
		}
		return filepath.Join(absPath, indexFile), strings.TrimRight(urlPath, "/") + "/" + indexFile
	}
	return absPath, urlPath
}

// serveFromCache writes a cached file to the response, respecting Accept-Encoding.
func (h *FileHandler) serveFromCache(w http.ResponseWriter, r *http.Request, urlPath string, f *cache.CachedFile) {
	w.Header().Set("X-Cache", "HIT")

	// Set ETag and Cache-Control headers (not already set by headers middleware if this is a 200).
	headers.SetFileHeaders(w, urlPath, f, &h.cfg.Headers)
	w.Header().Set("Content-Type", f.ContentType)

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", f.Size))
		w.WriteHeader(http.StatusOK)
		return
	}

	// Negotiate content encoding using pre-compressed variants.
	data, encoding := h.negotiateEncoding(r, f)

	if encoding != "" {
		w.Header().Set("Content-Encoding", encoding)
		w.Header().Add("Vary", "Accept-Encoding")
	}

	// Use http.ServeContent for Range request support.
	// Wrap bytes.Reader so http.ServeContent can seek and detect size.
	reader := bytes.NewReader(data)
	http.ServeContent(w, r, urlPath, f.LastModified, reader)
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
func (h *FileHandler) serveFromDisk(w http.ResponseWriter, r *http.Request, absPath, urlPath string) {
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			h.serveNotFound(w, r)
			return
		}
		if os.IsPermission(err) {
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

	// Load pre-compressed sidecar files if enabled.
	if h.cfg.Compression.Enabled && h.cfg.Compression.Precompressed {
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

	// Store in cache.
	if h.cfg.Cache.Enabled {
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

// serveNotFound serves a custom 404 page if configured, otherwise a plain 404.
// The configured path is validated via PathSafe to prevent path traversal through
// a malicious config value (e.g. STATIC_FILES_NOT_FOUND=../../etc/passwd).
func (h *FileHandler) serveNotFound(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Files.NotFound != "" {
		safeNotFound, err := security.PathSafe(h.cfg.Files.NotFound, h.cfg.Files.Root, false)
		if err == nil {
			if data, err := os.ReadFile(safeNotFound); err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				w.Write(data)
				return
			}
		}
	}
	http.Error(w, "404 Not Found", http.StatusNotFound)
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
