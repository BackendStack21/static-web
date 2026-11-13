// Package security provides path safety checks and HTTP security middleware
// for the static web server.
package security

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/headers"
	"github.com/valyala/fasthttp"
)

// Sentinel errors returned by PathSafe.
var (
	// ErrNullByte indicates the URL path contained a null byte.
	ErrNullByte = errors.New("path contains null byte")
	// ErrPathTraversal indicates the resolved path escapes the root directory.
	ErrPathTraversal = errors.New("path traversal detected")
	// ErrDotfile indicates a path component starts with '.' and dotfiles are blocked.
	ErrDotfile = errors.New("dotfile access denied")
)

// safePathUserValueKey is the key used to store the validated absolute path
// in the fasthttp RequestCtx via SetUserValue/UserValue, passing it from
// security.Middleware to the downstream file handler so that a request is
// fully validated at most once.
const safePathUserValueKey = "__safePath"

// SafePathFromCtx retrieves the absolute filesystem path that
// security.Middleware fully validated (symlinks resolved) during this request.
//
// Returns ("", false) when the middleware answered from its PathCache — it
// then ran only the lexical checks, because a cached verdict may predate a
// symlink swap — or when the middleware is not in the chain. Callers must
// then validate the path themselves (RevalidatePath) before touching the
// filesystem.
func SafePathFromCtx(ctx *fasthttp.RequestCtx) (string, bool) {
	v, ok := ctx.UserValue(safePathUserValueKey).(string)
	return v, ok && v != ""
}

// ---------------------------------------------------------------------------
// PathCache — bounded LRU cache for urlPath → safePath lookups
// ---------------------------------------------------------------------------

// DefaultPathCacheSize is the default maximum number of entries in the
// PathCache. 10 000 entries ≈ 1–2 MB of memory for typical URL paths.
const DefaultPathCacheSize = 10_000

// PathCache caches the results of PathSafe so that repeated requests for the
// same URL path skip the filesystem syscalls (filepath.EvalSymlinks).
// It is safe for concurrent use.
//
// A cached verdict is only a hint that the path was safe when it was
// validated: the filesystem can change afterwards (e.g. a file replaced by a
// symlink leaving the root). security.Middleware uses a hit to skip symlink
// resolution, which is sound only because the file handler serves such a
// request from its in-memory cache or re-validates it (RevalidatePath) before
// any disk access. Never open a file on the strength of a PathCache entry.
//
// SEC-001: Uses a bounded LRU cache (hashicorp/golang-lru) instead of an
// unbounded sync.Map to prevent memory exhaustion via cache-flooding attacks.
type PathCache struct {
	cache *lru.Cache[string, string]
}

// NewPathCache creates a new PathCache with the given maximum number of entries.
// When the cache is full, the least-recently-used entry is evicted.
func NewPathCache(maxEntries int) *PathCache {
	if maxEntries <= 0 {
		maxEntries = DefaultPathCacheSize
	}
	c, _ := lru.New[string, string](maxEntries)
	return &PathCache{cache: c}
}

// Lookup returns the cached safe path for urlPath, or ("", false) on miss.
func (pc *PathCache) Lookup(urlPath string) (string, bool) {
	return pc.cache.Get(urlPath)
}

// lookup is Lookup that treats a nil cache as always missing.
func (pc *PathCache) lookup(urlPath string) (string, bool) {
	if pc == nil {
		return "", false
	}
	return pc.cache.Get(urlPath)
}

// Store records a urlPath → safePath mapping in the cache.
func (pc *PathCache) Store(urlPath, safePath string) {
	pc.cache.Add(urlPath, safePath)
}

// Remove drops the entry for urlPath, if any.
func (pc *PathCache) Remove(urlPath string) {
	pc.cache.Remove(urlPath)
}

// Flush removes all entries from the cache. Call this on SIGHUP alongside
// the file cache flush to ensure stale path mappings don't persist.
func (pc *PathCache) Flush() {
	pc.cache.Purge()
}

