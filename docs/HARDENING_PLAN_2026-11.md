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
| P-F1 ✔ | PathCache only existed with `--preload`; default cached hits ran `EvalSymlinks` (16.4 µs, 81 allocs) | default-config hit 17.1 → 1.28 µs (−92.5%), 85 → 2 allocs; default-config 404 18.1 → 10.7 µs | `57c8fb7` |
| P-F4 | Cache disabled: every request read, hashed, gzipped **and** zstd'd the file | 11 KB GET 69 → 42 µs, 171 → 70 allocs; AR-1 then added ~12 µs per disk read (opening through `os.Root`): 11 KB GET ≈ 49 µs, 62 µs with compression | `652d223`, `5a08b0a` |
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
| — | Disk-miss path never answered 304 (every conditional request with the cache off got a full 200) | `CheckNotModified` on the disk-miss path, same headers as a cache-hit 304 | `3a8f688` |
| — | HEAD with a satisfiable `Range` returned 206 (RFC 9110 defines Range for GET only) | Range evaluated only for GET | `6e52e25` |
| — | Redirect listener's invalid-host 400 used `ctx.Error`, dropping earlier headers | Set status/body directly | `d36ce8f` |
| R-1 | With `compression.min_size = 0`, a large-file HEAD was gzipped (empty body) while GET streamed identity | Middleware never encodes a bodiless HEAD that declares its length | `fc35ce1` |
| R-2 | After a PathCache hit, a `__safePath` value set by an outer layer would pass as validated | Cleared on PathCache hits | `31f8b52` |
| R-3 | Directory-listing HEAD reported identity / length 0 while GET was gzipped (pre-existing) | HEAD renders the listing; fasthttp never sends it | `4c3f90f` |
| R-4 | Lazy fills took size/mtime from an earlier stat; a file changing in between was cached with a wrong length, or read past `max_file_size` (pre-existing) | `cache.ReadRegularFile` (shared with preload); files that grew/changed are streamed once, uncached | `9bc5b1f` |

## Independent review

A fresh reviewer audited the combined diff (`7b602b3..d36ce8f`) with adversarial throwaway tests: cached-directory and large-file symlink swaps, sidecar chains to dotfiles and outside the root with the cache off, a 2M-case lexical-check fuzz, a sendfile recorder on the TCP connection, and a short-file response-smuggling probe on a keep-alive connection. It found no P0/P1; R-1 to R-4 above came out of that review and were fixed with regression tests. Its symlink-swap probes swapped paths *between* requests; the adversarial round below raced swaps *within* a request and found the check-then-open race (AR-1), which R-4's fallback had widened.

## Adversarial review (2026-11-14)

Three more reviewers attacked `7b602b3..d4e38f9`: filesystem/path security and cache, HTTP semantics/DoS/concurrency, and published claims against the code. Every finding came with a reproduction; the fixes have deterministic regression tests, each checked to fail with its fix reverted. The reviewers' race probes were re-run against the fixes: 0 leaks in ~5.7M requests (in-process and over TCP, cache on/off, small and large files, dangling links), where they had leaked within a second.

| # | Severity | Finding | Fix | Commit |
|---|----------|---------|-----|--------|
| AR-1 | P1 | Check-then-open race: a path swapped for a symlink between validation and open was followed — out of the root, or to a blocked dotfile. Large files (default config) and every file with the cache off; R-4's `ErrFileChanged` fallback stat'ed and streamed the swapped path. Also directory listings. | Every read and listing opens through an `os.Root` for the resolved root (`cache.OpenInRoot`), after an `Lstat`, and must be that file (`cache.OpenSameFile`) | `dbbc988` |
| AR-2 | P1 | A large file renamed over between the request's stat and open was served as the new bytes under the old strong ETag and length; If-Range with the old ETag got a 206 of the new bytes | `serveLargeFile` opens first and describes the handle | `dbbc988` |
| AR-3 | P1 | Wildcard CORS sent `ACAO: *` only when `Origin` was present, without `Vary: Origin`: a shared cache could serve the no-ACAO variant cross-origin (pre-existing) | `*` on every response | `7d1c757` |
| AR-4 | P2 | With listing on, a cached or preloaded `index.html` replaced the directory listing (cache-state dependent; pre-existing) | Directory URLs skip the file-cache fast path when listing is on | `4e16ec7` |
| AR-5 | P2 | `max_serve_file_size` was enforced only above `cache.max_file_size` | In-memory limit capped by the serving limit (`Config.MaxInMemoryFileSize`), for the handler and preload | `3d6750b` |
| AR-6 | P3 | Preload stored each index file twice (under the directory URL too), counting its bytes twice | One entry per index file | `5d9f64e` |
| AR-7 | P3 | The large-file rate cap overflowed (≈276 TiB body or a ~292-year `write_timeout`), failing the first chunk | Saturates | `174d1ff` |
| AR-8 | P3 | Unreadable large file: HEAD 200, GET 403 (pre-existing) | HEAD opens the file (AR-2) | `dbbc988` |
| AR-9 | — | R-4's mtime half had no test | Test | `820f388` |
| AR-10 | P1–P3 (docs) | False or stale claims: HTTP/2 (fasthttp is HTTP/1.1 only), every key has an env var, `max_compress_size` scope, security headers on *every* response, TLS examples without `redirect_host` (the HTTP port answers 400), zstd on the fly, and ~20 smaller ones | Docs corrected | `2a08ddf`…`41c4b74` and this commit |

