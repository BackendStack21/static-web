// Package security provides path safety checks and HTTP security middleware
// for the static web server.
package security

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BackendStack21/static-web/internal/config"
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
// security.Middleware to the downstream file handler and avoiding a second
// PathSafe call and the two filepath.Abs syscalls it entails.
const safePathUserValueKey = "__safePath"

// SafePathFromCtx retrieves the pre-validated absolute filesystem path
// that security.Middleware stored in the request context via SetUserValue.
// Returns ("", false) when the value is absent (e.g. in unit tests that bypass
// the security middleware).
func SafePathFromCtx(ctx *fasthttp.RequestCtx) (string, bool) {
	v, ok := ctx.UserValue(safePathUserValueKey).(string)
	return v, ok && v != ""
}

// ---------------------------------------------------------------------------
// PathCache — caches urlPath → safePath to avoid per-request syscalls
// ---------------------------------------------------------------------------

// PathCache caches the results of PathSafe so that repeated requests for the
// same URL path skip the filesystem syscalls (filepath.EvalSymlinks).
// It is safe for concurrent use.
type PathCache struct {
	m sync.Map // urlPath (string) → safePath (string)
}

// NewPathCache creates a new empty PathCache.
func NewPathCache() *PathCache {
	return &PathCache{}
}

// Lookup returns the cached safe path for urlPath, or ("", false) on miss.
func (pc *PathCache) Lookup(urlPath string) (string, bool) {
	v, ok := pc.m.Load(urlPath)
	if !ok {
		return "", false
	}
	return v.(string), true
}

// Store records a urlPath → safePath mapping in the cache.
func (pc *PathCache) Store(urlPath, safePath string) {
	pc.m.Store(urlPath, safePath)
}

// Flush removes all entries from the cache. Call this on SIGHUP alongside
// the file cache flush to ensure stale path mappings don't persist.
func (pc *PathCache) Flush() {
	pc.m.Range(func(key, _ any) bool {
		pc.m.Delete(key)
		return true
	})
}

// PreWarm populates the cache for a set of known URL paths by running each
// through PathSafe. Paths that fail validation are silently skipped.
func (pc *PathCache) PreWarm(paths []string, absRoot string, blockDotfiles bool) {
	for _, urlPath := range paths {
		safePath, err := PathSafe(urlPath, absRoot, blockDotfiles)
		if err == nil {
			pc.m.Store(urlPath, safePath)
		}
	}
}

