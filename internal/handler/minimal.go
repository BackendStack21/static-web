package handler

import (
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BackendStack21/static-web/internal/config"
)

// preloadedFile holds a file's content and pre-built header values, ready to
// serve without any per-request filesystem interaction or allocation.
type preloadedFile struct {
	body     []byte
	ctHeader []string // pre-allocated {"text/html; charset=utf-8"}
	clHeader []string // pre-allocated {"2943"}
}

// BuildBenchmarkHandler returns a handler optimised for raw throughput in
// apples-to-apples benchmarks against other static file servers. All files
// under cfg.Files.Root are loaded into memory at startup so the hot path is
// a pure map lookup + w.Write() with zero syscalls and zero allocations.
//
// Security: path traversal is validated at startup (only real files under root
// are indexed). At request time the URL is cleaned with path.Clean and looked
// up in the preloaded map — there is no way to escape the root.
func BuildBenchmarkHandler(cfg *config.Config) http.Handler {
	absRoot, err := filepath.Abs(cfg.Files.Root)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Internal Server Error: invalid root path", http.StatusInternalServerError)
		})
	}
	if real, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = real
	}

	indexFile := cfg.Files.Index
	if indexFile == "" {
		indexFile = "index.html"
	}

	// Preload every regular file under absRoot into a map keyed by its
	// URL path (e.g. "/style.css", "/index.html").
	files := make(map[string]*preloadedFile, 32)

	_ = filepath.WalkDir(absRoot, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip dirs and errors
		}

		rel, err := filepath.Rel(absRoot, fpath)
		if err != nil {
			return nil
		}

		body, err := os.ReadFile(fpath)
		if err != nil {
			return nil // skip unreadable files
		}

		// Build the URL-style key: always forward-slash, rooted.
		urlKey := "/" + filepath.ToSlash(rel)

		ct := mime.TypeByExtension(filepath.Ext(fpath))
		if ct == "" {
			ct = "application/octet-stream"
		}

		pf := &preloadedFile{
			body:     body,
			ctHeader: []string{ct},
			clHeader: []string{strconv.Itoa(len(body))},
		}
		files[urlKey] = pf

		// If this file is the index, also register it for the directory path.
		if path.Base(urlKey) == indexFile {
			dir := path.Dir(urlKey)
			if dir != "/" {
				dir += "/"
			}
			files[dir] = pf
		}

		return nil
	})

	// Pre-compute the 404 response body.
	notFoundBody := []byte("404 page not found\n")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Method gate — GET is overwhelmingly common so check it first.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Clean and look up. path.Clean is cheap for already-clean paths
		// (which is the common case for benchmarks hitting "/").
		clean := path.Clean(r.URL.Path)
		pf := files[clean]
		if pf == nil && clean != "/" && !strings.HasSuffix(clean, "/") {
			// Try with trailing slash (directory with index).
			pf = files[clean+"/"]
		}
		if pf == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write(notFoundBody) //nolint:errcheck
			return
		}

		// Direct map assignment avoids Header().Set() overhead (no canonicalization).
		h := w.Header()
		h["Content-Type"] = pf.ctHeader
		h["Content-Length"] = pf.clHeader
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			w.Write(pf.body) //nolint:errcheck
		}
	})
}