## Behaviour changes

- `GET /file.ext/` → 404 (was 200 with the file).
- Non-regular files (FIFO, socket, device) → 403; an index entry that is a directory → 403 (was 500).
- A URL component starting with `.` is refused even when it is a symlink to a non-dot name.
- Large-file ETags are strong (`"size-mtime"`); clients holding old `W/"…"` tags still get 304 (If-None-Match is weak comparison).
- For large files, `write_timeout` is a progress deadline with a 32 KiB/s minimum average rate.
- With a PathCache (now the default), file-cache misses — including all large files — pay one `EvalSymlinks` per request, and every disk read or listing one `os.Root` open (~12 µs on the test machine).
- HEAD and 304 on a large file open it (without reading it), so an unreadable large file gets 403 for HEAD as for GET (was 200).
- A large file replaced between the `Lstat` and the open gets 503 for that one request; a path swapped for a symlink gets 403.
- With `directory_listing` on, a directory URL always gets the listing, even when its `index.html` is cached or preloaded.
- `max_serve_file_size` applies to every file (413); files above it are never cached or preloaded.
- Wildcard CORS sends `Access-Control-Allow-Origin: *` on every response, not only to requests with an `Origin`.
- Preload keeps one cache entry per index file (no extra entry under the directory URL).
- Cache accounting counts allocated capacity, so slightly fewer entries fit in the same `max_bytes`.
- Cache disabled: br/zstd served only from sidecars; otherwise the compress middleware gzips.
- Preload pre-compresses exactly the types `compress.IsCompressible` accepts (previously a broader prefix list).
- Access-log lines are buffered (≤ 500 ms / 64 KiB) and flushed on graceful shutdown; up to that much can be lost on SIGKILL/crash.
- No `Server` header anywhere; security headers (and HSTS on TLS) on fasthttp's own 400/408/431 error responses. The HTTP→HTTPS redirect listener's 301/400 and fasthttp's per-IP-limit 429 (written before any handler runs) still carry no security headers.
- 413 body no longer discloses `max_serve_file_size`.
- HEAD with any `Range` gets 200 (unsatisfiable ranges on HEAD used to get 416).
- A disk miss (every request with the cache off, the first with it on) answers 304 when validators match.

## Deferred / decisions for the maintainer

- **`os.Root`** — *done* (AR-1). The objection recorded here earlier (it rejects absolute symlinks pointing inside the root) applies only to unresolved paths: the server opens the `EvalSymlinks` result, so such links were already resolved during validation. **Residual:** `os.Root` follows symlinks that stay inside the root, so a parent directory swapped mid-request for a link to another in-root directory (a dot-directory, say) is still followed. Closing that needs `O_NOFOLLOW` on every component (`openat2(RESOLVE_NO_SYMLINKS)` on Linux, `O_NOFOLLOW_ANY` on macOS), which is platform-specific.
- **Strong large-file ETag collisions** (P3): `"size-mtime"` is the same for a same-size rewrite with a preserved or coarse mtime (`cp -p`, `rsync -t`, pinned build timestamps), so If-Range could splice. nginx has the same limitation; adding inode or ctime would make ETags differ per replica behind a load balancer.
- **A path swapped for a symlink at the moment of a small-file read** is refused but answered 500 ("not a regular file"), not 403. No content is served.
- **Windows junctions** (unconfirmed): since Go 1.23 `EvalSymlinks` no longer resolves junctions, so a junction as the last component could pass validation; with listing on, an outside directory might be listed. Not reproducible here (`GOOS=windows go vet` only).
- **Untested details:** the 500 ms access-log flush interval (P-F10) has no test that a longer interval would fail; `FileHandler.ValidateSidecarPath`/`LoadSidecar` have no production caller (serving uses `cache.LoadSidecar`, which is tested).
- **G3** (lock-free read index in front of both LRUs, −76% parallel hit latency in the prototype): changes LRU recency semantics; breaks `TestPathCache_LookupPromotesEntry` as written.
- **P-F11** (two `string(ctx.Path())` allocations per hit): skipped — about 2% per hit, and the `unsafe` aliasing it needs would let `cache.Put` retain request bytes once `CacheKeyForPath` returns its input unchanged.
- **Case-variant cache aliases** (`/APP.JS` vs `/app.js` on case-insensitive filesystems): bounded by the LRU; canonicalising case is a platform-specific design change.
- **sendfile with `max_conns_per_ip > 0`**: fasthttp's per-IP connection wrapper hides `ReadFrom`, so large files fall back to buffered copies. Upstream limitation.
- **TLS large-file writes** go through fasthttp's 4 KiB bufio chunks.
- **Operations / concurrency / supply-chain review** (race under SIGHUP reload, `govulncheck`, workflows, goreleaser) was not performed this round.
