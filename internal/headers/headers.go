// Package headers provides HTTP caching and response header middleware.
// It handles ETag/Last-Modified conditional requests (304 Not Modified) and
// sets appropriate Cache-Control headers based on file type.
package headers

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
)

// Middleware returns an http.Handler that:
//  1. Looks up the requested file in the cache to obtain ETag and LastModified.
//  2. Handles If-None-Match → 304 Not Modified.
//  3. Handles If-Modified-Since → 304 Not Modified.
//  4. Sets Cache-Control, ETag, Last-Modified, and Vary headers.
//
// If the file is not yet cached (cache miss at this stage), header setting is
// deferred — the file handler will populate the cache and the next request
// will receive full caching headers.
func Middleware(c *cache.Cache, cfg *config.HeadersConfig, indexFile string, next http.Handler) http.Handler {
	if indexFile == "" {
		indexFile = "index.html"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only handle GET and HEAD.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		urlPath := cacheKeyForPath(r.URL.Path, indexFile)

		cached, ok := c.Get(urlPath)
		if ok {
			// Check conditional request headers.
			if checkNotModified(w, r, cached) {
				return
			}
			// Set caching headers.
			setCacheHeaders(w, urlPath, cached, cfg)
		}

		next.ServeHTTP(w, r)
	})
}

// cacheKeyForPath normalises a URL path to the cache key used by the file
// handler. Directory paths (trailing slash, or bare "/") are mapped to their
// index file so that 304 checks succeed for index requests.
func cacheKeyForPath(urlPath, indexFile string) string {
	if urlPath == "" || urlPath == "/" {
		return "/" + indexFile
	}
	// Any path ending with "/" is a directory request → append index filename.
	if strings.HasSuffix(urlPath, "/") {
		return urlPath + indexFile
	}
	return urlPath
}

// checkNotModified evaluates conditional request headers.
// Returns true and writes a 304 response if the resource has not changed.
func checkNotModified(w http.ResponseWriter, r *http.Request, f *cache.CachedFile) bool {
	etag := f.ETagFull
	if etag == "" {
		etag = `W/"` + f.ETag + `"`
	}

	// If-None-Match takes precedence over If-Modified-Since (RFC 7232 §6).
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatches(inm, etag) {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		return false
	}

	// If-Modified-Since.
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil {
			// Truncate to second precision for comparison.
			if !f.LastModified.After(t.Add(time.Second - 1)) {
				w.Header().Set("Last-Modified", f.LastModified.UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}

	return false
}

// etagMatches reports whether the If-None-Match value matches the given etag.
// It supports the wildcard "*" and a comma-separated list of tags.
func etagMatches(ifNoneMatch, etag string) bool {
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, v := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(v) == etag {
			return true
		}
	}
	return false
}

// setCacheHeaders writes ETag, Last-Modified, Cache-Control, and Vary headers.
func setCacheHeaders(w http.ResponseWriter, urlPath string, f *cache.CachedFile, cfg *config.HeadersConfig) {
	etag := f.ETagFull
	if etag == "" {
		etag = `W/"` + f.ETag + `"`
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", f.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Add("Vary", "Accept-Encoding")

	maxAge := cacheMaxAge(urlPath, f.ContentType, cfg)
	if maxAge == 0 {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		cc := "public, max-age=" + strconv.Itoa(maxAge)
		// Immutable hint for fingerprinted assets.
		if cfg.ImmutablePattern != "" && matchesImmutablePattern(urlPath, cfg.ImmutablePattern) {
			cc += ", immutable"
		}
		w.Header().Set("Cache-Control", cc)
	}
}

// cacheMaxAge returns the appropriate max-age for the file.
func cacheMaxAge(urlPath, contentType string, cfg *config.HeadersConfig) int {
	// HTML gets its own max-age (often 0 for always-revalidate).
	if isHTML(urlPath, contentType) {
		return cfg.HTMLMaxAge
	}
	return cfg.StaticMaxAge
}

// isHTML reports whether the path or content type indicates an HTML file.
func isHTML(urlPath, contentType string) bool {
	if strings.Contains(contentType, "text/html") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(urlPath))
	return ext == ".html" || ext == ".htm"
}

// matchesImmutablePattern checks whether the file path matches the immutable glob pattern.
func matchesImmutablePattern(urlPath, pattern string) bool {
	base := filepath.Base(urlPath)
	matched, err := filepath.Match(pattern, base)
	if err != nil {
		return false
	}
	return matched
}

// SetFileHeaders writes ETag, Last-Modified, Cache-Control, and Vary response
// headers for a file that has just been loaded (possibly bypassing the middleware
// cache-check path). This is called directly from the file handler.
func SetFileHeaders(w http.ResponseWriter, urlPath string, f *cache.CachedFile, cfg *config.HeadersConfig) {
	setCacheHeaders(w, urlPath, f, cfg)
}
