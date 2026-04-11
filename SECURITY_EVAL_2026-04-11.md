# Security Audit Report — static-web

**Project:** BackendStack21/static-web
**Language:** Go 1.26
**Framework:** valyala/fasthttp
**Audit Date:** April 11, 2026
**Overall Grade:** **B+**
**Findings:** 0 CRITICAL · 1 HIGH · 5 MEDIUM · 5 LOW · 5 INFO

---

## Executive Summary

`static-web` demonstrates strong security fundamentals — multi-layer path traversal prevention, XSS-safe templating, excellent TLS configuration, and a CI pipeline with `govulncheck` and race detection. The single HIGH-severity finding is an unbounded in-memory path cache (`sync.Map`) that enables a straightforward memory exhaustion DoS. Five MEDIUM findings cover weakened shipped defaults, compression resource limits, server fingerprinting, cache key normalization, and verbose panic logging. No critical vulnerabilities were found.

---

## Findings Summary

| #       | Finding                                        | Severity       | Category             | File                            |
| ------- | ---------------------------------------------- | -------------- | -------------------- | ------------------------------- |
| SEC-001 | Unbounded `PathCache` (DoS)                    | **HIGH**       | Resource Exhaustion  | `security/security.go:49–70`    |
| SEC-002 | Shipped `config.toml` weakens code defaults    | MEDIUM         | Misconfiguration     | `config.toml:28–38`             |
| SEC-003 | Full stack traces logged on panic              | MEDIUM         | Information Disclosure | `handler/middleware.go:121–132` |
| SEC-004 | Static multipart range boundary                | MEDIUM         | Fingerprinting       | `handler/file.go:615, 659`      |
| SEC-005 | No max body size for gzip compression          | MEDIUM         | Resource Exhaustion  | `compress/compress.go:170–187`  |
| SEC-006 | Cache keys not explicitly normalized           | MEDIUM         | Access Control       | `headers/headers.go:19–33`      |
| SEC-007 | Server name disclosed in headers               | LOW            | Fingerprinting       | `server/server.go:70, 112`      |
| SEC-008 | Unsanitized paths in log output                | LOW            | Log Injection        | `handler/middleware.go:113–115` |
| SEC-009 | Deprecated `PreferServerCipherSuites`          | LOW            | Cryptography         | `server/server.go:93`           |
| SEC-010 | Template execution error silently discarded    | LOW            | Error Handling       | `handler/dirlist.go:191`        |
| SEC-011 | Large files read entirely into memory          | LOW            | Resource Exhaustion  | `handler/file.go:338–377`       |
| SEC-012 | CORS wildcard `Vary` header note               | INFO           | CORS                 | `security/security.go:313`      |
| SEC-013 | ETag truncated to 64 bits                      | INFO           | Cryptography         | `handler/file.go:480–483`       |
| SEC-014 | `MaxRequestBodySize: 0` uses fasthttp default  | INFO           | Misconfiguration     | `server/server.go:74`           |
| SEC-015 | No built-in rate limiting                       | INFO           | Resource Exhaustion  | Architectural                   |
| SEC-016 | Preload walker doesn't validate symlink targets | INFO          | Access Control       | `cache/preload.go:74–158`       |

---

## Detailed Findings

### SEC-001: Unbounded Path Validation Cache (Denial of Service)

| Attribute   | Value                                                  |
| ----------- | ------------------------------------------------------ |
| **Severity**| **HIGH**                                               |
| **CWE**     | CWE-400 (Uncontrolled Resource Consumption)            |
| **OWASP**   | A05:2021 — Security Misconfiguration                   |
| **File**    | `internal/security/security.go:49–70`                  |

#### Description

The `PathCache` struct wraps a bare `sync.Map` with no upper bound on the number of entries. Every unique URL path that passes `PathSafe` validation is unconditionally cached (line 304 of `security.go`). Because `PathSafe` successfully validates *non-existent* file paths (they pass the prefix check and return the unresolved candidate at line 165), an attacker doesn't even need to target real files — any fabricated path like `/aaa`, `/aab`, `/aac`, … will be validated, cached, and never evicted.

#### Evidence