// Len returns the number of entries in the cache.
func (pc *PathCache) Len() int {
	n := 0
	pc.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
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
//  4. Resolves symlinks via filepath.EvalSymlinks and re-verifies the target
//     is still inside absRoot (prevents symlink escape attacks). For paths that
//     do not exist yet (e.g. not-found pages), the unresolved candidate is
//     returned — it has already passed the prefix check.
//  5. Blocks any path component starting with "." when blockDotfiles is true.
//
// On success it returns the absolute filesystem path. On failure it returns
// one of the sentinel errors (ErrNullByte, ErrPathTraversal, ErrDotfile).
func PathSafe(urlPath, absRoot string, blockDotfiles bool) (string, error) {
	// 1. Reject null bytes — they can be used to truncate paths in C-based syscalls.
	if strings.ContainsRune(urlPath, 0) {
		return "", ErrNullByte
	}

	// Resolve symlinks in absRoot itself so that comparisons below use the
	// real canonical path. This is important on platforms like macOS where
	// /tmp is a symlink to /private/tmp. If EvalSymlinks fails (root doesn't
	// exist) we keep the original absRoot.
	realRoot := absRoot
	if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		realRoot = r
	}

	// 2. Normalise the URL-style path.
	cleanURL := path.Clean("/" + urlPath)

	// 3. Build the candidate filesystem path.
	//    absRoot is already absolute; filepath.Join cleans away any remaining "..".
	candidate := filepath.Join(realRoot, filepath.FromSlash(cleanURL))

	// Ensure candidate is inside realRoot.
	// Add a trailing separator to prevent prefix collisions like
	// "/root" matching "/rootsuffix".
	rootWithSep := realRoot
	if !strings.HasSuffix(rootWithSep, string(filepath.Separator)) {
		rootWithSep += string(filepath.Separator)
	}

	if candidate != realRoot && !strings.HasPrefix(candidate, rootWithSep) {
		return "", ErrPathTraversal
	}

	// 4. Resolve symlinks and re-verify the real target is inside realRoot.
	//    This prevents a symlink inside the root pointing outside it.
	//    If the path does not exist (ENOENT) we skip resolution — the candidate
	//    has already passed the prefix check above.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if !os.IsNotExist(err) {
			// Unexpected error (permission denied, etc.) — treat as forbidden.
			return "", ErrPathTraversal
		}
		// File does not exist — use the unresolved candidate.
		resolved = candidate
	} else {
		// Re-check that the resolved real path is still inside realRoot.
		if resolved != realRoot && !strings.HasPrefix(resolved, rootWithSep) {
			return "", ErrPathTraversal
		}
	}

	// 5. Block dotfile components.
	if blockDotfiles {
		rel := strings.TrimPrefix(resolved, realRoot)
		for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
			if segment == "" {
				continue
			}
			if strings.HasPrefix(segment, ".") {
				return "", ErrDotfile
			}
		}
	}

	return resolved, nil
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
// per-request syscall overhead. The resolved safe path is stored in the
// request context via SetUserValue so downstream handlers can retrieve it with
// SafePathFromCtx instead of calling PathSafe a second time.
//
// An optional *PathCache may be provided to cache PathSafe results so that
// repeated requests for the same URL path skip the filesystem syscalls
// entirely. Pass nil (or omit) to disable path caching.
func Middleware(cfg *config.SecurityConfig, root string, next fasthttp.RequestHandler, pc ...*PathCache) fasthttp.RequestHandler {
	// Resolve absRoot once at startup — not on every request.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		// If we can't resolve the root at startup the server is misconfigured;
		// return a handler that always responds 500.
		return func(ctx *fasthttp.RequestCtx) {
			ctx.Error("Internal Server Error: invalid root path", fasthttp.StatusInternalServerError)
		}
	}
	// Resolve symlinks in the root itself so the prefix check in PathSafe
	// uses the canonical real path (important on macOS where /tmp → /private/tmp).
	if real, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = real
	}

	// Extract optional path cache.
	var pathCache *PathCache
	if len(pc) > 0 {
		pathCache = pc[0]
	}

	// Pre-compute security headers at construction time (PERF-002).
	// These are set on every response via direct header calls.
	type headerPair struct {
		key, value string
	}
	staticHeaders := make([]headerPair, 0, 5)
	staticHeaders = append(staticHeaders, headerPair{"X-Content-Type-Options", "nosniff"})
	staticHeaders = append(staticHeaders, headerPair{"X-Frame-Options", "SAMEORIGIN"})
	if cfg.CSP != "" {
		staticHeaders = append(staticHeaders, headerPair{"Content-Security-Policy", cfg.CSP})
	}
	if cfg.ReferrerPolicy != "" {
		staticHeaders = append(staticHeaders, headerPair{"Referrer-Policy", cfg.ReferrerPolicy})
	}
	if cfg.PermissionsPolicy != "" {
		staticHeaders = append(staticHeaders, headerPair{"Permissions-Policy", cfg.PermissionsPolicy})
	}

	return func(ctx *fasthttp.RequestCtx) {
		// Set security headers on every response, including errors (SEC-006).
		for _, h := range staticHeaders {
			ctx.Response.Header.Set(h.key, h.value)
		}

		// sendError writes an error response without clearing previously set
		// security headers. fasthttp's ctx.Error() resets all headers, so we
		// use SetStatusCode + SetBodyString instead (SEC-006).
		sendError := func(msg string, code int) {
			ctx.SetStatusCode(code)
			ctx.SetBodyString(msg)
			ctx.Response.Header.Set("Content-Type", "text/plain; charset=utf-8")
		}

		// SEC-010: Check the raw request URI for null bytes. fasthttp's
		// ctx.Path() strips null bytes during URI parsing, so we must
		// inspect the raw URI to detect them.
		if strings.ContainsRune(string(ctx.Request.RequestURI()), 0) {
			sendError("Bad Request: path contains null byte", fasthttp.StatusBadRequest)
			return
		}

		// Reject disallowed HTTP methods (SEC-009). Only GET, HEAD, and OPTIONS
		// are valid for a static file server; reject TRACE, PUT, POST, DELETE etc.
		if !ctx.IsGet() && !ctx.IsHead() && !ctx.IsOptions() {
			sendError("Method Not Allowed", fasthttp.StatusMethodNotAllowed)
			return
		}

		urlPath := string(ctx.Path())

		// Fast path: check the path cache before hitting the filesystem.
		var safePath string
		if pathCache != nil {
			if cached, ok := pathCache.Lookup(urlPath); ok {
				safePath = cached
			}
		}

		if safePath == "" {
			var pathErr error
			safePath, pathErr = PathSafe(urlPath, absRoot, cfg.BlockDotfiles)
			if pathErr != nil {
				switch {
				case errors.Is(pathErr, ErrNullByte):
					sendError("Bad Request: "+pathErr.Error(), fasthttp.StatusBadRequest)
				case errors.Is(pathErr, ErrPathTraversal), errors.Is(pathErr, ErrDotfile):
					sendError("Forbidden: "+pathErr.Error(), fasthttp.StatusForbidden)
				default:
					sendError("Forbidden", fasthttp.StatusForbidden)
				}
				return
			}

			// Cache the successful result.
			if pathCache != nil {
				pathCache.Store(urlPath, safePath)
			}
		}

		// Handle CORS preflight.
		if len(cfg.CORSOrigins) > 0 {
			origin := string(ctx.Request.Header.Peek("Origin"))
			if origin != "" {
				if isWildcard(cfg.CORSOrigins) {
					// Wildcard: emit literal "*" — never reflect the origin value.
					// Do NOT set Vary: Origin since all origins receive the same header.
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
					ctx.Response.Header.Set("Vary", "Origin")
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

		// Store the validated path in context so downstream handlers can
		// retrieve it via SafePathFromCtx. On the hot path, the file handler
		// reads from PathCache directly, so this UserValue write is only
		// reached for cache-miss requests where the path wasn't in PathCache.
		// PERF-001: skip context allocation when the path is already cached.
		if pathCache == nil {
			ctx.SetUserValue(safePathUserValueKey, safePath)
		}

		next(ctx)
	}
}

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