// PreWarm populates the cache for a set of known URL paths by running each
// through PathSafe. Paths that fail validation are silently skipped.
//
// root may be relative (the default files.root is "./public"); it is
// resolved with ResolveRoot first so that only absolute, symlink-resolved
// paths are stored.
func (pc *PathCache) PreWarm(paths []string, root string, blockDotfiles bool) {
	realRoot, err := ResolveRoot(root)
	if err != nil {
		return
	}
	for _, urlPath := range paths {
		safePath, exists, err := resolveSafe(urlPath, realRoot, blockDotfiles)
		if err == nil && exists {
			pc.cache.Add(urlPath, safePath)
		}
	}
}

// ResolveRoot returns root as an absolute path with symlinks resolved, which
// is the form the path checks compare against. A root that does not exist
// (or cannot be resolved) is returned as an absolute path. It fails only when
// root cannot be made absolute.
func ResolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// Len returns the number of entries in the cache.
func (pc *PathCache) Len() int {
	return pc.cache.Len()
}

// ---------------------------------------------------------------------------
// PathSafe
// ---------------------------------------------------------------------------

// PathSafe validates and resolves urlPath relative to absRoot.
// absRoot must already be an absolute, cleaned path (use filepath.Abs once at
// startup). The function performs the following checks in order:
//  1. Rejects paths containing null bytes.
//  2. Cleans the URL path with path.Clean.
//  3. Verifies the resolved path is inside absRoot.
//  4. Blocks any component of the cleaned URL path starting with "." when
//     blockDotfiles is true.
//  5. Resolves symlinks via filepath.EvalSymlinks and re-verifies the target
//     is still inside absRoot (prevents symlink escape attacks) and, when
//     blockDotfiles is true, has no component starting with ".". For paths
//     that do not exist yet (e.g. not-found pages), the unresolved candidate
//     is returned — it has already passed the checks above.
//
// Steps 1-4 are the lexical checks (see CheckLexical).
//
// On success it returns the absolute filesystem path. On failure it returns
// one of the sentinel errors (ErrNullByte, ErrPathTraversal, ErrDotfile).
func PathSafe(urlPath, absRoot string, blockDotfiles bool) (string, error) {
	p, _, err := pathSafe(urlPath, absRoot, blockDotfiles)
	return p, err
}

// RevalidatePath fully validates urlPath like PathSafe and refreshes pc to
// match the result: the entry is stored when the path exists and is safe, and
// removed otherwise. pc may be nil.
//
// realRoot must already be absolute and symlink-resolved (see ResolveRoot);
// unlike PathSafe it is not resolved again on every call. An unresolved root
// fails closed: existing paths are reported as ErrPathTraversal.
//
// Call it before any filesystem access for a request whose path was not
// validated by security.Middleware during the same request (SafePathFromCtx
// reports false). A PathCache entry is never a substitute: it may predate a
// symlink swap.
func RevalidatePath(pc *PathCache, urlPath, realRoot string, blockDotfiles bool) (string, error) {
	p, exists, err := resolveSafe(urlPath, realRoot, blockDotfiles)
	if pc != nil {
		if err == nil && exists {
			pc.Store(urlPath, p)
		} else {
			pc.Remove(urlPath)
		}
	}
	return p, err
}

// CheckLexical runs the checks of PathSafe that need no filesystem access:
// null bytes, containment of the cleaned path in root, and (when
// blockDotfiles is true) dot-prefixed components of the cleaned URL path.
// root must be an absolute, cleaned path.
//
// It does not resolve symlinks, so it never replaces PathSafe before a file
// is opened; it is what must still run on every request when the response
// comes from memory. It does not allocate for plain paths such as "/a/b.js".
func CheckLexical(urlPath, root string, blockDotfiles bool) error {
	if isPlainPath(urlPath) {
		return nil
	}
	_, err := lexicalCandidate(urlPath, root, blockDotfiles)
	return err
}

// isPlainPath reports whether p is an absolute URL path with no NUL byte, no
// backslash, no empty segment and no segment starting with '.'. Such a path
// stays inside any root once joined to it and has no dotfile component, so
// it passes the lexical checks whatever the root and dotfile setting.
func isPlainPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	for i := 1; i < len(p); i++ {
		switch p[i] {
		case 0, '\\':
			return false
		case '/', '.':
			if p[i-1] == '/' {
				return false
			}
		}
	}
	return true
}