```go
// security.go:49-51 — no size limit declared
type PathCache struct {
    m sync.Map // urlPath (string) -> safePath (string)
}

// security.go:68-70 — unconditional store, no eviction
func (pc *PathCache) Store(urlPath, safePath string) {
    pc.m.Store(urlPath, safePath)
}

// security.go:302-305 — stored on every cache miss
if pathCache != nil {
    pathCache.Store(urlPath, safePath)
}
```

#### Attack Scenario

1. Attacker scripts HTTP requests to unique, non-existent paths: `GET /rand_000001`, `GET /rand_000002`, …, `GET /rand_99999999`.
2. Each path passes `PathSafe` (it's a valid path that simply doesn't exist on disk).
3. Each path is stored in `sync.Map` — two strings (URL path + resolved filesystem path) per entry.
4. With ~100-byte average key+value per entry, 100 million requests consume ~10 GB of RAM.
5. The `sync.Map` has no eviction, no TTL, no maximum size. Memory grows monotonically until OOM kill.
6. The `Flush()` method (line 74) is only called on SIGHUP — not automatically.

#### Recommendation

Replace `sync.Map` with a bounded LRU cache (the project already depends on `hashicorp/golang-lru/v2`), or only cache paths for files that actually exist on disk:

```go
// Option A: Bounded LRU
import lru "github.com/hashicorp/golang-lru/v2"

type PathCache struct {
    m *lru.Cache[string, string]
}

func NewPathCache(maxEntries int) *PathCache {
    c, _ := lru.New[string, string](maxEntries) // e.g., 65536
    return &PathCache{m: c}
}

// Option B: Only cache existing files (in Middleware, after PathSafe)
if pathCache != nil {
    if _, err := os.Stat(safePath); err == nil {
        pathCache.Store(urlPath, safePath)
    }
}
```

---

### SEC-002: Shipped `config.toml` Weakens Code Defaults

| Attribute   | Value                                                  |
| ----------- | ------------------------------------------------------ |
| **Severity**| MEDIUM                                                 |
| **CWE**     | CWE-1188 (Insecure Default Initialization of Resource) |
| **OWASP**   | A05:2021 — Security Misconfiguration                   |
| **File**    | `config.toml:28–38`                                    |

#### Description

The code in `config.go:147–178` sets strong security defaults (`EnableETags = true`, `CSP = "default-src 'self'"`, `ReferrerPolicy = "strict-origin-when-cross-origin"`, `PermissionsPolicy = "geolocation=(), microphone=(), camera=()"`, `HSTSMaxAge = 31536000`). However, the shipped `config.toml` overrides several of these with weaker values.

#### Evidence

```toml
# config.toml:33 — disables ETag generation
enable_etags = false

# config.toml:38 — empties CSP entirely
csp = ""

# config.toml — MISSING these keys entirely (reset to zero-values):
# referrer_policy = ""          <- code default: "strict-origin-when-cross-origin"
# permissions_policy = ""       <- code default: "geolocation=(), microphone=(), camera=()"
# hsts_max_age = 0              <- code default: 31536000
```

Because `toml.DecodeFile` merges into the struct *after* `applyDefaults` runs (config.go:131–138), any key present in the TOML file replaces the secure code default. Keys absent from the TOML get reset to Go zero values.

#### Attack Scenario

1. Operator deploys with the shipped `config.toml` without reviewing every security field.
2. CSP is empty — no Content-Security-Policy header — XSS payloads injected via user-uploaded HTML files execute freely.
3. ETags disabled — clients can't use `If-None-Match` for conditional requests.
4. No Referrer-Policy — browser uses default (leaks full URL to third parties).
5. No Permissions-Policy — embedded iframes can request geolocation, camera, microphone.
6. No HSTS — first-visit connections on HTTP are not upgraded, enabling SSL-stripping attacks.

#### Recommendation

Update `config.toml` to include all security defaults matching the code:

```toml
[headers]
enable_etags = true

[security]
block_dotfiles = true
directory_listing = false
cors_origins = []
csp = "default-src 'self'"
referrer_policy = "strict-origin-when-cross-origin"
permissions_policy = "geolocation=(), microphone=(), camera=()"
hsts_max_age = 31536000
hsts_include_subdomains = false
```

