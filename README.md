# static-web

A production-grade, high-performance static web file server written in Go. Zero external runtime dependencies beyond `BurntSushi/toml` and `hashicorp/golang-lru/v2`.

## Table of Contents

- [Quick Start](#quick-start)
- [CLI](#cli)
- [Features](#features)
- [Architecture](#architecture)
- [Performance](#performance)
- [Security Model](#security-model)
- [Configuration Reference](#configuration-reference)
- [Environment Variables](#environment-variables)
- [TLS / HTTPS](#tls--https)
- [Pre-compressed Files](#pre-compressed-files)
- [HTTP Signals](#http-signals)
- [Building & Development](#building--development)
- [Package Layout](#package-layout)
- [Known Limitations](#known-limitations)

---

## Quick Start

```bash
# Go install (requires Go 1.26+)
go install github.com/static-web/server/cmd/static-web@latest

# Serve the current directory
static-web .

# Serve a build output directory on port 3000
static-web --port 3000 ./dist

# Scaffold a config file
static-web init
```

---

## CLI

For the full flag reference, subcommand documentation, and installation options, see [CLI.md](CLI.md).

```bash
static-web --help
```

---

## Features

| Feature | Detail |
|---------|--------|
| **In-memory LRU cache** | Size-bounded, byte-accurate; zero-alloc hot path (~27 ns/op) |
| **gzip compression** | On-the-fly via pooled `gzip.Writer`; pre-compressed `.gz`/`.br` sidecar support |
| **HTTP/2** | Automatic ALPN negotiation when TLS is configured |
| **Conditional requests** | ETag, `304 Not Modified`, `If-Modified-Since`, `If-None-Match` |
| **Range requests** | Byte ranges via `http.ServeContent` for video and large files |
| **TLS 1.2 / 1.3** | Modern cipher suites; configurable cert/key paths |
| **Security headers** | `X-Content-Type-Options`, `X-Frame-Options`, `Content-Security-Policy`, `Referrer-Policy`, `Permissions-Policy` |
| **HSTS** | `Strict-Transport-Security` on all HTTPS responses; configurable max-age |
| **HTTP→HTTPS redirect** | Automatic 301 redirect on the HTTP port when TLS is active |
| **Method whitelist** | Only `GET`, `HEAD`, `OPTIONS` are accepted (TRACE/PUT/POST blocked) |
| **Dotfile protection** | Blocks `.env`, `.git/`, etc. by default |
| **Directory listing** | Optional HTML directory index with breadcrumb navigation, sorted entries, human-readable sizes, and dotfile filtering |
| **Symlink escape prevention** | `EvalSymlinks` re-verified against root; symlinks pointing outside root are blocked |
| **CORS** | Configurable per-origin or wildcard (`*` emits literal `*`, never reflected) |
| **Graceful shutdown** | SIGTERM/SIGINT drains in-flight requests with configurable timeout |
| **Live cache flush** | SIGHUP flushes the in-memory cache without downtime |

---

## Architecture

```
HTTP request
     │
     ▼
┌─────────────────┐
│ recoveryMiddleware │  ← panic → 500, log stack
└────────┬────────┘
         │
┌────────▼────────┐
│ loggingMiddleware │  ← pooled statusResponseWriter; logs method/path/status/duration
└────────┬────────┘
         │
┌────────▼────────────────────────────────────────┐
│ security.Middleware                              │
│  • Method whitelist (GET/HEAD/OPTIONS only)      │
│  • Security headers (set BEFORE path check)      │
│  • PathSafe: null bytes, path.Clean, EvalSymlinks│
│  • Dotfile blocking                              │
│  • CORS (preflight + per-origin or wildcard *)   │
│  • Injects validated path into context           │
└────────┬────────────────────────────────────────┘
         │
┌────────▼────────────────────────────────────────┐
│ headers.Middleware                               │
│  • 304 Not Modified (ETag, If-Modified-Since)    │
│  • Cache-Control, immutable pattern matching     │
└────────┬────────────────────────────────────────┘
         │
┌────────▼────────────────────────────────────────┐
│ compress.Middleware                              │
│  • lazyGzipWriter: decides at first Write()      │
│  • Skips 1xx/204/304, non-compressible types     │
│  • Respects q=0 explicit denial                  │
└────────┬────────────────────────────────────────┘
         │
┌────────▼────────────────────────────────────────┐
│ handler.FileHandler                              │
│  • Cache hit → serve from memory (zero os.Stat) │
│  • Cache miss → os.Stat → disk read → cache put  │
│  • Large files (> max_file_size) bypass cache    │
│  • Encoding negotiation: brotli > gzip > plain   │
│  • Custom 404 page (path-validated)              │
└─────────────────────────────────────────────────┘
```

### Request path through the cache

```
GET /app.js
  │
  ├─ cache.Get("/app.js") hit?
  │     YES → serveFromCache (no syscall) → done
  │
  └─ NO → resolveIndexPath → cache.Get(canonicalURL) hit?
              YES → serveFromCache → done
              NO  → os.Stat → os.ReadFile → cache.Put → serveFromCache
```

---

## Performance

Benchmark numbers on Apple M2 Pro (`go test -bench=. -benchtime=5s`):

| Benchmark | ops/s | ns/op | allocs/op |
|-----------|-------|-------|-----------|
| `BenchmarkCacheGet` | 87–131 M | 27 | 0 |
| `BenchmarkCachePut` | 42–63 M | 57 | 0 |
| `BenchmarkCacheGetParallel` | 15–25 M | 142–147 | 0 |
| `BenchmarkHandler_CacheHit` | — | ~5,840 | — |

Key design decisions driving these numbers:

- **Cache-before-stat**: `os.Stat` is never called on a cache hit — the hot path is pure memory.
- **Zero-alloc `AcceptsEncoding`**: walks the `Accept-Encoding` header byte-by-byte without `strings.Split`.
- **Pooled `sync.Pool`**: both `gzip.Writer` and `statusResponseWriter` are pooled.
- **`filepath.Abs` at startup**: computed once during construction, never per-request.
- **Pre-computed `ETagFull`**: the `W/"..."` string is built when the file is cached.

---

## Security Model

### Path Safety (`internal/security`)

Every request URL is validated through `PathSafe` before any filesystem access:

1. **Null byte rejection** — prevents C-level path truncation.
2. **`path.Clean` normalisation** — collapses `/../`, `//`, etc.
3. **Prefix check** — ensures the resolved path starts with the absolute root (separator-aware to prevent `/rootsuffix` collisions).
4. **`EvalSymlinks` re-verification** — resolves the canonical real path and re-checks the prefix. Symlinks pointing outside root return `ErrPathTraversal`. Non-existent paths (ENOENT) fall back to the already-checked candidate.
5. **Dotfile blocking** — each path segment is checked for a leading `.`.

### HTTP Security Headers

Set on **every** response including 4xx/5xx errors:

| Header | Default Value |
|--------|---------------|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `SAMEORIGIN` |
| `Content-Security-Policy` | `default-src 'self'` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `geolocation=(), microphone=(), camera=()` |
| `Strict-Transport-Security` | `max-age=31536000` *(HTTPS only)* |

### Method Whitelist

Only `GET`, `HEAD`, and `OPTIONS` are accepted. All other methods (including `TRACE`, `PUT`, `POST`, `DELETE`, `PATCH`) receive `405 Method Not Allowed`. This means TRACE-based XST attacks are impossible by design.

### CORS

- **Wildcard (`["*"]`)**: emits the literal string `*`. The request `Origin` is never reflected. `Vary: Origin` is not added (correct per RFC 6454).
- **Specific origins**: each allowed origin is compared exactly. Matching origins receive `Access-Control-Allow-Origin: <origin>` and `Vary: Origin`.
- **Preflight (`OPTIONS`)**: returns `204` with `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers`, and `Access-Control-Max-Age: 86400`.

### DoS Mitigations

| Mitigation | Value |
|------------|-------|
| `ReadHeaderTimeout` | 5 s (Slowloris) |
| `ReadTimeout` | 10 s |
| `WriteTimeout` | 10 s |
| `MaxHeaderBytes` | 8 KiB |

---

## Configuration Reference

Copy `config.toml.example` to `config.toml` and edit as needed. The server starts without a config file using sensible defaults.

### `[server]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `addr` | string | `:8080` | HTTP listen address |
| `tls_addr` | string | `:8443` | HTTPS listen address |
| `tls_cert` | string | — | Path to TLS certificate (PEM) |
| `tls_key` | string | — | Path to TLS private key (PEM) |
| `read_header_timeout` | duration | `5s` | Slowloris protection |
| `read_timeout` | duration | `10s` | Full request read deadline |
| `write_timeout` | duration | `10s` | Response write deadline |
| `idle_timeout` | duration | `75s` | Keep-alive idle timeout |
| `shutdown_timeout` | duration | `15s` | Graceful drain window |

### `[files]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `root` | string | `./public` | Directory to serve |
| `index` | string | `index.html` | Index file for directory requests |
| `not_found` | string | — | Custom 404 page (relative to `root`) |

### `[cache]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Toggle in-memory LRU cache |
| `max_bytes` | int | `268435456` | Cache size cap (bytes) |
| `max_file_size` | int | `10485760` | Max file size to cache (bytes) |
| `ttl` | duration | `0` | Entry TTL (0 = no expiry; flush with SIGHUP) |

### `[compression]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Enable compression |
| `min_size` | int | `1024` | Minimum bytes to compress |
| `level` | int | `5` | gzip level (1–9) |
| `precompressed` | bool | `true` | Serve `.gz`/`.br` sidecar files |

### `[headers]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `immutable_pattern` | string | — | Glob for immutable assets |
| `static_max_age` | int | `3600` | `Cache-Control` max-age for non-HTML (seconds) |
| `html_max_age` | int | `0` | `Cache-Control` max-age for HTML (seconds) |

### `[security]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `block_dotfiles` | bool | `true` | Block `.`-prefixed path components |
| `directory_listing` | bool | `false` | Enable directory index listing |
| `cors_origins` | []string | `[]` | Allowed CORS origins (`["*"]` for wildcard) |
| `csp` | string | `default-src 'self'` | `Content-Security-Policy` value |
| `referrer_policy` | string | `strict-origin-when-cross-origin` | `Referrer-Policy` value |
| `permissions_policy` | string | `geolocation=(), microphone=(), camera=()` | `Permissions-Policy` value |
| `hsts_max_age` | int | `31536000` | HSTS `max-age` in seconds (HTTPS only; 0 disables) |
| `hsts_include_subdomains` | bool | `false` | Add `includeSubDomains` to HSTS header |

---

## Environment Variables

All environment variables override the corresponding TOML setting. Useful for containers.

| Variable | Config Field |
|----------|-------------|
| `STATIC_SERVER_ADDR` | `server.addr` |
| `STATIC_SERVER_TLS_ADDR` | `server.tls_addr` |
| `STATIC_SERVER_TLS_CERT` | `server.tls_cert` |
| `STATIC_SERVER_TLS_KEY` | `server.tls_key` |
| `STATIC_SERVER_READ_HEADER_TIMEOUT` | `server.read_header_timeout` |
| `STATIC_SERVER_READ_TIMEOUT` | `server.read_timeout` |
| `STATIC_SERVER_WRITE_TIMEOUT` | `server.write_timeout` |
| `STATIC_SERVER_IDLE_TIMEOUT` | `server.idle_timeout` |
| `STATIC_SERVER_SHUTDOWN_TIMEOUT` | `server.shutdown_timeout` |
| `STATIC_FILES_ROOT` | `files.root` |
| `STATIC_FILES_INDEX` | `files.index` |
| `STATIC_FILES_NOT_FOUND` | `files.not_found` |
| `STATIC_CACHE_ENABLED` | `cache.enabled` |
| `STATIC_CACHE_MAX_BYTES` | `cache.max_bytes` |
| `STATIC_CACHE_MAX_FILE_SIZE` | `cache.max_file_size` |
| `STATIC_CACHE_TTL` | `cache.ttl` |
| `STATIC_COMPRESSION_ENABLED` | `compression.enabled` |
| `STATIC_COMPRESSION_MIN_SIZE` | `compression.min_size` |
| `STATIC_COMPRESSION_LEVEL` | `compression.level` |
| `STATIC_SECURITY_BLOCK_DOTFILES` | `security.block_dotfiles` |
| `STATIC_SECURITY_CSP` | `security.csp` |
| `STATIC_SECURITY_CORS_ORIGINS` | `security.cors_origins` (comma-separated) |

---

## TLS / HTTPS

Set `tls_cert` and `tls_key` to enable HTTPS:

```toml
[server]
addr     = ":80"
tls_addr = ":443"
tls_cert = "/etc/ssl/certs/server.pem"
tls_key  = "/etc/ssl/private/server.key"
```

When TLS is configured:
- HTTP requests on `addr` are automatically **redirected** to `tls_addr` with `301 Moved Permanently`.
- **HTTP/2** is enabled automatically via ALPN negotiation.
- **HSTS** (`Strict-Transport-Security`) is added to all HTTPS responses (configurable max-age).
- Minimum TLS version is **1.2**; preferred cipher suites are ECDHE+AES-256-GCM and ChaCha20-Poly1305.

---

## Pre-compressed Files

Place `.gz` and `.br` sidecar files alongside originals. The server serves them automatically when the client signals support:

```
public/
  app.js
  app.js.gz    ← served for Accept-Encoding: gzip
  app.js.br    ← served for Accept-Encoding: br (preferred over gzip)
  style.css
  style.css.gz
```

Generate sidecars from the `Makefile`:

```bash
make precompress   # runs gzip and brotli on all .js/.css/.html/.json/.svg
```

> **Note**: On-the-fly brotli encoding is not implemented. Only `.br` sidecar files are served with brotli encoding.

---

## HTTP Signals

| Signal | Action |
|--------|--------|
| `SIGTERM` | Graceful shutdown (drains in-flight requests up to `shutdown_timeout`) |
| `SIGINT` | Graceful shutdown |
| `SIGHUP` | Flush in-memory cache; re-reads config pointer in `main` |

> **Note**: SIGHUP reloads the config pointer in `main` but the live middleware chain holds references to the old config. A full restart is required for config changes to take effect. SIGHUP is useful for flushing the cache without downtime.

---

## Building & Development

### Prerequisites

- Go 1.26+
- GNU Make

### Commands

```bash
make build        # compile → bin/static-web
make release      # compile stripped binary → bin/static-web
make install      # install to $(GOPATH)/bin
make run          # build + run with ./config.toml
make test         # go test -race ./...
make bench        # go test -bench=. -benchtime=5s ./...
make lint         # go vet ./...
make precompress  # generate .gz/.br sidecars for public/
make clean        # remove bin/
```

### Running Tests

```bash
go test -race ./...                       # full suite with race detector
go test -run TestPathSafe ./internal/security/...  # specific test
go test -bench=BenchmarkCacheGet -benchtime=10s ./internal/cache/
```

### Code Quality Gates

All PRs must pass:

```bash
go build ./...    # clean compile
go vet ./...      # static analysis
go test -race ./... # all tests, race-free
```

---

## Known Limitations

| Limitation | Detail |
|------------|--------|
| **Brotli on-the-fly** | Not implemented. Only pre-compressed `.br` sidecar files are served. |
| **Cache TTL not enforced** | `cache.ttl` is parsed but the expiry logic is not yet implemented. Use SIGHUP to flush manually. |
| **SIGHUP config reload** | Reloads the config struct pointer in `main` only. Live middleware chains hold old references — full restart required for config changes to propagate. |
