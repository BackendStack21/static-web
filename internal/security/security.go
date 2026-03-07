// Package security provides path safety checks and HTTP security middleware
// for the static web server.
package security

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/static-web/server/internal/config"
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

// safePathKey is the context key used to pass the validated absolute path
// from security.Middleware to the downstream file handler, avoiding a second
// PathSafe call and the two filepath.Abs syscalls it entails.
type safePathKey struct{}

// SafePathFromContext retrieves the pre-validated absolute filesystem path
// that security.Middleware stored in the request context.
// Returns ("", false) when the value is absent (e.g. in unit tests that bypass
// the security middleware).
func SafePathFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(safePathKey{}).(string)
	return v, ok && v != ""
}

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

// Middleware returns an http.Handler that validates the request path and sets
// security response headers before delegating to next.
// It returns 400 for null bytes, 403 for path traversal and dotfile attempts,
// and 405 for disallowed HTTP methods.
//
// absRoot is computed once at construction time (via filepath.Abs +
// filepath.EvalSymlinks) and reused for every request, eliminating the
// per-request syscall overhead. The resolved safe path is stored in the
// request context so downstream handlers can retrieve it with
// SafePathFromContext instead of calling PathSafe a second time.
func Middleware(cfg *config.SecurityConfig, root string, next http.Handler) http.Handler {
	// Resolve absRoot once at startup — not on every request.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		// If we can't resolve the root at startup the server is misconfigured;
		// return a handler that always responds 500.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Internal Server Error: invalid root path", http.StatusInternalServerError)
		})
	}
	// Resolve symlinks in the root itself so the prefix check in PathSafe
	// uses the canonical real path (important on macOS where /tmp → /private/tmp).
	if real, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = real
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set security headers on every response, including errors (SEC-006).
		setSecurityHeaders(w, cfg)

		// Reject disallowed HTTP methods (SEC-009). Only GET, HEAD, and OPTIONS
		// are valid for a static file server; reject TRACE, PUT, POST, DELETE etc.
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// allowed — fall through
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		safePath, err := PathSafe(r.URL.Path, absRoot, cfg.BlockDotfiles)
		if err != nil {
			switch {
			case errors.Is(err, ErrNullByte):
				http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
			case errors.Is(err, ErrPathTraversal), errors.Is(err, ErrDotfile):
				http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
			default:
				http.Error(w, "Forbidden", http.StatusForbidden)
			}
			return
		}

		// Handle CORS preflight.
		if len(cfg.CORSOrigins) > 0 {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if isWildcard(cfg.CORSOrigins) {
					// Wildcard: emit literal "*" — never reflect the origin value.
					// Do NOT set Vary: Origin since all origins receive the same header.
					w.Header().Set("Access-Control-Allow-Origin", "*")
					if r.Method == http.MethodOptions {
						w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
						w.Header().Set("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Range")
						w.Header().Set("Access-Control-Max-Age", "86400")
						w.WriteHeader(http.StatusNoContent)
						return
					}
				} else if originAllowed(origin, cfg.CORSOrigins) {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					if r.Method == http.MethodOptions {
						w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
						w.Header().Set("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Range")
						w.Header().Set("Access-Control-Max-Age", "86400")
						w.WriteHeader(http.StatusNoContent)
						return
					}
				}
			}
		}

		// Store the validated path in context so the file handler can skip its
		// own PathSafe call entirely.
		ctx := context.WithValue(r.Context(), safePathKey{}, safePath)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// setSecurityHeaders writes hardened HTTP security headers to the response.
func setSecurityHeaders(w http.ResponseWriter, cfg *config.SecurityConfig) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")

	if cfg.CSP != "" {
		w.Header().Set("Content-Security-Policy", cfg.CSP)
	}
	if cfg.ReferrerPolicy != "" {
		w.Header().Set("Referrer-Policy", cfg.ReferrerPolicy)
	}
	if cfg.PermissionsPolicy != "" {
		w.Header().Set("Permissions-Policy", cfg.PermissionsPolicy)
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