---

### SEC-003: Full Stack Trace Logged on Panic in Production

| Attribute   | Value                                                        |
| ----------- | ------------------------------------------------------------ |
| **Severity**| MEDIUM                                                       |
| **CWE**     | CWE-209 (Error Message Containing Sensitive Information)     |
| **OWASP**   | A04:2021 — Insecure Design                                   |
| **File**    | `internal/handler/middleware.go:121–132`                      |

#### Description

The `recoveryMiddleware` calls `debug.Stack()` on every panic and logs the full Go stack trace. This trace includes absolute file paths, function names, goroutine IDs, and line numbers — information that aids an attacker in understanding the server's internals. While the stack trace is NOT sent to the client (only "Internal Server Error" is returned), it is an information disclosure risk in logging pipelines.

#### Evidence

```go
// middleware.go:121-132
func recoveryMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
    return func(ctx *fasthttp.RequestCtx) {
        defer func() {
            if rec := recover(); rec != nil {
                stack := debug.Stack()
                log.Printf("PANIC recovered: %v\n%s", rec, stack)
                ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
            }
        }()
        next(ctx)
    }
}
```

#### Attack Scenario

1. Attacker finds a way to trigger a panic (e.g., a malformed Range header causing a slice-bounds-out-of-range).
2. Full stack trace is written to stdout/stderr, which may be forwarded to a centralized logging system.
3. If logs are accessible to a broader team or leak through a log aggregation UI, the stack trace reveals internal file structure, function names, and Go version/module paths.

#### Recommendation

Make stack trace logging configurable, defaulting to a truncated version in production:

```go
func recoveryMiddleware(next fasthttp.RequestHandler, verbose bool) fasthttp.RequestHandler {
    return func(ctx *fasthttp.RequestCtx) {
        defer func() {
            if rec := recover(); rec != nil {
                if verbose {
                    log.Printf("PANIC recovered: %v\n%s", rec, debug.Stack())
                } else {
                    log.Printf("PANIC recovered: %v", rec)
                }
                ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
            }
        }()
        next(ctx)
    }
}
```

---

### SEC-004: Static Multipart Range Boundary Enables Server Fingerprinting

| Attribute   | Value                                              |
| ----------- | -------------------------------------------------- |
| **Severity**| MEDIUM                                             |
| **CWE**     | CWE-200 (Exposure of Sensitive Information)        |
| **OWASP**   | A05:2021 — Security Misconfiguration               |
| **File**    | `internal/handler/file.go:615` and `file.go:659`   |

#### Description

Multi-range responses use a hardcoded boundary string `"static_web_range_boundary"`. This same string appears in two separate functions (`serveRange` and `serveLargeFileRange`). This boundary is globally constant across all responses and all server instances, uniquely identifying the server software.

#### Evidence

```go
// file.go:615
boundary := "static_web_range_boundary"

// file.go:659
boundary := "static_web_range_boundary"
```

#### Attack Scenario

1. Attacker sends a multi-range request: `Range: bytes=0-0,1-1`.
2. Response contains `Content-Type: multipart/byteranges; boundary=static_web_range_boundary`.
3. This uniquely identifies the server software as "static-web" even if the `Server` header is stripped by a reverse proxy.
4. The static boundary also has a theoretical MIME confusion risk if an attacker can control file content containing the exact boundary string.

#### Recommendation

Generate a random boundary per response:

```go
import "crypto/rand"

func randomBoundary() string {
    var buf [16]byte
    _, _ = rand.Read(buf[:])
    return hex.EncodeToString(buf[:])
}

// Usage:
boundary := randomBoundary()
```

---

### SEC-005: No Upper Bound on On-The-Fly Gzip Compression Body Size

| Attribute   | Value                                           |
| ----------- | ----------------------------------------------- |
| **Severity**| MEDIUM                                          |
| **CWE**     | CWE-400 (Uncontrolled Resource Consumption)     |
| **OWASP**   | A05:2021 — Security Misconfiguration            |
| **File**    | `internal/compress/compress.go:170–187`         |

