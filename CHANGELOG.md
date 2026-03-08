## v1.3.0 (2026-03-08)

### Perf

- **server**: migrate HTTP layer from net/http to fasthttp — ~141k req/sec (55% faster than Bun)
- **server**: use `tcp4` listener to eliminate dual-stack overhead (2x throughput gain on macOS)

### Refactor

- **handler**: replace `http.ServeContent` with custom `parseRange()`/`serveRange()` for byte-range requests
- **compress**: convert gzip middleware from wrapping `ResponseWriter` to post-processing response body
- **security**: use `ctx.SetStatusCode()`+`ctx.SetBodyString()` instead of `ctx.Error()` to preserve headers
- **cache**: change `CachedFile` header fields from `[]string` to `string`

### Build

- **benchmark**: add fasthttp/net-http hello world baselines and update baremetal script

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
