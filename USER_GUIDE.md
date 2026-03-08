# static-web User Guide

This guide covers everything you need to get `static-web` running in production — from a single binary to a fully containerised deployment behind a TLS-terminating reverse proxy.

## Table of Contents

- [Quick Start](#quick-start)
- [CLI Flags](#cli-flags)
- [Configuration](#configuration)
  - [Config File](#config-file)
  - [Environment Variables](#environment-variables)
- [TLS / HTTPS Setup](#tls--https-setup)
  - [Self-Signed Certificate (Dev / Testing)](#self-signed-certificate-dev--testing)
  - [Let's Encrypt (Production)](#lets-encrypt-production)
  - [Behind a Reverse Proxy (nginx / Caddy)](#behind-a-reverse-proxy-nginx--caddy)
- [Pre-compressing Assets](#pre-compressing-assets)
- [Docker Deployment](#docker-deployment)
  - [Dockerfile](#dockerfile)
  - [docker-compose.yml](#docker-composeyml)
  - [Running the Container](#running-the-container)
- [Health Checks and Readiness Probes](#health-checks-and-readiness-probes)
- [Live Cache Flush (SIGHUP)](#live-cache-flush-sighup)
- [CORS Configuration](#cors-configuration)
- [Custom 404 Page](#custom-404-page)
- [Directory Listing](#directory-listing)
- [Known Limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)

---

## Quick Start

### From source

```bash
# requires Go 1.26+
git clone https://github.com/BackendStack21/static-web.git
cd server
make build          # produces bin/static-web
./bin/static-web    # serves ./public on :8080
```

The server starts with sensible defaults even without a config file:

| Default                | Value                 |
| ---------------------- | --------------------- |
| Listen address         | `:8080`               |
| Static files directory | `./public`            |
| In-memory cache        | enabled, 256 MB       |
| Compression            | enabled, gzip level 5 |
| Dotfile protection     | enabled               |
| Security headers       | always set            |

Point your browser at `http://localhost:8080`.

```bash
# Or install directly with go install:
go install github.com/BackendStack21/static-web/cmd/static-web@latest
static-web .
```

### Using a config file

```bash
cp config.toml.example config.toml
# edit config.toml as needed
./bin/static-web --config config.toml
```

---

## CLI Flags

For common use cases you don't need a config file at all. Just pass flags:

```bash
# Change the port
static-web --port 3000 ./dist

# Disable cache (useful during development)
static-web --no-cache ./dist

# Enable directory listing
static-web --dir-listing ~/Downloads

# Enable CORS for all origins
static-web --cors '*' ./dist

# Serve with TLS
static-web --tls-cert cert.pem --tls-key key.pem ./dist

# Suppress access logs
static-web --quiet ./dist

# Debug: show resolved config on startup
static-web --verbose ./dist
```

Run `static-web --help` or see [CLI.md](CLI.md) for the full flag reference.

---

## Configuration

### Config File

`config.toml` is a [TOML](https://toml.io) file. All fields are optional — the server applies safe defaults for anything not specified.

```toml
[server]
addr             = ":8080"       # HTTP listen address
tls_addr         = ":8443"       # HTTPS listen address (requires tls_cert + tls_key)
redirect_host    = ""            # canonical host for HTTP→HTTPS redirects (recommended in production)
tls_cert         = ""            # path to PEM certificate file
tls_key          = ""            # path to PEM private key file
read_header_timeout = "5s"       # Slowloris protection
read_timeout        = "10s"
write_timeout       = "10s"
idle_timeout        = "75s"
shutdown_timeout    = "15s"      # graceful drain window on SIGTERM/SIGINT

[files]
root      = "./public"           # directory to serve
index     = "index.html"         # index file for directory requests (e.g. GET /)
not_found = "404.html"           # custom 404 page, relative to root (optional)

[cache]
enabled       = true
max_bytes     = 268435456        # 256 MB total cache cap
max_file_size = 10485760         # files > 10 MB bypass the cache
ttl           = "0s"             # 0 = no expiry; >0 evicts stale entries on access
preload       = false            # true = load all files into RAM at startup
# gc_percent  = 0                # Go GC target %; 400 recommended with preload

[compression]
enabled       = true
min_size      = 1024             # don't compress responses smaller than 1 KB
level         = 5                # gzip level 1 (fastest) – 9 (best)
precompressed = true             # serve .gz / .br sidecar files when available

[headers]
immutable_pattern = ""           # glob for fingerprinted assets → Cache-Control: immutable
static_max_age    = 3600         # max-age for non-HTML assets (seconds)
html_max_age      = 0            # 0 = no-cache (always revalidate HTML)

[security]
block_dotfiles    = true
directory_listing = false        # enable to show directory index pages
cors_origins      = []           # e.g. ["https://app.example.com"] or ["*"]
csp               = "default-src 'self'"
referrer_policy   = "strict-origin-when-cross-origin"
permissions_policy = "geolocation=(), microphone=(), camera=()"
hsts_max_age      = 31536000     # 1 year; only sent over HTTPS; 0 disables
hsts_include_subdomains = false
```

### Environment Variables

Every config field can also be set via an environment variable, which takes precedence over the TOML file. This is the recommended approach for containers.

| Environment Variable                | Config Field                                     |
| ----------------------------------- | ------------------------------------------------ |
| `STATIC_SERVER_ADDR`                | `server.addr`                                    |
| `STATIC_SERVER_TLS_ADDR`            | `server.tls_addr`                                |
| `STATIC_SERVER_REDIRECT_HOST`       | `server.redirect_host`                           |
| `STATIC_SERVER_TLS_CERT`            | `server.tls_cert`                                |
| `STATIC_SERVER_TLS_KEY`             | `server.tls_key`                                 |
| `STATIC_SERVER_READ_HEADER_TIMEOUT` | `server.read_header_timeout`                     |
| `STATIC_SERVER_READ_TIMEOUT`        | `server.read_timeout`                            |
| `STATIC_SERVER_WRITE_TIMEOUT`       | `server.write_timeout`                           |
| `STATIC_SERVER_IDLE_TIMEOUT`        | `server.idle_timeout`                            |
| `STATIC_SERVER_SHUTDOWN_TIMEOUT`    | `server.shutdown_timeout`                        |
| `STATIC_FILES_ROOT`                 | `files.root`                                     |
| `STATIC_FILES_INDEX`                | `files.index`                                    |
| `STATIC_FILES_NOT_FOUND`            | `files.not_found`                                |
| `STATIC_CACHE_ENABLED`              | `cache.enabled`                                  |
| `STATIC_CACHE_PRELOAD`              | `cache.preload`                                  |
| `STATIC_CACHE_MAX_BYTES`            | `cache.max_bytes`                                |
| `STATIC_CACHE_MAX_FILE_SIZE`        | `cache.max_file_size`                            |
| `STATIC_CACHE_TTL`                  | `cache.ttl`                                      |
| `STATIC_CACHE_GC_PERCENT`           | `cache.gc_percent`                               |
| `STATIC_COMPRESSION_ENABLED`        | `compression.enabled`                            |
| `STATIC_COMPRESSION_MIN_SIZE`       | `compression.min_size`                           |
| `STATIC_COMPRESSION_LEVEL`          | `compression.level`                              |
| `STATIC_SECURITY_BLOCK_DOTFILES`    | `security.block_dotfiles`                        |
| `STATIC_SECURITY_CSP`               | `security.csp`                                   |
| `STATIC_SECURITY_CORS_ORIGINS`      | `security.cors_origins` (comma-separated values) |

**Example — override address and root at runtime:**

```bash
STATIC_SERVER_ADDR=:3000 STATIC_FILES_ROOT=/srv/www ./bin/static-web
```

**Example — CORS for a single origin:**

```bash
STATIC_SECURITY_CORS_ORIGINS=https://app.example.com ./bin/static-web
```

---

## TLS / HTTPS Setup

### Self-Signed Certificate (Dev / Testing)

```bash
# generate a self-signed cert valid for localhost
openssl req -x509 -newkey rsa:4096 -sha256 -days 365 \
  -nodes -keyout server.key -out server.crt \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
```

Then in `config.toml`:

```toml
[server]
addr     = ":8080"
tls_addr = ":8443"
redirect_host = "localhost"
tls_cert = "server.crt"
tls_key  = "server.key"
```

Now `http://localhost:8080` redirects to `https://localhost:8443` automatically.

### Let's Encrypt (Production)

The server does not perform ACME/Let's Encrypt certificate issuance itself. The recommended approach is:

1. Place the server behind **Caddy** (built-in ACME) or use **certbot** to obtain and renew certificates.
2. Point `tls_cert` and `tls_key` at the issued files.
3. Restart the server after renewal (or use the symlink-safe paths that certbot maintains at `/etc/letsencrypt/live/<domain>/`).

**Example with certbot on Linux:**

```bash
certbot certonly --standalone -d example.com

# config.toml
[server]
tls_cert = "/etc/letsencrypt/live/example.com/fullchain.pem"
tls_key  = "/etc/letsencrypt/live/example.com/privkey.pem"
```

Set up a cron job or systemd timer to call `certbot renew` and restart the service.

### Behind a Reverse Proxy (nginx / Caddy)

If your ingress layer (nginx, Caddy, AWS ALB, etc.) handles TLS termination, run `static-web` in plain HTTP mode and let the proxy forward requests to it:

```toml
# config.toml — no TLS, only HTTP
[server]
addr = ":8080"

[security]
# HSTS is meaningless here — proxy handles it
hsts_max_age = 0
```

**nginx upstream example:**

```nginx
upstream static_web {
    server 127.0.0.1:8080;
    keepalive 32;
}

server {
    listen 443 ssl http2;
    server_name example.com;

    ssl_certificate     /etc/ssl/certs/example.com.pem;
    ssl_certificate_key /etc/ssl/private/example.com.key;

    add_header Strict-Transport-Security "max-age=31536000" always;

    location / {
        proxy_pass         http://static_web;
        proxy_http_version 1.1;
        proxy_set_header   Connection "";
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
    }
}
```

---

## Pre-compressing Assets

Serving pre-compressed files is far more efficient than on-the-fly gzip, especially for large JavaScript bundles. Place `.gz` and `.br` files alongside originals:

```
public/
  app.js
  app.js.gz      ← served when client sends Accept-Encoding: gzip
  app.js.br      ← served when client sends Accept-Encoding: br (preferred over gzip)
  style.css
  style.css.gz
```

Generate them with the bundled Makefile target:

```bash
make precompress
```

Or manually (requires `gzip` and `brotli` installed):

```bash
# gzip
gzip -k -9 public/app.js          # keeps original, produces app.js.gz

# brotli
brotli -9 public/app.js -o public/app.js.br
```

Enable in config (on by default):

```toml
[compression]
precompressed = true
```

> **Note:** Brotli encoding is only available via pre-compressed `.br` sidecar files. On-the-fly brotli compression is not implemented.

---

## Docker Deployment

### Dockerfile

Multi-stage build — the final image is scratch-based (~7 MB).

```dockerfile
# syntax=docker/dockerfile:1

# ── Stage 1: build ──────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /static-web ./cmd/static-web

# ── Stage 2: runtime ────────────────────────────────────────────────────────
FROM scratch

# TLS root certificates (needed for outbound TLS, e.g. fetching Let's Encrypt chains)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# The binary
COPY --from=builder /static-web /static-web

# Static files (build them into the image or mount at runtime — see below)
COPY public/ /public/

EXPOSE 8080

ENTRYPOINT ["/static-web"]
```

> **Mounting static files at runtime** — skip the `COPY public/` line and mount a volume instead:
>
> ```bash
> docker run -v /path/to/site:/public -e STATIC_FILES_ROOT=/public ...
> ```

### docker-compose.yml

Configuration can be passed via environment variables (good for secrets and 12-factor deployments) or via CLI flags in the `command:` field (good for readability and quick overrides).

**Using environment variables:**

```yaml
version: "3.9"

services:
  static-web:
    build: .
    restart: unless-stopped
    ports:
      - "8080:8080"
      - "8443:8443" # optional — only needed when TLS is handled by this container
    volumes:
      - ./public:/public:ro # mount static files (read-only)
      - ./tls:/tls:ro       # mount TLS certs (read-only); omit if using a reverse proxy
    environment:
      STATIC_SERVER_ADDR:     ":8080"
      STATIC_SERVER_TLS_ADDR: ":8443"
      STATIC_SERVER_TLS_CERT: "/tls/server.crt" # omit if no TLS
      STATIC_SERVER_TLS_KEY:  "/tls/server.key" # omit if no TLS
      STATIC_FILES_ROOT:      "/public"
      STATIC_CACHE_MAX_BYTES: "134217728"        # 128 MB
    healthcheck:
      test: ["CMD", "/static-web", "version"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
    deploy:
      resources:
        limits:
          memory: 256M
```

**Using CLI flags (`command:`):**

```yaml
version: "3.9"

services:
  static-web:
    build: .
    restart: unless-stopped
    ports:
      - "8080:8080"
      - "8443:8443"
    volumes:
      - ./public:/public:ro
      - ./tls:/tls:ro
    command: >
      --port 8080
      --tls-port 8443
      --tls-cert /tls/server.crt
      --tls-key  /tls/server.key
      --cache-size 128MB
      /public
    healthcheck:
      test: ["CMD", "/static-web", "version"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
    deploy:
      resources:
        limits:
          memory: 256M
```

**No-TLS variant (reverse proxy in front) — env vars:**

```yaml
services:
  static-web:
    build: .
    restart: unless-stopped
    expose:
      - "8080" # internal only — not published to host
    volumes:
      - ./public:/public:ro
    environment:
      STATIC_SERVER_ADDR: ":8080"
      STATIC_FILES_ROOT:  "/public"
      STATIC_SECURITY_HSTS_MAX_AGE: "0" # proxy handles HSTS
```

**No-TLS variant — CLI flags:**

```yaml
services:
  static-web:
    build: .
    restart: unless-stopped
    expose:
      - "8080"
    volumes:
      - ./public:/public:ro
    command: --port 8080 --csp "default-src 'self'" /public
```

### Running the Container

**Using env vars (12-factor style):**

```bash
# build
docker build -t static-web:latest .

# run (no TLS, files in ./public)
docker run --rm -p 8080:8080 \
  -v "$(pwd)/public:/public:ro" \
  -e STATIC_FILES_ROOT=/public \
  static-web:latest

# run (with TLS)
docker run --rm -p 80:8080 -p 443:8443 \
  -v "$(pwd)/public:/public:ro" \
  -v "$(pwd)/tls:/tls:ro" \
  -e STATIC_SERVER_TLS_CERT=/tls/server.crt \
  -e STATIC_SERVER_TLS_KEY=/tls/server.key \
  static-web:latest
```

**Using CLI flags directly:**

```bash
# run (no TLS) — passing directory as positional argument
docker run --rm -p 8080:8080 \
  -v "$(pwd)/public:/public:ro" \
  static-web:latest /public

# run (with TLS) — all config via flags, no env vars needed
docker run --rm -p 80:8080 -p 443:8443 \
  -v "$(pwd)/public:/public:ro" \
  -v "$(pwd)/tls:/tls:ro" \
  static-web:latest \
  --tls-cert /tls/server.crt \
  --tls-key  /tls/server.key \
  /public

# run with directory listing, no access log spam
docker run --rm -p 8080:8080 \
  -v "$(pwd)/files:/public:ro" \
  static-web:latest --dir-listing --quiet /public
```

**Send SIGHUP to flush the cache without restarting:**

```bash
docker kill --signal=HUP <container_name_or_id>
```

**Maximum throughput with preload (Docker env vars):**

```bash
docker run --rm -p 8080:8080 \
  -v "$(pwd)/public:/public:ro" \
  -e STATIC_FILES_ROOT=/public \
  -e STATIC_CACHE_PRELOAD=true \
  -e STATIC_CACHE_GC_PERCENT=400 \
  static-web:latest
```

---

## Health Checks and Readiness Probes

The server does not expose a dedicated `/healthz` endpoint. Use a lightweight `GET` request to any known static file (e.g., `index.html`):

```bash
curl -fsS http://localhost:8080/ > /dev/null
```

**Kubernetes liveness + readiness probes:**

```yaml
livenessProbe:
  httpGet:
    path: /
    port: 8080
  initialDelaySeconds: 3
  periodSeconds: 15
  timeoutSeconds: 3
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /
    port: 8080
  initialDelaySeconds: 1
  periodSeconds: 5
  timeoutSeconds: 2
  failureThreshold: 2
```

**Docker health check (in compose or Dockerfile):**

```yaml
healthcheck:
  test: ["CMD-SHELL", "wget -qO- http://localhost:8080/ || exit 1"]
  interval: 30s
  timeout: 5s
  retries: 3
```

---

## Live Cache Flush (SIGHUP)

Send `SIGHUP` to flush both the in-memory LRU file cache and the path-safety cache without restarting the server. This is useful after deploying updated static files to disk — new requests will read fresh content from disk and repopulate the cache.

```bash
# by PID
kill -HUP $(pgrep static-web)

# by systemd service
systemctl kill --signal=HUP static-web.service

# in Docker
docker kill --signal=HUP <container_id>
```

> **Important:** SIGHUP flushes the file cache and the path-safety cache. It does **not** reload the configuration. Config changes require a full restart.

---

## Preloading for Maximum Performance

Enable `preload` to read every eligible file into the in-memory cache at startup. Combined with the fasthttp engine, this yields the highest possible throughput — up to **~141,000 req/sec** on Apple M-series (**55% faster than Bun's native static serve**, while including full security headers, TLS, and compression).

### Configuration

```toml
[cache]
enabled   = true
preload   = true       # load all files under [files.root] into RAM at startup
gc_percent = 400       # reduce GC frequency for throughput (default: 0 = Go default 100)
```

Or via CLI flags:

```bash
static-web --preload --gc-percent 400 ./dist
```

Or via environment variables:

```bash
STATIC_CACHE_PRELOAD=true STATIC_CACHE_GC_PERCENT=400 ./bin/static-web
```

### What preloading does

1. At startup, walks every file under `files.root`.
2. Files smaller than `max_file_size` are read into the LRU cache.
3. Pre-formatted `Content-Type` and `Content-Length` response headers are computed once per file.
4. The path-safety cache (`sync.Map`) is pre-warmed — the first request for any preloaded file skips `filepath.EvalSymlinks`.
5. Preload statistics (file count, total bytes, duration) are logged at startup.

### When to use preload

- **Ideal**: bounded set of static files (SPA builds, marketing sites, docs sites).
- **Not recommended**: very large file trees where total size exceeds `max_bytes`, or directories with frequent file changes.

### GC tuning

`gc_percent` sets the Go runtime `GOGC` target. A higher value means the GC runs less often, trading memory for throughput. The handler's hot path is allocation-free, and fasthttp reuses per-connection buffers (unlike net/http which allocates per-request). Recommended values:

| `gc_percent` | Behaviour |
|---|---|
| `0` | Do not change (Go default: 100) |
| `200` | Moderate: ~5% throughput boost |
| `400` | Aggressive: ~8% throughput boost (recommended with preload) |

---

## CORS Configuration

CORS is disabled by default. To enable it, set `cors_origins` in `config.toml` or via the environment variable.

**Allow a specific origin:**

```toml
[security]
cors_origins = ["https://app.example.com"]
```

**Allow multiple origins:**

```toml
[security]
cors_origins = [
  "https://app.example.com",
  "https://staging.example.com",
]
```

**Open CORS (public API / CDN):**

```toml
[security]
cors_origins = ["*"]
```

Using `["*"]` emits the literal `*` in the `Access-Control-Allow-Origin` response header. The request `Origin` is never reflected back (preventing origin confusion attacks).

**Via environment variable (comma-separated):**

```bash
STATIC_SECURITY_CORS_ORIGINS=https://app.example.com,https://staging.example.com ./bin/static-web
```

---

## Custom 404 Page

Create a `404.html` file in your static files directory and reference it in the config:

```toml
[files]
root      = "./public"
not_found = "404.html"    # relative to root
```

The custom 404 page is served with the correct `404 Not Found` status code. All security headers are still applied. The path is validated through the same symlink-safe check as all other paths — it cannot reference files outside `root`.

---

## Directory Listing

When enabled, `static-web` renders an HTML index page for any directory that is requested directly.

**Enable in config:**

```toml
[security]
directory_listing = true
```

**Or via environment variable:**

```bash
STATIC_SECURITY_DIRECTORY_LISTING=true ./bin/server
```

### Behaviour

- Enabled per-server (not per-directory).
- Entries are sorted: subdirectories first (alphabetically), then files (alphabetically).
- Each directory shows a `..` parent link except the root.
- A breadcrumb navigation bar shows the full path with clickable segments.
- File sizes are displayed in human-readable format (B / KB / MB / GB).
- Last-modified timestamps are shown in UTC.
- When `block_dotfiles = true` (the default), files and directories whose names start with `.` are hidden from the listing. They also cannot be accessed directly.
- `HEAD` requests return `200` with no body (correct for use with health checks / probes).
- All security headers (`X-Content-Type-Options`, `CSP`, etc.) are set on listing responses.

### Security note

Directory listing is **disabled by default** (`directory_listing = false`). Enable it only when you intentionally want to expose the directory tree — for example, a file download server or a local development environment. Do not enable it on a production web application that serves an SPA or a site with an `index.html` at each route.

---

## Known Limitations

| Limitation                            | Impact                                                           | Workaround                                                                         |
| ------------------------------------- | ---------------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| **Brotli on-the-fly not implemented** | Brotli encoding requires pre-compressed `.br` files.             | Run `make precompress` as part of your build pipeline.                             |
| **No hot config reload**              | SIGHUP flushes the cache only; config changes require a restart. | Use a process manager (systemd, Docker restart policy) for zero-downtime restarts. |

---

## Troubleshooting

### `403 Forbidden` on a file that exists

The most common causes:

1. **Dotfile protection** — the path contains a component that starts with `.` (e.g., `.well-known`, `.env`). If you need to serve `.well-known/` for ACME challenges, disable `block_dotfiles` or use a reverse proxy to serve that path separately.

   ```toml
   [security]
   block_dotfiles = false
   ```

2. **Path traversal blocked** — the resolved path (after following symlinks) falls outside `root`. Move the files inside `root` or ensure symlinks do not point outside it.

### `405 Method Not Allowed`

The server only accepts `GET`, `HEAD`, and `OPTIONS`. Any other method (POST, PUT, DELETE, PATCH, TRACE, etc.) is rejected with `405`. This is intentional — it's a static file server, not an API. If your browser is sending a `POST` request, check your HTML form actions and JavaScript fetch calls.

### Files are stale after a deploy

The in-memory cache serves files from memory after the first request (or immediately if `preload = true`). After deploying new files to disk, flush both the file cache and the path-safety cache:

```bash
kill -HUP $(pgrep static-web)
```

If `cache.ttl` is `0`, entries remain cached until eviction pressure or SIGHUP flush. If `cache.ttl` is greater than `0`, stale entries are evicted automatically on access.

### Compression not working

1. Verify `compression.enabled = true` in config.
2. Check that the response is larger than `compression.min_size` (default: 1024 bytes).
3. The client must send `Accept-Encoding: gzip`. Browsers do this automatically; `curl` does not by default — use `curl --compressed`.
4. Some content types are not compressed (images, video, audio, pre-compressed archives). This is intentional — re-compressing already-compressed data makes files larger.

### HTTPS redirect loop

If you're behind a reverse proxy that already handles HTTPS and you have `tls_cert` / `tls_key` set on the container, the HTTP→HTTPS redirect will fire on the internal HTTP port. Solution: don't set `tls_cert` / `tls_key` when TLS is terminated at the proxy. Run the container in plain HTTP mode.

### `connection refused` on startup

The default port is `:8080`. Verify:

- No other process is bound to the port: `lsof -i :8080`
- The `STATIC_SERVER_ADDR` env var or `server.addr` config value matches what you're connecting to.
- In Docker, the container port is published: `-p 8080:8080`.

### High memory usage

The in-memory cache holds file contents in memory. By default the cap is 256 MB. Reduce it if needed:

```toml
[cache]
max_bytes = 67108864   # 64 MB
```

Or disable caching entirely for disk-constrained environments:

```toml
[cache]
enabled = false
```

### Security headers missing from error responses

All security headers (`X-Content-Type-Options`, `X-Frame-Options`, `CSP`, etc.) are set before path evaluation, so they are present on **all** responses including `400`, `403`, `404`, and `405`. If you're not seeing them, check whether an upstream proxy is stripping or overwriting them.