#### Description

The compression middleware checks `len(body) < cfg.MinSize` (minimum threshold) but has no *maximum* threshold. If a large compressible response bypasses the file cache (e.g., `serveLargeFile` serving a 500 MB HTML file), the entire body is gzip-compressed in memory.

#### Evidence

```go
// compress.go:170-187
body := ctx.Response.Body()
if len(body) < cfg.MinSize {
    return
}
// No upper bound check here!

buf := gzipBufPool.Get().(*bytes.Buffer)
buf.Reset()
buf.Grow(len(body) / 2)  // Allocates len(body)/2 upfront

gz := gzipWriterPool.Get().(*gzip.Writer)
gz.Reset(buf)
gz.Write(body)  // Compresses entire body in memory
gz.Close()
```

#### Attack Scenario

1. Operator configures `max_file_size = 1073741824` (1 GB).
2. Attacker requests a 500 MB `.html` file with `Accept-Encoding: gzip`.
3. The file handler reads 500 MB into memory.
4. Compression middleware allocates an additional ~250 MB buffer and compresses.
5. Peak memory usage for this single request: ~750 MB. A handful of concurrent requests exhaust available memory.

#### Recommendation

Add a maximum compression threshold:

```go
const maxCompressSize = 10 * 1024 * 1024 // 10 MB

body := ctx.Response.Body()
if len(body) < cfg.MinSize || len(body) > maxCompressSize {
    return
}
```

---

### SEC-006: Cache Keys Not Explicitly Normalized Before Lookup

| Attribute   | Value                                                         |
| ----------- | ------------------------------------------------------------- |
| **Severity**| MEDIUM                                                        |
| **CWE**     | CWE-706 (Use of Incorrectly-Resolved Name or Reference)      |
| **OWASP**   | A01:2021 — Broken Access Control                              |
| **File**    | `internal/headers/headers.go:19–33` and `cache/cache.go:209`  |

#### Description

Cache keys are derived from `ctx.Path()` (which fasthttp normalizes) and passed through `CacheKeyForPath()`, but this function does NOT call `path.Clean()`. While fasthttp does normalize most paths, edge cases with percent-encoding or unusual Unicode normalization could theoretically produce distinct cache keys that resolve to the same filesystem file.

#### Evidence

```go
// headers.go:19-33
func CacheKeyForPath(urlPath, indexFile string) string {
    // No path.Clean() call
    if urlPath == "" || urlPath == "/" {
        if indexFile == "index.html" {
            return "/index.html"
        }
        return "/" + indexFile
    }
    if strings.HasSuffix(urlPath, "/") {
        return urlPath + indexFile
    }
    return urlPath  // passed through verbatim
}
```

#### Attack Scenario

1. If fasthttp's URI normalization has a bypass, two different URL strings could map to the same file but produce different cache keys.
2. Request A (`/styles/app.css`) is served and cached.
3. Request B (`/styles/./app.css` — if somehow not normalized) would get a cache MISS and be re-read from disk, bypassing cache-based controls.
4. Low probability because fasthttp *does* normalize paths, but defense-in-depth argues for explicit normalization.

#### Recommendation

Apply `path.Clean` in the cache key function:

```go
func CacheKeyForPath(urlPath, indexFile string) string {
    urlPath = path.Clean("/" + urlPath) // explicit normalization
    if indexFile == "" {
        indexFile = "index.html"
    }
    // ... rest of function
}
```

---

### SEC-007: Server Name Disclosed in HTTP Response Headers

| Attribute   | Value                                                              |
| ----------- | ------------------------------------------------------------------ |
| **Severity**| LOW                                                                |
| **CWE**     | CWE-200 (Exposure of Sensitive Information to Unauthorized Actor)  |
| **OWASP**   | A05:2021 — Security Misconfiguration                               |
| **File**    | `internal/server/server.go:70` and `server.go:112`                 |

#### Description

Both the HTTP and HTTPS `fasthttp.Server` instances set `Name: "static-web"`. Fasthttp uses this value to populate the `Server` response header on every response, identifying the software.

#### Evidence

