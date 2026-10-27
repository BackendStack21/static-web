# Hardening Plan — 2026-10

Produced by a four-expert review panel (application security, Go concurrency /
correctness, HTTP semantics, SRE / release engineering). Each finding was
verified with a throwaway reproduction test against HEAD (`43316d9`) unless
marked *reasoned*. Items found independently by multiple experts are marked ★.

## P0 — security & data integrity

| # | Finding | Location | Fix |
|---|---------|----------|-----|
| 1 | ★ Multi-range amplification: `bytes=0-,0-,…` (≈1300 ranges in 4 KB) builds a 1300× body in memory | `handler/file.go` `parseRange` | Cap ranges, reject overlap/over-size totals, 416 for unsatisfiable |
| 2 | ★ On-the-fly gzip applied to 206 responses → body no longer matches `Content-Range` | `compress/compress.go` | Compress only `200` responses |
| 3 | Directory index bypasses symlink-escape & dotfile checks (`/sub/` serves `sub/index.html → /outside/secret`) | `handler/file.go` `resolveIndexPath` | Validate the index path through `PathSafe` |
| 4 | PathCache stores "safe" verdict for non-existent paths; later symlink swap escapes root | `security/security.go` | Don't cache paths that do not exist |
| 5 | ★ Large files (> `cache.max_file_size`) read fully into memory per request, including HEAD; no validators, no 304 | `handler/file.go` `serveLargeFile` | Stream from disk, stat-only HEAD, ETag/Last-Modified/304 |
| 6 | ★ Listener start failure is only logged; process stays up serving nothing. Unreadable TLS cert panics in a goroutine | `cmd/static-web/main.go`, `server/server.go` | Propagate errors, exit non-zero, load cert up front |

## P1 — protocol correctness & robustness

| # | Finding | Fix |
|---|---------|-----|
| 7 | `Vary: Origin` overwritten by `Vary: Accept-Encoding` (CORS cache poisoning); Vary duplicated up to 3× | Merge Vary tokens instead of `Set` |
| 8 | 304 omits `Cache-Control` / `Vary` / `ETag` | Emit validators + cache headers on 304 |
| 9 | HEAD `Content-Length` = identity size while advertising `Content-Encoding` | Use encoded length |
| 10 | No `If-Range` support (resume splices two versions) | Honour `If-Range` (date or ETag) |
| 11 | If-None-Match uses exact, not weak, comparison | Weak comparison per RFC 9110 §13.1.2 |
| 12 | OPTIONS without CORS returns file body; 405 lacks `Allow` | 204 + `Allow`; `Allow` on 405 |
| 13 | Preload uses symlink's `Lstat` size/mtime (HEAD reports `Content-Length: 5`) | `os.Stat` the target |
| 14 | Cache TTL eviction in `Get` can evict a freshly `Put` entry | Compare-and-remove under lock |
| 15 | Sidecar files read without size bound | Skip sidecars larger than source |
| 16 | Accept-Encoding coding names matched case-sensitively | Case-insensitive match |

## P1 — operations

| # | Finding | Fix |
|---|---------|-----|
| 17 | No config validation (negative sizes, bad levels, only one of cert/key) and env parse errors silently ignored | `Config.Validate()`, report bad env values |
| 18 | Explicit `--config` pointing to a missing file is silently ignored | Error unless it is the implicit default |
| 19 | SIGHUP reload drops CLI flag overrides / positional root | Re-apply overrides; document reload scope |
| 20 | Listeners hard-code `tcp4` (IPv6 hosts fail) | Use `tcp` |
| 21 | `helpText` prints a literal `%%` | Fix string |
| 22 | Docs mention env vars that do not exist; embedded and root `config.toml.example` drifted | Align docs/examples |

## P2 — CI / release

- Pin `govulncheck`; add `staticcheck` and `gofmt` checks; add `go mod tidy -diff` check.
- Remove unused code flagged by staticcheck (`zstdWriterPool`, `zstdBufPool`).
- Dependabot for `gomod` and `github-actions`; pin action comments to exact versions.
- Scope `contents: write` to the release job; drop `go mod tidy` release hook.

## Deferred (behaviour changes — need maintainer decision)

- 301 redirect `/dir` → `/dir/` (currently serves the index directly).
- q-value–ordered encoding preference (currently fixed br > zstd > gzip).
- Default `MaxConnsPerIP` / `Concurrency` limits.
- True live config reload via `atomic.Pointer[config.Config]`.
- Release signing (cosign), SBOM, provenance.
- Serving via `os.OpenRoot` to close remaining check-then-open TOCTOU.

## Status (2026-10-27)

All P0, P1 and P2 items above are implemented on `hardening/2026-10`, each bug with a
regression test written first (RED → GREEN). Additional fixes found during the work:
embedded default assets now honour conditional requests; `serveLargeFile` HEAD no
longer opens the file (except to sniff an unknown type).

Implementation notes:
- #20: `tcp4` is kept for empty/IPv4 hosts (documented throughput choice); `tcp` is used
  for IPv6 literals and hostnames.
- Behaviour changes: invalid env values (e.g. `STATIC_X=yes` for booleans) and
  out-of-range config values now abort startup instead of being ignored; an explicit
  `--config` path must exist; unsatisfiable ranges return 416; OPTIONS returns 204.
- On Linux, `SO_REUSEPORT` still lets a second instance bind the same port (deferred).
