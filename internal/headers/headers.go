// Package headers provides HTTP caching utilities and response header helpers.
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

// CacheKeyForPath normalises a URL path to the cache key used by the file
// handler. Directory paths (trailing slash, or bare "/") are mapped to their
// index file so that 304 checks succeed for index requests.
func CacheKeyForPath(urlPath, indexFile string) string {
	if indexFile == "" {
		indexFile = "index.html"
	}
	if urlPath == "" || urlPath == "/" {
		return "/" + indexFile
	}
	if strings.HasSuffix(urlPath, "/") {
		return urlPath + indexFile
	}
	return urlPath
}

// CheckNotModified evaluates conditional request headers.
// Returns true and writes a 304 response if the resource has not changed.
func CheckNotModified(w http.ResponseWriter, r *http.Request, f *cache.CachedFile) bool {
	etag := f.ETagFull
	if etag == "" {
		etag = `W/"` + f.ETag + `"`
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if ETagMatches(inm, etag) {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		return false
	}

	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil {
			if !f.LastModified.After(t.Add(time.Second - 1)) {
				w.Header().Set("Last-Modified", f.LastModified.UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}

	return false
}

// ETagMatches reports whether the If-None-Match value matches the given etag.
// It supports the wildcard "*" and a comma-separated list of tags.
func ETagMatches(ifNoneMatch, etag string) bool {
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

// SetCacheHeaders writes ETag, Last-Modified, Cache-Control, and Vary headers.
func SetCacheHeaders(w http.ResponseWriter, urlPath string, f *cache.CachedFile, cfg *config.HeadersConfig) {
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
		if cfg.ImmutablePattern != "" && matchesImmutablePattern(urlPath, cfg.ImmutablePattern) {
			cc += ", immutable"
		}
		w.Header().Set("Cache-Control", cc)
	}
}

// SetFileHeaders writes caching headers for a file response.
func SetFileHeaders(w http.ResponseWriter, urlPath string, f *cache.CachedFile, cfg *config.HeadersConfig) {
	SetCacheHeaders(w, urlPath, f, cfg)
}

func cacheMaxAge(urlPath, contentType string, cfg *config.HeadersConfig) int {
	if isHTML(urlPath, contentType) {
		return cfg.HTMLMaxAge
	}
	return cfg.StaticMaxAge
}

func isHTML(urlPath, contentType string) bool {
	if strings.Contains(contentType, "text/html") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(urlPath))
	return ext == ".html" || ext == ".htm"
}

func matchesImmutablePattern(urlPath, pattern string) bool {
	base := filepath.Base(urlPath)
	matched, err := filepath.Match(pattern, base)
	if err != nil {
		return false
	}
	return matched
}