```go
s.http = &fasthttp.Server{
    Handler:            httpHandler,
    Name:               "static-web",  // disclosed
}
s.https = &fasthttp.Server{
    Handler:            httpsHandler,
    Name:               "static-web",  // disclosed
}
```

#### Recommendation

Set `Name` to an empty string to suppress the `Server` header, or make it configurable:

```go
s.http = &fasthttp.Server{
    Name: "", // suppress Server header
}
```

---

### SEC-008: Unsanitized File Paths in Log Output

| Attribute   | Value                                                     |
| ----------- | --------------------------------------------------------- |
| **Severity**| LOW                                                       |
| **CWE**     | CWE-117 (Improper Output Neutralization for Logs)         |
| **OWASP**   | A09:2021 — Security Logging and Monitoring Failures       |
| **File**    | `internal/handler/middleware.go:113–115` and `file.go:257` |

#### Description

Access logs include the raw request URI without sanitizing control characters (newlines, carriage returns, ANSI escape sequences). An attacker can inject fake log lines via specially crafted URLs.

#### Attack Scenario

1. Attacker sends: `GET /legit%0a2026/04/11%2012:00:00%20GET%20/admin%20200%200%201us HTTP/1.1`
2. When decoded, the URI contains a newline, creating a fake log line that appears to show a successful request to `/admin`.
3. Log analysis tools or human reviewers may be misled.

#### Recommendation

Sanitize URIs before logging by replacing control characters:

```go
func sanitizeForLog(s string) string {
    return strings.Map(func(r rune) rune {
        if r < 0x20 || r == 0x7f {
            return '?'
        }
        return r
    }, s)
}

uri := sanitizeForLog(string(ctx.RequestURI()))
```

---

### SEC-009: Deprecated `PreferServerCipherSuites` TLS Field

| Attribute   | Value                                                  |
| ----------- | ------------------------------------------------------ |
| **Severity**| LOW                                                    |
| **CWE**     | CWE-327 (Use of a Broken or Risky Cryptographic Algorithm) |
| **OWASP**   | A02:2021 — Cryptographic Failures                      |
| **File**    | `internal/server/server.go:93`                         |

#### Description

The TLS configuration sets `PreferServerCipherSuites: true`, which has been deprecated since Go 1.17 and is a no-op since Go 1.21. The cipher suite selection itself is excellent (all AEAD ciphers, no CBC, no RSA key exchange). This is purely a code hygiene issue.

#### Recommendation

Remove the deprecated field:

```go
tlsCfg := &tls.Config{
    MinVersion: tls.VersionTLS12,
    CurvePreferences: []tls.CurveID{
        tls.X25519,
        tls.CurveP256,
    },
    CipherSuites: []uint16{
        // ... same excellent suite list
    },
    // PreferServerCipherSuites removed -- Go >=1.21 always prefers server order
}
```

---

### SEC-010: Template Execution Error Silently Discarded

| Attribute   | Value                                              |
| ----------- | -------------------------------------------------- |
| **Severity**| LOW                                                |
| **CWE**     | CWE-755 (Improper Handling of Exceptional Conditions) |
| **OWASP**   | A04:2021 — Insecure Design                         |
| **File**    | `internal/handler/dirlist.go:191`                   |

#### Description

The directory listing template execution assigns the error to the blank identifier `_`. If the template fails to render, the client receives a 200 OK with an empty or partial HTML body and no indication of failure.

#### Evidence

```go
var buf bytes.Buffer
_ = dirListTemplate.Execute(&buf, data)
ctx.SetBody(buf.Bytes())
```

#### Recommendation

Handle the error and return 500:

```go
var buf bytes.Buffer
if err := dirListTemplate.Execute(&buf, data); err != nil {
    log.Printf("handler: directory listing template error: %v", err)
    ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
    return
}
ctx.SetBody(buf.Bytes())
```

---

### SEC-011: Large Files Read Entirely Into Memory

| Attribute   | Value                                           |
| ----------- | ----------------------------------------------- |
| **Severity**| LOW                                             |
| **CWE**     | CWE-770 (Allocation of Resources Without Limits or Throttling) |
| **OWASP**   | A05:2021 — Security Misconfiguration            |
| **File**    | `internal/handler/file.go:338–377`              |

