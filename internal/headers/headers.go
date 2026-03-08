// Package headers provides HTTP caching utilities and response header helpers.
package headers

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// CacheKeyForPath normalises a URL path to the cache key used by the file
// handler. Directory paths (trailing slash, or bare "/") are mapped to their
// index file so that 304 checks succeed for index requests.
// PERF-005: fast-path for the common "/" → "/index.html" case.
func CacheKeyForPath(urlPath, indexFile string) string {
	if indexFile == "" {
		indexFile = "index.html"
	}
	if urlPath == "" || urlPath == "/" {
		if indexFile == "index.html" {
			return "/index.html" // static string — zero alloc
		}
		return "/" + indexFile
	}
	if strings.HasSuffix(urlPath, "/") {
		return urlPath + indexFile
	}
	return urlPath
}

// parseHTTPTime parses an HTTP-date string per RFC 7231 §7.1.1.1.
// It tries RFC 1123, RFC 850, and ANSI C asctime formats in that order,
// matching the behaviour of net/http.ParseTime.
func parseHTTPTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC1123, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC850, s); err == nil {
		return t, nil
	}
	return time.Parse(time.ANSIC, s)
}

// CheckNotModified evaluates conditional request headers.
// Returns true and writes a 304 response if the resource has not changed.
// Uses pre-formatted header strings when available (PERF-003).
func CheckNotModified(ctx *fasthttp.RequestCtx, f *cache.CachedFile) bool {
	// Resolve the ETag value to use.
	var etagStr string
	if f.ETagHeader != "" {
		etagStr = f.ETagHeader
	} else {
		etagStr = f.ETagFull
		if etagStr == "" {
			etagStr = `W/"` + f.ETag + `"`
		}
	}

	if inm := string(ctx.Request.Header.Peek("If-None-Match")); inm != "" {
		if ETagMatches(inm, etagStr) {
			ctx.Response.Header.Set("Etag", etagStr)
			ctx.SetStatusCode(fasthttp.StatusNotModified)
			return true
		}
		return false
	}

	if ims := string(ctx.Request.Header.Peek("If-Modified-Since")); ims != "" {
		if t, err := parseHTTPTime(ims); err == nil {
			if !f.LastModified.After(t.Add(time.Second - 1)) {
				if f.LastModHeader != "" {
					ctx.Response.Header.Set("Last-Modified", f.LastModHeader)
				} else {
					ctx.Response.Header.Set("Last-Modified", f.LastModified.UTC().Format(cache.HTTPTimeFormat))
				}
				ctx.SetStatusCode(fasthttp.StatusNotModified)
				return true
			}
		}
	}

	return false
}

// ETagMatches reports whether the If-None-Match value matches the given etag.
// It supports the wildcard "*" and a comma-separated list of tags.
// Uses zero-alloc IndexByte walking instead of strings.Split (PERF-006).
func ETagMatches(ifNoneMatch, etag string) bool {
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for {
		// Skip leading whitespace.
		i := 0
		for i < len(ifNoneMatch) && (ifNoneMatch[i] == ' ' || ifNoneMatch[i] == '\t') {
			i++
		}
		ifNoneMatch = ifNoneMatch[i:]
		if ifNoneMatch == "" {
			return false
		}
		// Find the next comma.
		end := strings.IndexByte(ifNoneMatch, ',')
		var token string
		if end < 0 {
			token = strings.TrimRight(ifNoneMatch, " \t")
			if token == etag {
				return true
			}
			return false
		}
		token = strings.TrimRight(ifNoneMatch[:end], " \t")
		if token == etag {
			return true
		}
		ifNoneMatch = ifNoneMatch[end+1:]
	}
}

// SetCacheHeaders writes ETag, Last-Modified, Cache-Control, and Vary headers.
// When the CachedFile has pre-formatted header strings (from InitHeaders +
// InitCacheControl), they are assigned directly, bypassing string formatting
// entirely (PERF-003).
func SetCacheHeaders(ctx *fasthttp.RequestCtx, urlPath string, f *cache.CachedFile, cfg *config.HeadersConfig) {
	// Pre-formatted fast path: assign pre-computed strings directly.
	if f.ETagHeader != "" {
		ctx.Response.Header.Set("Etag", f.ETagHeader)
	} else {
		etag := f.ETagFull
		if etag == "" {
			etag = `W/"` + f.ETag + `"`
		}
		ctx.Response.Header.Set("ETag", etag)
	}

	if f.LastModHeader != "" {
		ctx.Response.Header.Set("Last-Modified", f.LastModHeader)
	} else {
		ctx.Response.Header.Set("Last-Modified", f.LastModified.UTC().Format(cache.HTTPTimeFormat))
	}

	if f.VaryHeader != "" {
		ctx.Response.Header.Set("Vary", f.VaryHeader)
	} else {
		ctx.Response.Header.Add("Vary", "Accept-Encoding")
	}

	if f.CacheControlHeader != "" {
		ctx.Response.Header.Set("Cache-Control", f.CacheControlHeader)
	} else {
		// Fallback: compute at request time (cold path).
		maxAge := cacheMaxAge(urlPath, f.ContentType, cfg)
		if maxAge == 0 {
			ctx.Response.Header.Set("Cache-Control", "no-cache")
		} else {
			cc := "public, max-age=" + strconv.Itoa(maxAge)
			if cfg.ImmutablePattern != "" && matchesImmutablePattern(urlPath, cfg.ImmutablePattern) {
				cc += ", immutable"
			}
			ctx.Response.Header.Set("Cache-Control", cc)
		}
	}
}

// SetFileHeaders writes caching headers for a file response.
func SetFileHeaders(ctx *fasthttp.RequestCtx, urlPath string, f *cache.CachedFile, cfg *config.HeadersConfig) {
	SetCacheHeaders(ctx, urlPath, f, cfg)
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
