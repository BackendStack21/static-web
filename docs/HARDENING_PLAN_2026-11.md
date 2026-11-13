# Hardening Plan — 2026-11

Produced by a three-reviewer panel (filesystem/path security, HTTP protocol & DoS,
performance) against `7b602b3` (v1.7.0). Each finding was verified with a throwaway
reproduction test or benchmark unless marked *reasoned*. Items marked ✔ were also
re-run independently before fixing. The operations / concurrency / supply-chain slice
was **not** reviewed in this round.

"Default config?" says whether the finding is reachable with shipped defaults
(`preload = false`, `ttl = 0s`, `precompressed = true`, compression and cache on).

## P0 — security

| # | Finding | Location | Default config? | Fix | Commit |
|---|---------|----------|-----------------|-----|--------|
| FS-1 ✔ | PathCache kept a "safe" verdict for existing paths forever; replacing a file or directory with a symlink later served content from outside the root (until SIGHUP) | `security/security.go`, `handler/file.go` | No (`preload`); needs write access inside root | Lexical checks on every request; `EvalSymlinks` containment skipped only for file-cache hits; every file-cache miss re-validates and refreshes/drops the PathCache entry | `c2a9e1a` |

## P1 — security & correctness

| # | Finding | Default config? | Fix | Commit |
|---|---------|-----------------|-----|--------|
| HTTP-F2 ✔ | An ignored `Range` (malformed, over-cap, any If-Range mismatch) dropped the cached encoded variant → on-the-fly gzip per request (1 MiB: 72 µs → 9.2 ms); HEAD/GET disagreed on length/encoding | Yes, unauthenticated | `serveRange` answers only 206/416; an ignored Range falls through to the normal 200 with the negotiated variant | `6623d10` |
| FS-3 ✔ | Sidecar validation skipped the dotfile rule: `big.js.br -> .env` served `.env` with `Content-Encoding: br` | Yes (`precompressed`); needs write access inside root | Dot-segment rule applied to the resolved sidecar target; rejected sidecars fall back | `0341f74` |
| HTTP-F1 ✔ | `write_timeout` was a total per-response deadline (fasthttp sets it once), truncating large downloads slower than size/10 s; all ETags weak, so browser If-Range never resumed | Yes | Large-file body stream extends the deadline per chunk, capped at `start + wt + size/32 KiB/s`; strong ETag on the streamed large-file path only | `832ce33` |
| FS-2 | PreWarm stored relative paths when `files.root` was relative (default `./public`): grown files 403'd, sidecars ignored | No (`preload`) | Root resolved once (`security.ResolveRoot`) in `main` and PreWarm | `9b68016` |
| P-F7 ✔ | With a PathCache, a 404 validated the path twice (17.8 → 34 µs, 87 → 168 allocs) — October regression | Yes after P-F1 | Middleware hands its freshly validated path to the handler | `c2a9e1a` |

## P1 — performance

| # | Finding | Before → after | Commit |
|---|---------|----------------|--------|
| P-F1 ✔ | PathCache only existed with `--preload`; default cached hits ran `EvalSymlinks` (16.4 µs, 81 allocs) | default hit ≈ 1.4 µs; no-PathCache hit 18.7 → 10.2 µs (root resolved once) | `57c8fb7` |
| P-F4 | Cache disabled: every request read, hashed, gzipped **and** zstd'd the file | 11 KB GET 69 → 42 µs, 171 → 70 allocs | `652d223`, `5a08b0a` |
| P-F5 | New gzip/zstd encoder per `GzipBytes`/`ZstdBytes` call | 11 KB: 79 µs → 5.9 µs, 204 µs → 3.6 µs; preload 2000×8 KB: 2.1 GiB → 21 MiB allocated | `2cc1bf7` |
| P-F3 | Vary merging allocated on every hit (October regression, +13–47%) | hit allocs 6–15 → 2 | `0045344` |
| G1 | Cached hits took 3 exclusive LRU locks; 10 cores ≈ 1.4× one core | parallel hits −30% @10 procs | `c2a9e1a` |
| P-F6 | Large-file stream wrapper defeated sendfile | 64 MiB loopback ≈ 3–4 → 7.4 GiB/s | `e424dda` |

## P2

