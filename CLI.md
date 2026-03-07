# CLI Design — `static-web`

This document is the reference for the `static-web` command-line interface. It covers the command structure, flag design, installation methods, configuration priority, and implementation notes. It is the authoritative reference for usage, flags, and build integration.

---

## Table of Contents

- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Binary Name](#binary-name)
- [Command Structure](#command-structure)
- [Subcommands](#subcommands)
  - [`serve` (default)](#serve-default)
  - [`init`](#init)
  - [`version`](#version)
- [Configuration Priority](#configuration-priority)
- [Flag Reference](#flag-reference)
- [Usage Examples](#usage-examples)
- [Installation Methods](#installation-methods)
- [Build-Time Version Injection](#build-time-version-injection)
- [Implementation Notes](#implementation-notes)
- [Decisions & Rationale](#decisions--rationale)

---

## Goals

1. **Zero-friction start.** `static-web ./dist` should be all a developer needs to type to serve a directory.
2. **No new runtime dependencies.** Implement with Go stdlib `flag` package only — no cobra, urfave/cli, or any external CLI framework.
3. **Flags for the common case; config file for everything else.** CLI flags cover the ~10 settings changed most often. The full config surface remains accessible via `config.toml` and environment variables.
4. **Installable as a global tool.** `go install`, Homebrew, pre-built binaries, and a curl one-liner all work.
5. **Consistent with Unix conventions.** Flags use `--long-form` style. Exit 0 on success, non-zero on error. `--help` prints to stdout. Errors print to stderr.

---

## Non-Goals

- Shell completion scripts (can be added later, not in scope for v1).
- A `start`/`stop`/`status` daemon manager — that is the OS's job (systemd, launchd).
- Interactive prompts or wizards.
- A web UI or admin API.

---

## Binary Name

```
static-web
```

- Matches the repository name.
- Descriptive and unambiguous.
- Hyphenated names tab-complete correctly on all major shells.
- Avoids collision with system binaries (`httpd`, `nginx`, `caddy` are already taken).

---

## Command Structure

```
static-web [command] [flags] [directory]
```

When no subcommand is given, `serve` is assumed. This means the most common usage is just:

```
static-web ./dist
static-web --port 3000 .
```

The three subcommands are:

| Subcommand | Purpose |
|------------|---------|
| `serve` | Start the file server (default when omitted) |
| `init` | Scaffold a `config.toml` in the current directory |
| `version` | Print version, Go version, OS/arch and exit |

---

## Subcommands

### `serve` (default)

Start the static file server.

```
static-web serve [flags] [directory]
static-web [flags] [directory]       # shorthand — serve is the default
```

**Positional argument:**

`directory` — the directory to serve. Defaults to `./public` if omitted. Overrides `files.root` from the config file and the `STATIC_FILES_ROOT` env var.

```bash
static-web                     # serves ./public on :8080
static-web ./dist              # serves ./dist on :8080
static-web --port 3000 ./dist  # serves ./dist on :3000
```

---

### `init`

Write a `config.toml` file to the current directory (or to `--output`), populated with all options and their defaults, ready to edit.

```
static-web init [--output path]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--output` | `./config.toml` | Path to write the config file |
| `--force` | `false` | Overwrite if file already exists |

**Behaviour:**

- Writes the same content as `config.toml.example`, including all comments.
- If the file already exists and `--force` is not set, prints an error and exits 1.
- Prints the absolute path of the written file on success.

```bash
static-web init                         # writes ./config.toml
static-web init --output /etc/static-web/config.toml
static-web init --force                 # overwrite existing
```

---

### `version`

Print version information and exit 0.

```
static-web version
```

**Output format:**

```
static-web v1.2.3
  go:     go1.23.4
  os:     darwin/arm64
  commit: a1b2c3d4
```

Version, commit, and build date are injected at build time via `-ldflags` (see [Build-Time Version Injection](#build-time-version-injection)). When not injected (e.g. `go run`), values fall back to `dev`.

---

## Configuration Priority

Settings are resolved in this order, highest priority first:

```
1. CLI flags          (--port, --host, --tls-cert, ...)
2. Environment vars   (STATIC_SERVER_ADDR, STATIC_FILES_ROOT, ...)
3. Config file        (config.toml, or path from --config)
4. Built-in defaults  (:8080, ./public, cache=true, ...)
```

This is the standard Unix/12-factor convention. A flag always wins, even over an env var set in the same shell. The config file is optional — if it does not exist the server starts with defaults.

---

## Flag Reference

### Global flags (available on all subcommands)

| Flag | Type | Description |
|------|------|-------------|
| `--config` | string | Path to TOML config file (default: `./config.toml`) |
| `--help`, `-h` | bool | Print help and exit |

### `serve` flags

Grouped by concern for readability. All flags are optional; unset flags do not override the config file.

#### Network

| Flag | Type | Default | Config field |
|------|------|---------|--------------|
| `--host` | string | `` (all interfaces) | `server.addr` (host part) |
| `--port`, `-p` | int | `8080` | `server.addr` (port part) |
| `--tls-cert` | string | — | `server.tls_cert` |
| `--tls-key` | string | — | `server.tls_key` |
| `--tls-port` | int | `8443` | `server.tls_addr` (port part) |

> `--host` and `--port` are combined into `server.addr` as `<host>:<port>`. Specifying `--host` alone without `--port` uses the default port (8080), and vice versa.

#### Files

| Flag | Type | Default | Config field |
|------|------|---------|--------------|
| `--index` | string | `index.html` | `files.index` |
| `--404` | string | — | `files.not_found` |

#### Cache

| Flag | Type | Default | Config field |
|------|------|---------|--------------|
| `--no-cache` | bool | `false` | `cache.enabled = false` |
| `--cache-size` | string | `256MB` | `cache.max_bytes` (parses `256MB`, `64MB`, `1GB`) |

#### Compression

| Flag | Type | Default | Config field |
|------|------|---------|--------------|
| `--no-compress` | bool | `false` | `compression.enabled = false` |

#### Security

| Flag | Type | Default | Config field |
|------|------|---------|--------------|
| `--cors` | string | — | `security.cors_origins` (comma-separated, or `*`) |
| `--dir-listing` | bool | `false` | `security.directory_listing` |
| `--no-dotfile-block` | bool | `false` | `security.block_dotfiles = false` |
| `--csp` | string | — | `security.csp` |

#### Logging / output

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--quiet`, `-q` | bool | `false` | Suppress per-request access log lines |
| `--verbose` | bool | `false` | Log config values on startup |

---

## Usage Examples

```bash
# Serve current directory on port 8080
static-web .

# Serve a build output directory on a custom port
static-web --port 3000 ./dist

# Enable directory listing (e.g. for a local file share)
static-web --dir-listing --no-dotfile-block ~/Downloads

# Serve with TLS (HTTPS on :443, HTTP redirect on :80)
static-web --port 80 --tls-port 443 \
           --tls-cert /etc/ssl/cert.pem \
           --tls-key  /etc/ssl/key.pem \
           ./public

# Open CORS (for a public API or CDN-served assets)
static-web --cors '*' ./dist

# CORS for specific origins
static-web --cors 'https://app.example.com,https://staging.example.com' ./dist

# Use a config file for all settings
static-web --config /etc/static-web/config.toml

# Scaffold a config file, then edit and run
static-web init
$EDITOR config.toml
static-web

# Disable caching (useful during local development to see file changes immediately)
static-web --no-cache ./dist

# Print version info
static-web version
```

---

## Installation Methods

### 1. `go install` (recommended for Go developers)

```bash
go install github.com/static-web/server/cmd/static-web@latest
```

Requires Go 1.26+. Installs to `$(go env GOPATH)/bin/static-web`. Add `$(go env GOPATH)/bin` to your `PATH` if not already there.

### 2. Homebrew (macOS / Linux)

```bash
brew install static-web/tap/static-web
```

Or with the full tap URL:

```bash
brew tap static-web/tap https://github.com/static-web/homebrew-tap
brew install static-web
```

Auto-updates with `brew upgrade`.

### 3. Pre-built binaries (GitHub Releases)

Download a binary for your platform from the [GitHub Releases](https://github.com/static-web/server/releases) page. Binaries are published for:

| Platform | File |
|----------|------|
| macOS (Apple Silicon) | `static-web_darwin_arm64.tar.gz` |
| macOS (Intel) | `static-web_darwin_amd64.tar.gz` |
| Linux (x86-64) | `static-web_linux_amd64.tar.gz` |
| Linux (ARM64) | `static-web_linux_arm64.tar.gz` |
| Windows (x86-64) | `static-web_windows_amd64.zip` |

**Quick install on Linux/macOS:**

```bash
# Replace X.Y.Z with the desired version, and PLATFORM/ARCH with your system
curl -fsSL https://github.com/static-web/server/releases/download/vX.Y.Z/static-web_linux_amd64.tar.gz \
  | tar -xz -C /usr/local/bin static-web
chmod +x /usr/local/bin/static-web
```

### 4. One-liner install script

```bash
curl -fsSL https://static-web.dev/install.sh | sh
```

The script:
1. Detects OS and architecture.
2. Downloads the latest release binary from GitHub.
3. Installs to `/usr/local/bin` (or `~/.local/bin` if `/usr/local/bin` is not writable).
4. Verifies the SHA256 checksum before installing.
5. Prints the installed version on success.

To install a specific version:

```bash
curl -fsSL https://static-web.dev/install.sh | sh -s -- --version v1.2.3
```

### 5. Docker

```bash
docker run --rm -p 8080:8080 -v "$(pwd)/dist:/public:ro" ghcr.io/static-web/server:latest
```

See `USER_GUIDE.md` for full Docker and docker-compose examples.

---

## Build-Time Version Injection

Version information is injected at link time via `-ldflags`. The variables live in a new `internal/version` package:

```go
// internal/version/version.go
package version

var (
    Version = "dev"
    Commit  = "none"
    Date    = "unknown"
)
```

Build command:

```bash
go build \
  -ldflags="-X github.com/static-web/server/internal/version.Version=v1.2.3 \
            -X github.com/static-web/server/internal/version.Commit=$(git rev-parse --short HEAD) \
            -X github.com/static-web/server/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o bin/static-web ./cmd/static-web
```

This is added to the `Makefile` as the `release` target and used by GoReleaser in CI.

---

## Implementation Notes

The CLI was implemented using Go stdlib `flag.FlagSet` — no external framework. Key implementation details:

- **Subcommand dispatch**: `os.Args[1]` switch in `main()`. Unknown first arguments (flags, paths) fall through to the implicit `serve` subcommand.
- **Flag isolation**: each subcommand owns its own `flag.FlagSet` with `flag.ContinueOnError`, so flags don't bleed between subcommands.
- **Config layering**: `config.Load()` handles defaults + TOML file + env vars. `applyFlagOverrides()` in `main.go` applies CLI flags on top as the final layer.
- **`--host` + `--port` merging**: `net.SplitHostPort` / `net.JoinHostPort` used to decompose and reconstruct `server.addr`.
- **`parseBytes()`**: a small helper that parses `256MB`, `1GB`, etc. with `B`/`KB`/`MB`/`GB` suffixes (case-insensitive).
- **`//go:embed config.toml.example`**: the example config is embedded in `cmd/static-web/` at compile time. The binary is fully self-contained.
- **`--quiet`**: passes `io.Discard` to a `loggingMiddlewareWithWriter` variant, suppressing access log output with zero overhead.
- **`--verbose`**: calls `logConfig(cfg)` after all overrides are applied, so you see the final resolved values.
- **Version injection**: `internal/version.Version`, `Commit`, `Date` are set via `-ldflags` at build time. Default to `"dev"`, `"none"`, `"unknown"` for `go run`.

---

## Decisions & Rationale

### No external CLI framework

cobra and urfave/cli are excellent libraries, but they add a dependency, and this project's explicit design constraint is "stdlib-first, minimal external deps". The subcommand surface is small (3 commands), and `flag.FlagSet` handles per-subcommand flags cleanly without any framework.

### `serve` as the default (omittable) subcommand

`static-web ./dist` is significantly more ergonomic than `static-web serve ./dist` for the majority use case. The serve subcommand is still explicit when needed. This pattern is used by tools like `python -m http.server`, `npx serve`, and `caddy file-server`.

### `--port` not `--addr`

`--port 3000` is what every developer reaches for. `--addr :3000` is correct but unusual for a CLI tool. We accept `--host` + `--port` separately and combine them, which is more intuitive even if slightly more code.

### Positional directory argument

Following the precedent of `python -m http.server`, `caddy file-server --root`, `npx serve`, and `ruby -run -e httpd`. The directory is the most commonly varied parameter — making it positional reduces typing.

### `--no-cache` and `--no-compress` rather than `--cache` / `--compress`

Boolean flags that default to `true` are awkward as positional booleans in CLI (`--cache=false` is ugly). The `--no-*` pattern (used by npm, git, curl) is idiomatic and readable: "I want no caching."

### `--cache-size` uses human-readable suffixes

`--cache-size 128MB` is more readable than `--cache-size 134217728`. The parser handles `B`, `KB`, `MB`, `GB` (and lowercase variants). Invalid values print an error and exit 1.

### `init` uses `//go:embed` not a runtime file lookup

Embedding the example config at compile time means the binary is fully self-contained. Running `static-web init` works correctly regardless of the current working directory, even if installed as a global binary to `/usr/local/bin`.

### Version information via `-ldflags`

Standard Go practice. Avoids a generated file that would pollute diffs. The `internal/version` package has zero dependencies and is importable by any future tooling.