#### Description

The `serveLargeFile` function reads the entire file into memory via `io.ReadAll`. For very large files, this can consume significant RAM per concurrent request. This is a known limitation of fasthttp's buffered response model.

#### Recommendation

1. Document the constraint — warn operators that `MaxFileSize` also implicitly limits the maximum servable file size before memory pressure.
2. Add a hard maximum:

```go
const absoluteMaxFileSize = 512 * 1024 * 1024 // 512 MB

if info.Size() > absoluteMaxFileSize {
    ctx.Error("File too large", fasthttp.StatusRequestEntityTooLarge)
    return
}
```

3. Consider `net/http` for a future streaming path.

---

### SEC-012: CORS Wildcard Does Not Set `Vary: Origin`

| Attribute   | Value                     |
| ----------- | ------------------------- |
| **Severity**| INFO                      |
| **CWE**     | N/A (informational)       |
| **File**    | `internal/security/security.go:313–316` |

This is actually **correct** per the Fetch specification. A literal `*` response is not origin-dependent, so `Vary: Origin` would needlessly fragment proxy caches. No code change needed.

---

### SEC-013: ETag Truncation to 16 Hex Characters

| Attribute   | Value                     |
| ----------- | ------------------------- |
| **Severity**| INFO                      |
| **CWE**     | CWE-328 (Use of Weak Hash) |
| **File**    | `internal/handler/file.go:480–483` |

ETags are computed as `sha256(data)[:8]` (64 bits). For cache validation purposes, the ~10^19 possible values yield negligible collision probability at realistic file counts. ETags are not used for authentication. No change needed — appropriate trade-off for header size.

---

### SEC-014: `MaxRequestBodySize: 0` Relies on Fasthttp Default

| Attribute   | Value                     |
| ----------- | ------------------------- |
| **Severity**| INFO                      |
| **CWE**     | CWE-770 (Allocation of Resources Without Limits or Throttling) |
| **File**    | `internal/server/server.go:74` |

In fasthttp, `0` means "use the default" (4 MB). For a static file server that should never receive request bodies, consider setting an explicit small value:

```go
MaxRequestBodySize: 1024, // 1 KB -- static server needs no request body
```

---

### SEC-015: No Rate Limiting or Request Throttling

| Attribute   | Value                     |
| ----------- | ------------------------- |
| **Severity**| INFO                      |
| **CWE**     | CWE-770 (Allocation of Resources Without Limits or Throttling) |
| **File**    | N/A (architectural)       |

No built-in rate limiting. This is typical for a static file server (usually handled by a reverse proxy or CDN). Consider adding `MaxConnsPerIP` via fasthttp's built-in support for direct-exposure deployments.

---

### SEC-016: Preload Walker Does Not Validate Symlink Targets

| Attribute   | Value                     |
| ----------- | ------------------------- |
| **Severity**| INFO                      |
| **CWE**     | CWE-59 (Improper Link Resolution Before File Access) |
| **File**    | `internal/cache/preload.go:74–158` |

#### Description

The `Preload` function uses `filepath.WalkDir` to traverse the root directory and load files into cache. Files that are symlinks are read via `os.ReadFile(fpath)`, which follows the symlink without verifying that the target is still within the root directory. The request-time path via `PathSafe` **does** perform symlink resolution and blocks this — the vulnerability is only during preload at startup.

#### Recommendation

Add symlink target validation in the preload walker:

```go
realPath, err := filepath.EvalSymlinks(fpath)
if err != nil {
    stats.Skipped++
    return nil
}
if !strings.HasPrefix(realPath, absRoot+string(filepath.Separator)) && realPath != absRoot {
    stats.Skipped++
    return nil
}
```

---

## Positive Security Observations

The following practices demonstrate strong security awareness and are worth preserving:

### 1. Multi-Layer Path Traversal Prevention
**File:** `internal/security/security.go:120–187`

The `PathSafe` function implements defense-in-depth with 5 sequential checks: null byte rejection, `path.Clean` normalization, `filepath.Join` with prefix verification, `filepath.EvalSymlinks` with re-verification, and dotfile component blocking. Textbook path traversal prevention.