// lexicalCandidate performs the lexical checks and returns the unresolved
// filesystem path for urlPath under root.
func lexicalCandidate(urlPath, root string, blockDotfiles bool) (string, error) {
	// Reject null bytes — they can be used to truncate paths in C-based syscalls.
	if strings.IndexByte(urlPath, 0) >= 0 {
		return "", ErrNullByte
	}
	// Normalise the URL-style path, then build the candidate filesystem path;
	// filepath.Join cleans away anything left (e.g. "\.." on Windows).
	cleanURL := path.Clean("/" + urlPath)
	candidate := filepath.Join(root, filepath.FromSlash(cleanURL))
	if !within(candidate, root) {
		return "", ErrPathTraversal
	}
	if blockDotfiles && hasDotSegment(candidate[len(root):]) {
		return "", ErrDotfile
	}
	return candidate, nil
}

// within reports whether p is root or lies below it. Comparing against root
// plus a separator prevents "/root" from matching "/rootsuffix".
func within(p, root string) bool {
	if !strings.HasPrefix(p, root) {
		return false
	}
	if len(p) == len(root) || strings.HasSuffix(root, string(filepath.Separator)) {
		return true
	}
	return os.IsPathSeparator(p[len(root)])
}

// HasDotSegment reports whether any component of the relative filesystem
// path rel starts with '.'. It is the dotfile rule PathSafe applies (to the
// path relative to the root) when dotfiles are blocked.
func HasDotSegment(rel string) bool {
	return hasDotSegment(rel)
}

// hasDotSegment is HasDotSegment.
func hasDotSegment(rel string) bool {
	for i := 0; i < len(rel); i++ {
		if rel[i] == '.' && (i == 0 || os.IsPathSeparator(rel[i-1])) {
			return true
		}
	}
	return false
}

// pathSafe is PathSafe plus an existence flag: exists is true only when the
// path was fully resolved with EvalSymlinks. Results for paths that did not
// exist must not be memoised because the filesystem can change afterwards.
//
// It resolves absRoot on every call; callers holding a root resolved once at
// startup use resolveSafe and skip that EvalSymlinks walk.
func pathSafe(urlPath, absRoot string, blockDotfiles bool) (resolved string, exists bool, err error) {
	// Resolve symlinks in absRoot itself so that comparisons below use the
	// real canonical path. This is important on platforms like macOS where
	// /tmp is a symlink to /private/tmp. If EvalSymlinks fails (root doesn't
	// exist) we keep the original absRoot.
	realRoot := absRoot
	if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		realRoot = r
	}
	return resolveSafe(urlPath, realRoot, blockDotfiles)
}