| # | Finding | Fix | Commit |
|---|---------|-----|--------|
| FS-4 ✔ | FIFO (or socket/device) inside root blocked the handler forever; also as a sidecar and as the custom 404 page | Non-regular targets → 403; non-regular sidecars skipped; non-regular 404 page refused at startup | `dc16233`, `6e05ac0`, `6fcc2c5` |
| FS-5 | Dir listing: hrefs not path-escaped (`#`, `?`); symlink entries shown with `Lstat` info | `url.URL.EscapedPath`; symlinks resolved via `PathSafe`, unsafe/dangling ones hidden; non-regular entries hidden | `2095d0f`, `cbe52f3` |
| HTTP-F4 ✔ | `ctx.Error` wiped CSP/nosniff/CORS/HSTS on error responses; 413 disclosed the configured limit | Header-preserving `writeError`; HSTS set after the handler; fasthttp's own 400/431/408 get security headers via `ErrorHandler`; generic 413 body | `7b343d3`, `21bae93`, `23eaa9a` |
| HTTP-F5 | `Vary: Origin` only sent for allowed origins (CDN served the no-ACAO variant) | Always vary on Origin for a specific CORS list | `fb1a75d` |
| HTTP-F6 ✔ | `Server: fasthttp` still sent — SEC-007 fix was ineffective | `NoDefaultServerHeader: true`; wire-level test | `f7b1747` |
| HTTP-F7 | Default CSP blocked the listing's inline `<style>` (*reasoned*) | Listing response's CSP gets the style's sha256 | `c975c79` |
| P-F8 | Cache accounting counted `len`, not `cap` (1.4–2× real memory); replace-while-full double-subtracted; oversize replacement left the stale entry | Count capacity; remove old entry before eviction; drop stale entry | `8f48a12`, `6631e11` |
| P-F9 | Preload built only gzip (no br sidecar / zstd) and cached sidecars as entries | Shared `cache.FillVariants` for preload and lazy fills; sidecars validated like the handler | `b885d67`, `f11e28c`, `652d223` |
| P-F10 | Unbuffered access-log write per request (+80% serial, 3.5× parallel) | Buffered writer, flushed every ≤ 500 ms / 64 KiB and on shutdown | `3e11b3e` |
| — | Access log recorded size 0 for on-the-fly gzipped responses; panicked requests not logged | Real body size; logging inside recovery | `2cb93b7`, `1cb71ed` |
| — | `/file.js/` served the file and cached it a second time | 404 for a regular file with a trailing slash | `fe93ff1` |
| — | `X-Cache: HIT` reported on a disk read | MISS on disk reads | `20070a1` |

## Behaviour changes

- `GET /file.ext/` → 404 (was 200 with the file).
- Non-regular files (FIFO, socket, device) → 403; an index entry that is a directory → 403 (was 500).
- A URL component starting with `.` is refused even when it is a symlink to a non-dot name.
- Large-file ETags are strong (`"size-mtime"`); clients holding old `W/"…"` tags still get 304 (If-None-Match is weak comparison).
- For large files, `write_timeout` is a progress deadline with a 32 KiB/s minimum average rate.
- With a PathCache (now the default), file-cache misses — including all large files — pay one `EvalSymlinks` per request.
- Cache accounting counts allocated capacity, so slightly fewer entries fit in the same `max_bytes`.
- Cache disabled: br/zstd served only from sidecars; otherwise the compress middleware gzips.
- Preload pre-compresses exactly the types `compress.IsCompressible` accepts (previously a broader prefix list).
- Access-log lines are buffered (≤ 500 ms / 64 KiB) and flushed on graceful shutdown; up to that much can be lost on SIGKILL/crash.
- No `Server` header anywhere; security headers (and HSTS on TLS) on fasthttp's own error responses.
- 413 body no longer discloses `max_serve_file_size`.

## Deferred / decisions for the maintainer

- **`os.Root`** for opening files: would close the remaining check-then-open race structurally, but rejects absolute symlinks even when they point inside the root.
- **G3** (lock-free read index in front of both LRUs, −76% parallel hit latency in the prototype): changes LRU recency semantics; breaks `TestPathCache_LookupPromotesEntry` as written.
- **Case-variant cache aliases** (`/APP.JS` vs `/app.js` on case-insensitive filesystems): bounded by the LRU; canonicalising case is a platform-specific design change.
- **sendfile with `max_conns_per_ip > 0`**: fasthttp's per-IP connection wrapper hides `ReadFrom`, so large files fall back to buffered copies. Upstream limitation.
- **TLS large-file writes** go through fasthttp's 4 KiB bufio chunks.
- **Operations / concurrency / supply-chain review** (race under SIGHUP reload, `govulncheck`, workflows, goreleaser) was not performed this round.