### 2. HTTP Method Whitelist (GET/HEAD/OPTIONS Only)
**File:** `internal/security/security.go:272–275`

Prevents TRACE (XST attacks), PUT/POST/DELETE, and any other method. Correct for a static file server.

### 3. XSS-Safe Directory Listing via `html/template`
**File:** `internal/handler/dirlist.go:40`

Using `html/template` (not `text/template`) ensures all interpolated values are automatically HTML-escaped.

### 4. CORS Wildcard Does Not Reflect Origin
**File:** `internal/security/security.go:313–316`

Emits a literal `*` rather than reflecting the `Origin` header. Prevents credential-based cross-origin attacks.

### 5. CI/CD Actions Pinned to Commit SHAs
**Files:** `.github/workflows/ci.yml`, `.github/workflows/release.yml`

GitHub Actions are pinned to specific commit SHAs rather than mutable tags, preventing supply-chain attacks.

### 6. `govulncheck` in CI Pipeline
Proactive vulnerability scanning against the Go vulnerability database on every CI run.

### 7. Race Detector in Tests
Tests run with `-race`, detecting data races in concurrent code (`sync.Map`, `sync.Pool`, atomics, goroutines).

### 8. Zero Hardcoded Secrets
No API keys, tokens, passwords, or credentials found in any source file. All sensitive configuration loaded from config/environment.

### 9. Sidecar Path Validation
**File:** `internal/handler/file.go:509–552`

`ValidateSidecarPath` ensures `.gz`, `.br`, `.zst` sidecar files haven't escaped the root directory via symlink.

### 10. Robust TLS Configuration
**File:** `internal/server/server.go:79–94`

TLS 1.2+ minimum, only AEAD cipher suites (GCM, ChaCha20-Poly1305), modern curve preferences (X25519, P-256), HTTP-to-HTTPS redirect, and HSTS support.

### 11. Security Headers on Error Responses
**File:** `internal/security/security.go:248–260`

Security headers are set before calling the inner handler, ensuring even 400/403/404/405 responses carry X-Content-Type-Options, X-Frame-Options, CSP, etc.

### 12. Custom 404 Page Path Validated via PathSafe
**File:** `internal/handler/file.go:450`

Even the custom 404 page path from configuration is validated through `PathSafe`, preventing config-driven path injection.

---

## Prioritized Remediation Plan

| Priority | Finding | Severity | Effort | Impact |
| -------- | ------- | -------- | ------ | ------ |
| **P1**   | SEC-001: Bound the PathCache with LRU | HIGH | Low (~30 LOC) | Eliminates DoS vector |
| **P2**   | SEC-002: Align `config.toml` with secure code defaults | MEDIUM | Low (~10 lines TOML) | Restores secure-by-default |
| **P3**   | SEC-005: Add `maxCompressSize` threshold | MEDIUM | Low (~3 LOC) | Prevents memory exhaustion |
| **P4**   | SEC-004: Randomize multipart boundary | MEDIUM | Low (~10 LOC) | Eliminates fingerprinting |
| **P5**   | SEC-006: `path.Clean` in cache key function | MEDIUM | Low (~2 LOC) | Defense-in-depth |
| **P6**   | SEC-003: Make stack trace logging configurable | MEDIUM | Low (~10 LOC) | Reduces info leakage |
| **P7**   | SEC-007: Suppress server name header | LOW | Trivial (1 LOC) | Reduces fingerprinting |
| **P8**   | SEC-008: Sanitize log output | LOW | Low (~15 LOC) | Prevents log forgery |
| **P9**   | SEC-009: Remove deprecated TLS field | LOW | Trivial (1 LOC) | Code hygiene |
| **P10**  | SEC-010: Handle template execution errors | LOW | Low (~5 LOC) | Better error handling |
| **P11**  | SEC-011: Document large file memory constraint | LOW | Medium (docs + config) | Operator awareness |
| Backlog  | SEC-012 through SEC-016 | INFO | Various | Hardening / documentation |

---

*Report generated by Kai security audit pipeline. All findings verified against source code as of commit `fcfe429`.*