// resolveSafe is pathSafe for a root whose symlinks are already resolved.
func resolveSafe(urlPath, realRoot string, blockDotfiles bool) (resolved string, exists bool, err error) {
	candidate, err := lexicalCandidate(urlPath, realRoot, blockDotfiles)
	if err != nil {
		return "", false, err
	}

	// Resolve symlinks and re-verify the real target is inside realRoot.
	// This prevents a symlink inside the root pointing outside it.
	// If the path does not exist (ENOENT) we skip resolution — the candidate
	// has already passed the lexical checks.
	resolved, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		if !os.IsNotExist(err) {
			// Unexpected error (permission denied, etc.) — treat as forbidden.
			return "", false, ErrPathTraversal
		}
		// File does not exist — use the unresolved candidate.
		return candidate, false, nil
	}
	if !within(resolved, realRoot) {
		return "", false, ErrPathTraversal
	}
	// A symlink may point at a dotfile under a plain name.
	if blockDotfiles && hasDotSegment(resolved[len(realRoot):]) {
		return "", false, ErrDotfile
	}
	return resolved, true, nil
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// Middleware returns a fasthttp.RequestHandler that validates the request path
// and sets security response headers before delegating to next.
// It returns 400 for null bytes, 403 for path traversal and dotfile attempts,
// and 405 for disallowed HTTP methods.
//
// absRoot is computed once at construction time (via filepath.Abs +
// filepath.EvalSymlinks) and reused for every request, eliminating the
// per-request syscall overhead. A path fully validated during the request is
// stored in the request context via SetUserValue so downstream handlers can
// retrieve it with SafePathFromCtx instead of calling PathSafe a second time.
//
// An optional *PathCache may be provided so that repeated requests for the
// same URL path skip symlink resolution here; they still get the lexical
// checks. Downstream handlers must then validate the path themselves before
// any filesystem access (see SafePathFromCtx). Pass nil (or omit) to disable
// path caching.
func Middleware(cfg *config.SecurityConfig, root string, next fasthttp.RequestHandler, pc ...*PathCache) fasthttp.RequestHandler {
	// Pre-compute security headers at construction time (PERF-002).
	// These are set on every response via direct header calls.
	staticHeaders := ResponseHeaders(cfg)

	// Resolve absRoot once at startup — not on every request. Symlinks in the
	// root itself are resolved so the prefix check in PathSafe uses the
	// canonical real path (important on macOS where /tmp → /private/tmp).
	absRoot, err := ResolveRoot(root)
	if err != nil {
		// If we can't resolve the root at startup the server is misconfigured;
		// return a handler that always responds 500, still with the security
		// headers and those set by outer layers (HTTP-F4).
		return func(ctx *fasthttp.RequestCtx) {
			for _, h := range staticHeaders {
				ctx.Response.Header.Set(h.Key, h.Value)
			}
			sendError(ctx, "Internal Server Error: invalid root path", fasthttp.StatusInternalServerError)
		}
	}

	// Extract optional path cache.
	var pathCache *PathCache
	if len(pc) > 0 {
		pathCache = pc[0]
	}

	return func(ctx *fasthttp.RequestCtx) {
		// Set security headers on every response, including errors (SEC-006).
		for _, h := range staticHeaders {
			ctx.Response.Header.Set(h.Key, h.Value)
		}

		// SEC-010: Check the raw request URI for null bytes. fasthttp's
		// ctx.Path() strips null bytes during URI parsing, so we must
		// inspect the raw URI to detect them.
		if strings.ContainsRune(string(ctx.Request.RequestURI()), 0) {
			sendError(ctx, "Bad Request: path contains null byte", fasthttp.StatusBadRequest)
			return
		}

		// Reject disallowed HTTP methods (SEC-009). Only GET, HEAD, and OPTIONS
		// are valid for a static file server; reject TRACE, PUT, POST, DELETE etc.
		if !ctx.IsGet() && !ctx.IsHead() && !ctx.IsOptions() {
			ctx.Response.Header.Set("Allow", allowedMethods)
			sendError(ctx, "Method Not Allowed", fasthttp.StatusMethodNotAllowed)
			return
		}

		urlPath := string(ctx.Path())

		// The lexical checks (null byte, traversal, dotfile components) run on
		// every request. Full validation (symlink resolution) is skipped on a
		// PathCache hit: the file handler then serves the response from its
		// in-memory cache, which was filled while the path was valid, or
		// re-validates the path before touching the disk (FS-1).
		pathErr := CheckLexical(urlPath, absRoot, cfg.BlockDotfiles)
		var safePath string
		validated := false
		if pathErr == nil {
			if _, hit := pathCache.lookup(urlPath); !hit {
				var exists bool
				safePath, exists, pathErr = resolveSafe(urlPath, absRoot, cfg.BlockDotfiles)
				validated = pathErr == nil
				// Cache only fully resolved paths: a path that did not exist may
				// later become a symlink escaping the root.
				if validated && exists && pathCache != nil {
					pathCache.Store(urlPath, safePath)
				}
			}
		}
		if pathErr != nil {
			switch {
			case errors.Is(pathErr, ErrNullByte):
				sendError(ctx, "Bad Request: "+pathErr.Error(), fasthttp.StatusBadRequest)
			case errors.Is(pathErr, ErrPathTraversal), errors.Is(pathErr, ErrDotfile):
				sendError(ctx, "Forbidden: "+pathErr.Error(), fasthttp.StatusForbidden)
			default:
				sendError(ctx, "Forbidden", fasthttp.StatusForbidden)
			}
			return
		}

		if ctx.IsOptions() {
			ctx.Response.Header.Set("Allow", allowedMethods)
		}

		// Handle CORS preflight.
		if len(cfg.CORSOrigins) > 0 {
			wildcard := isWildcard(cfg.CORSOrigins)
			if !wildcard {
				// With a specific list the response depends on the Origin
				// header (ACAO is present only for allowed origins), so every
				// response varies on it -- including those to requests with
				// no Origin or a disallowed one. Otherwise a shared cache can
				// store the variant without ACAO and serve it to an allowed
				// origin (HTTP-F5).
				headers.AddVary(&ctx.Response.Header, "Origin")
			}
			origin := string(ctx.Request.Header.Peek("Origin"))
			if origin != "" {
				if wildcard {
					// Wildcard: emit literal "*" — never reflect the origin value.
					// SEC-012: Do NOT set Vary: Origin when using wildcard because all
					// origins receive the identical "Access-Control-Allow-Origin: *" header.
					// Adding Vary: Origin would cause caches to store per-origin variants
					// unnecessarily, wasting storage and reducing hit rates.
					ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
					if ctx.IsOptions() {
						ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
						ctx.Response.Header.Set("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Range")
						ctx.Response.Header.Set("Access-Control-Max-Age", "86400")
						ctx.SetStatusCode(fasthttp.StatusNoContent)
						return
					}
				} else if originAllowed(origin, cfg.CORSOrigins) {
					ctx.Response.Header.Set("Access-Control-Allow-Origin", origin)
					if ctx.IsOptions() {
						ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
						ctx.Response.Header.Set("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Range")
						ctx.Response.Header.Set("Access-Control-Max-Age", "86400")
						ctx.SetStatusCode(fasthttp.StatusNoContent)
						return
					}
				}
			}
		}

		// Plain OPTIONS (not a handled CORS preflight): answer directly with
		// Allow and no body (RFC 9110 §9.3.7).
		if ctx.IsOptions() {
			ctx.SetStatusCode(fasthttp.StatusNoContent)
			return
		}

		// Hand a path validated during this request to the file handler via
		// SafePathFromCtx so it is not validated a second time (P-F7). After
		// a PathCache hit there is no such path: the handler serves from its
		// file cache or re-validates.
		if validated {
			ctx.SetUserValue(safePathUserValueKey, safePath)
		} else {
			// Never let a value an outer layer stored under this key pass
			// for one validated here.
			ctx.RemoveUserValue(safePathUserValueKey)
		}

		next(ctx)
	}
}

