## v1.6.2 (2026-04-12)

### Fix

- **security**: replace unbounded sync.Map PathCache with bounded LRU (hashicorp/golang-lru) to prevent memory exhaustion DoS (SEC-001)
- **security**: make panic stack traces configurable via STATIC_DEBUG env var (SEC-003)
- **security**: generate random multipart boundary per response using crypto/rand (SEC-004)
- **security**: add MaxCompressSize (10 MB) limit for on-the-fly gzip (SEC-005)
- **security**: apply path.Clean in CacheKeyForPath to prevent cache poisoning (SEC-006)
- **security**: suppress server name disclosure (SEC-007)
- **security**: sanitize control characters in access log URIs (SEC-008)
- **security**: remove deprecated PreferServerCipherSuites TLS option (SEC-009)
- **security**: handle template execution errors in directory listing (SEC-010)
- **security**: add MaxServeFileSize (1 GB) hard limit for large file serving (SEC-011)
- **security**: add clarifying comment on CORS wildcard Vary behavior (SEC-012)
- **security**: document ETag 64-bit truncation rationale (SEC-013)
- **security**: set explicit MaxRequestBodySize (1024 bytes) (SEC-014)
- **security**: add MaxConnsPerIP config support for rate limiting (SEC-015)
- **security**: validate symlink targets during cache preload (SEC-016)

### Docs

- update landing page, README, and USER_GUIDE for security audit remediations
- add 3 new config fields to documentation tables
- mark all 16 security findings as resolved in audit report

### Test

- add TestBuildHandler_MaxServeFileSize (under/over/disabled)
- add TestMiddleware_MaxCompressSize (under/over/at-limit/disabled)
- expand TestCacheKeyForPath with path normalization edge cases
- add TestPathCache_BoundedLRU, LookupPromotesEntry, FlushClearsAll, DefaultSizeOnZero
- add TestNew_HTTPOnly_SecurityDefaults and TestNew_TLS_SecurityDefaults
- add TestNew_MaxConnsPerIP_Zero for disabled state

### Build

- bump brotli v1.2.0 → v1.2.1
- bump klauspost/compress v1.18.4 → v1.18.5
- bump fasthttp v1.69.0 → v1.70.0

## v1.6.1 (2026-03-28)

### Fix

- add explicit permissions block to CI workflow
- use filepath.Clean() and filepath.Join() to resolve CodeQL path-injection alert
- extract path validation helper to resolve CodeQL path-injection alert
- **security**: validate sidecar paths to prevent path injection attacks

## v1.6.0 (2026-03-16)

### Feat

- add zstd compression support and compression benchmarks
- add zstd compression support

## v1.5.0 (2026-03-12)

### Feat

- **cli**: add --no-etag flag for disabling ETag generation

### Fix

- **docs**: update hero stat to 148k req/sec

## v1.4.0 (2026-03-12)

### Feat

- use Outfit font and make ETags optional

### Fix

- set ETag and Cache-Control headers on embedded fallback assets

## v1.3.0 (2026-03-08)

### Feat

- **ui**: enhance landing page with reveal animations and high-quality hover states
- upgrade landing page to premium dark mode

### Fix

- **config**: remove dead read_header_timeout setting (fasthttp has no such field)
- **cache**: enforce true no-cache mode and honor entry ttl
- **server**: harden HTTP to HTTPS redirects against host header abuse

### Refactor

- **server**: remove benchmark-mode in favor of production preload

### Perf

- **server**: migrate HTTP layer from net/http to fasthttp
- **server**: add startup preloading, zero-alloc fast path, path cache, and GC tuning
- **handler**: reduce cache hit overhead and cold-miss filesystem work

## v1.2.0 (2026-03-07)

### Feat

- **docs**: add GitHub Pages landing page with SEO and source-verified content
- embed default pages into binary as fallback when files.root lacks them
- rebrand footer to 21no.de with love emoji
- redesign default pages with terminal/dev-oriented aesthetic

### Fix

- replace fingerprint-blocked named fonts with ui-monospace system stack
- extract inline styles to style.css to comply with default-src 'self' CSP
- remove inline script and onclick handler to comply with default-src 'self' CSP

## v1.1.0 (2026-03-07)

### Feat

- add branded default index and 404 pages based on BackendStack21 identity
- add branded default index and 404 pages based on BackendStack21 identity

## v1.0.1 (2026-03-07)

### Fix

- update repository URLs to github.com/BackendStack21/static-web

## v1.0.0 (2026-03-07)

### Feat

- initial project — static-web server with benchmark suite

### Perf

- upgrade to Go 1.26 and optimize static-web server config
- **benchmark**: optimize nginx and caddy configs for raw throughput
