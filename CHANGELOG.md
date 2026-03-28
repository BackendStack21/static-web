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