// HeaderPair is a response header name and value.
type HeaderPair struct {
	Key, Value string
}

// ResponseHeaders returns the security headers Middleware sets on every
// response for cfg (SEC-006): X-Content-Type-Options and X-Frame-Options,
// plus Content-Security-Policy, Referrer-Policy and Permissions-Policy when
// configured. Responses written outside Middleware, such as the parse
// errors fasthttp answers itself, use it to carry the same headers.
func ResponseHeaders(cfg *config.SecurityConfig) []HeaderPair {
	hs := make([]HeaderPair, 0, 5)
	hs = append(hs,
		HeaderPair{"X-Content-Type-Options", "nosniff"},
		HeaderPair{"X-Frame-Options", "SAMEORIGIN"})
	if cfg.CSP != "" {
		hs = append(hs, HeaderPair{"Content-Security-Policy", cfg.CSP})
	}
	if cfg.ReferrerPolicy != "" {
		hs = append(hs, HeaderPair{"Referrer-Policy", cfg.ReferrerPolicy})
	}
	if cfg.PermissionsPolicy != "" {
		hs = append(hs, HeaderPair{"Permissions-Policy", cfg.PermissionsPolicy})
	}
	return hs
}

// sendError writes a plain-text error response without clearing the headers
// already set: the security headers, and those of outer layers such as CORS
// or HSTS. fasthttp's ctx.Error() resets all headers, so it is not used
// (SEC-006, HTTP-F4).
func sendError(ctx *fasthttp.RequestCtx, msg string, code int) {
	ctx.SetStatusCode(code)
	ctx.SetBodyString(msg)
	ctx.Response.Header.Set("Content-Type", "text/plain; charset=utf-8")
}

// allowedMethods is the value of the Allow header.
const allowedMethods = "GET, HEAD, OPTIONS"

// isWildcard reports whether the allowed list consists solely of "*".
func isWildcard(allowed []string) bool {
	return len(allowed) == 1 && allowed[0] == "*"
}

// originAllowed reports whether origin is in the allowed list.
// Wildcard matching is handled separately by isWildcard; this function
// only matches specific origins.
func originAllowed(origin string, allowed []string) bool {
	for _, o := range allowed {
		if o == origin {
			return true
		}
	}
	return false
}
