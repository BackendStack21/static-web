// Package headers provides HTTP caching utilities and response header helpers.
package headers

import (
	"bytes"
	"path"
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
// SEC-006: Applies path.Clean to prevent cache key inconsistencies from
// non-canonical paths (e.g. "/a/../b" vs "/b").
// PERF-005: fast-path for the common "/" → "/index.html" case.
func CacheKeyForPath(urlPath, indexFile string) string {
	if indexFile == "" {
		indexFile = "index.html"
	}
	// Remember whether the original path denotes a directory (trailing slash).
	isDir := urlPath == "" || urlPath == "/" || strings.HasSuffix(urlPath, "/")

	// P-F3: an already-canonical file path is its own key; skip path.Clean
	// and its allocations on the cache-hit path.
	if !isDir && isCleanAbsPath(urlPath) {
		return urlPath
	}

	// SEC-006: Normalise the URL path to prevent cache-key collisions caused
	// by non-canonical input (e.g. "/a/../b" should map to the same key as "/b").
	urlPath = path.Clean("/" + urlPath)

	if urlPath == "/" {
		if indexFile == "index.html" {
			return "/index.html" // static string — zero alloc
		}
		return "/" + indexFile
	}
	if isDir {
		return urlPath + "/" + indexFile
	}
	return urlPath
}

// isCleanAbsPath reports whether path.Clean("/"+p) == p for a p without a
// trailing slash: p is absolute and has no empty, "." or ".." element.
// Elements that merely start with a dot (".well-known") are rejected too and
// take the slow path, which is always correct.
func isCleanAbsPath(p string) bool {
	if len(p) < 2 || p[0] != '/' {
		return false
	}
	for i := 1; i < len(p); i++ {
		if p[i-1] == '/' && (p[i] == '/' || p[i] == '.') {
			return false
		}
	}
	return true
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

// AddVary merges token into the response's Vary header, keeping a single
// header line and avoiding duplicates (case-insensitive). An existing
// "Vary: *" is left untouched.
func AddVary(h *fasthttp.ResponseHeader, token string) {
	existing := h.PeekAll("Vary")
	if len(existing) == 0 {
		h.Set("Vary", token)
		return
	}
	// P-F3 fast paths for the usual single, already-normalised line. They
	// produce exactly what the general merge below would, without allocating.
	if len(existing) == 1 {
		switch scanVaryLine(existing[0], token) {
		case varyLineUnchanged:
			return
		case varyLineAppend:
			var buf [128]byte
			v := append(buf[:0], existing[0]...)
			v = append(v, ", "...)
			v = append(v, token...)
			h.SetBytesV("Vary", v)
			return
		}
	}
	var tokens []string
	found := false
	for _, v := range existing {
		for _, t := range strings.Split(string(v), ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if t == "*" {
				return
			}
			if strings.EqualFold(t, token) {
				if found {
					continue
				}
				found = true
			}
			dup := false
			for _, e := range tokens {
				if strings.EqualFold(e, t) {
					dup = true
					break
				}
			}
			if !dup {
				tokens = append(tokens, t)
			}
		}
	}
	if !found {
		tokens = append(tokens, token)
	}
	h.Set("Vary", strings.Join(tokens, ", "))
}

// varyLineAction is the result of scanVaryLine.
type varyLineAction int

const (
	varyLineMerge     varyLineAction = iota // not normalised: use the general merge
	varyLineUnchanged                       // the merge would leave the line as is
	varyLineAppend                          // the merge would give line + ", " + token
)

// scanVaryLine decides, without allocating, what AddVary's general merge
// would do to a single Vary line v. A line containing "*" is never changed.
// Otherwise the merge rewrites the line as its distinct (case-insensitive),
// non-empty, trimmed tokens joined by ", ", plus token when it is missing.
// When v already has exactly that form, the result is v itself or
// v + ", " + token.
func scanVaryLine(v []byte, token string) varyLineAction {
	normalised, found := true, false
	for start := 0; ; {
		end := len(v)
		if j := bytes.IndexByte(v[start:], ','); j >= 0 {
			end = start + j
		}
		piece := v[start:end]
		t := bytes.TrimSpace(piece)
		if len(t) == 1 && t[0] == '*' {
			return varyLineUnchanged
		}
		// Normalised: the first piece is a bare token, every later piece is
		// one space followed by a bare token, and no token is empty or a
		// case-insensitive duplicate of an earlier one.
		if normalised {
			switch {
			case len(t) == 0:
				normalised = false
			case start == 0 && len(t) != len(piece):
				normalised = false
			case start > 0 && (len(t)+1 != len(piece) || piece[0] != ' '):
				normalised = false
			case varyHasToken(v[:start], t):
				normalised = false
			case equalFoldBytesString(t, token):
				found = true
			}
		}
		if end == len(v) {
			break
		}
		start = end + 1
	}
	switch {
	case !normalised:
		return varyLineMerge
	case found:
		return varyLineUnchanged
	default:
		return varyLineAppend
	}
}

// varyHasToken reports whether the comma-separated list v contains t,
// compared case-insensitively after trimming. It does not allocate.
func varyHasToken(v, t []byte) bool {
	for len(v) > 0 {
		p := v
		if j := bytes.IndexByte(v, ','); j >= 0 {
			p, v = v[:j], v[j+1:]
		} else {
			v = nil
		}
		if bytes.EqualFold(bytes.TrimSpace(p), t) {
			return true
		}
	}
	return false
}

// equalFoldBytesString is strings.EqualFold(string(b), s) without the
// conversion on the ASCII fast path.
func equalFoldBytesString(b []byte, s string) bool {
	if len(b) == len(s) {
		for i := 0; i < len(b); i++ {
			c, d := b[i], s[i]
			if c >= 0x80 || d >= 0x80 {
				return strings.EqualFold(string(b), s)
			}
			if c != d && lowerASCII(c) != lowerASCII(d) {
				return false
			}
		}
		return true
	}
	// Different byte lengths can only fold equal with non-ASCII runes.
	for _, c := range b {
		if c >= 0x80 {
			return strings.EqualFold(string(b), s)
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return strings.EqualFold(string(b), s)
		}
	}
	return false
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// setValidatorHeaders writes the headers a 304 must carry (RFC 9110 §15.4.5):
// ETag, Cache-Control, Vary (and Last-Modified).
func setValidatorHeaders(ctx *fasthttp.RequestCtx, f *cache.CachedFile, enableETags bool) {
	if enableETags {
		etagStr := f.ETagHeader
		if etagStr == "" {
			etagStr = f.ETagFull
			if etagStr == "" {
				etagStr = `W/"` + f.ETag + `"`
			}
		}
		ctx.Response.Header.Set("ETag", etagStr)
	}
	if f.LastModHeader != "" {
		ctx.Response.Header.Set("Last-Modified", f.LastModHeader)
	} else if !f.LastModified.IsZero() {
		ctx.Response.Header.Set("Last-Modified", f.LastModified.UTC().Format(cache.HTTPTimeFormat))
	}
	if f.CacheControlHeader != "" {
		ctx.Response.Header.Set("Cache-Control", f.CacheControlHeader)
	}
	AddVary(&ctx.Response.Header, "Accept-Encoding")
}

// CheckNotModified evaluates conditional request headers.
// Returns true and writes a 304 response if the resource has not changed.
// Uses pre-formatted header strings when available (PERF-003).
// When enableETags is false, ETag-based validation is skipped.
func CheckNotModified(ctx *fasthttp.RequestCtx, f *cache.CachedFile, enableETags bool) bool {
	// Check If-None-Match (ETag-based) only if ETags are enabled.
	if enableETags {
		etagStr := f.ETagHeader
		if etagStr == "" {
			etagStr = f.ETagFull
			if etagStr == "" {
				etagStr = `W/"` + f.ETag + `"`
			}
		}

		if inm := string(ctx.Request.Header.Peek("If-None-Match")); inm != "" {
			if ETagMatches(inm, etagStr) {
				setValidatorHeaders(ctx, f, enableETags)
				ctx.SetStatusCode(fasthttp.StatusNotModified)
				return true
			}
			return false
		}
	}

	if ims := string(ctx.Request.Header.Peek("If-Modified-Since")); ims != "" && !f.LastModified.IsZero() {
		if t, err := parseHTTPTime(ims); err == nil {
			if !f.LastModified.After(t.Add(time.Second - 1)) {
				setValidatorHeaders(ctx, f, enableETags)
				ctx.SetStatusCode(fasthttp.StatusNotModified)
				return true
			}
		}
	}

	return false
}

// ETagMatches reports whether the If-None-Match value matches the given etag
// using weak comparison (RFC 9110 §13.1.2): the W/ prefix is ignored on both
// sides. It supports the wildcard "*" and a comma-separated list of tags.
func ETagMatches(ifNoneMatch, etag string) bool {
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for {
		i := 0
		for i < len(ifNoneMatch) && (ifNoneMatch[i] == ' ' || ifNoneMatch[i] == '\t') {
			i++
		}
		ifNoneMatch = ifNoneMatch[i:]
		if ifNoneMatch == "" {
			return false
		}
		var token string
		end := strings.IndexByte(ifNoneMatch, ',')
		if end < 0 {
			token, ifNoneMatch = ifNoneMatch, ""
		} else {
			token, ifNoneMatch = ifNoneMatch[:end], ifNoneMatch[end+1:]
		}
		token = strings.TrimPrefix(strings.TrimRight(token, " \t"), "W/")
		if token == want {
			return true
		}
	}
}

// SetCacheHeaders writes ETag, Last-Modified, Cache-Control, and Vary headers.
// When the CachedFile has pre-formatted header strings (from InitHeaders +
// InitCacheControl), they are assigned directly, bypassing string formatting
// entirely (PERF-003).
// When cfg.EnableETags is false, ETag headers are not set.
func SetCacheHeaders(ctx *fasthttp.RequestCtx, urlPath string, f *cache.CachedFile, cfg *config.HeadersConfig) {
	// Set ETag header only if ETags are enabled.
	if cfg.EnableETags {
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
	}

	if f.LastModHeader != "" {
		ctx.Response.Header.Set("Last-Modified", f.LastModHeader)
	} else {
		ctx.Response.Header.Set("Last-Modified", f.LastModified.UTC().Format(cache.HTTPTimeFormat))
	}

	if f.VaryHeader != "" {
		// Walk the comma-separated tokens without strings.Split (P-F3).
		for rest := f.VaryHeader; rest != ""; {
			var tok string
			tok, rest, _ = strings.Cut(rest, ",")
			if tok = strings.TrimSpace(tok); tok != "" {
				AddVary(&ctx.Response.Header, tok)
			}
		}
	} else {
		AddVary(&ctx.Response.Header, "Accept-Encoding")
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
