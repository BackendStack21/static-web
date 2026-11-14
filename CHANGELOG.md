## v1.7.1 (2026-11-14)

### Fix

- **cmd**: refuse to start when files.root is not readable
- **cache**: refuse a root path replaced by a symlink
- **security**: validate a missing path by its deepest existing ancestor
- **cli**: list --no-etag in the --help text
- **security**: send ACAO "*" on every response with wildcard CORS
- **handler**: saturate the large-file rate cap instead of overflowing
- **handler**: apply max_serve_file_size to every file
- **cache**: preload each index file once
- **handler**: keep a cached index from replacing the directory listing
- **handler**: open files through the root and describe large files by the handle
- **handler**: size cached entries from the bytes read, not an earlier stat
- **handler**: render directory listings for HEAD so headers match GET
- **security**: clear a foreign safe-path value after a PathCache hit
- **compress**: never encode a bodiless HEAD that declares its length
- **server**: keep earlier headers on the redirect listener's 400
- **handler**: ignore Range on HEAD requests
- **handler**: answer conditional requests with 304 on a disk miss
- **server**: send security headers and HSTS on fasthttp's own errors
- **handler**: access-log requests whose handler panicked
- **handler**: hide FIFOs, sockets and devices in directory listings
- **handler**: refuse a non-regular custom 404 page
- **cache**: drop the stale entry when its replacement is too large
- **handler**: extend the write deadline as large downloads progress
- **cmd**: preload sidecars and zstd variants like lazy serving
- **handler**: report X-Cache: MISS for responses read from disk
- **handler**: keep security headers on every file-handler error
- **handler**: serve the negotiated variant when a Range header is ignored
- **handler**: refuse non-regular precompressed sidecars
- **handler**: 404 for a regular file requested with a trailing slash
- **security**: send Vary: Origin on every response for a specific CORS list
- **handler**: refuse non-regular files before opening them
- **handler**: apply the dotfile rule to precompressed sidecar targets
- **security**: resolve files.root once so PreWarm stores absolute paths
- **security**: re-validate paths on file-cache miss; never trust a stale PathCache verdict
- **handler**: log the real body size of on-the-fly gzipped responses
- **handler**: let the CSP allow the directory listing's inline style
- **handler**: escape listing links and resolve symlink entries
- **server**: stop sending "Server: fasthttp"
- **handler**: keep security headers and HSTS on error responses
- **cache**: preload builds the same variants as lazy serving
- **cache**: account retained capacity and stop double-subtracting on replace

### Perf

- **security**: validate a missing path with one Lstat per component
- **handler**: copy the uncached HEAD body only when it may be gzipped
- **handler**: let large-file bodies use sendfile
- **handler**: build encoded variants only for entries that are cached
- always build the PathCache; resolve the root once instead of per check
- **handler**: buffer access-log writes and flush them on shutdown
- **headers**: merge Vary and build cache keys without allocating
- **compress**: pool gzip/zstd encoders in GzipBytes and ZstdBytes

## v1.7.0 (2026-10-27)

### Feat

- **config**: add Config.Validate and report invalid env values

### Fix

- **handler**: place large-file containment check inside the open helper
- **handler**: re-check root containment at file sinks; guard int conversion
- **server**: use tcp listener for IPv6 and hostname addresses
- **cmd**: exit non-zero on startup failure and keep flags on SIGHUP
- **server**: return listen and TLS keypair errors from Start
- **handler**: conditional requests for embedded default assets
- **cache**: stat symlink targets in preload; compare-and-remove expired entries
- **handler**: stream large files with validators and 304; bound sidecar size
- **handler**: honour If-Range; answer OPTIONS with 204 and send Allow on 405
- **headers**: merge Vary tokens, full 304 headers, weak ETag comparison, HEAD length parity
- **security**: validate directory index path and never cache unverified paths
- **compress**: never re-encode 206/streamed responses; RFC-compliant Accept-Encoding parsing
- **handler**: bound multi-range requests and return 416 for unsatisfiable ranges

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
