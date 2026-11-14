package handler

// Regression tests for the 2026-11-14 adversarial review: a path validated by
// a request but changed before it is opened (a check-then-open race) must
// never be followed out of the root, or to a blocked dotfile. Each test hands
// a sink a path as validation returned it, after "swapping" one of its
// components, which is what a racing deploy or attacker does in between.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

const swSecret = "OUTSIDE-ROOT-SECRET"

type swEnv struct {
	root, outside string
	h             *FileHandler
	c             *cache.Cache
}

// swSetup builds a resolved root with a small and a large file in sub/, a
// directory outside the root holding files of the same names with secret
// content, and a handler with cache.max_file_size = 64.
func swSetup(t *testing.T, cacheOn bool) *swEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for dir, body := range map[string]string{filepath.Join(root, "sub"): "inside", outside: swSecret} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		big := strings.Repeat(body+"\n", 200)
		if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(swSecret), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Files.MaxServeFileSize = 1 << 30
	cfg.Cache.Enabled = cacheOn
	cfg.Cache.MaxBytes = 1 << 20
	cfg.Cache.MaxFileSize = 64
	cfg.Security.BlockDotfiles = true
	cfg.Security.DirectoryListing = true
	cfg.Headers.EnableETags = true
	cfg.Server.WriteTimeout = 10 * time.Second
	c := cache.NewCache(cfg.Cache.MaxBytes)
	return &swEnv{root: root, outside: outside, h: NewFileHandler(cfg, c), c: c}
}

// swapDirForOutside replaces root/sub with a symlink to the outside directory.
func (e *swEnv) swapDirForOutside(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(e.root, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.outside, filepath.Join(e.root, "sub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// swapFileFor replaces root/sub/name with a symlink to target.
func (e *swEnv) swapFileFor(t *testing.T, name, target string) {
	t.Helper()
	p := filepath.Join(e.root, "sub", name)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func swCtx(method, uri string, hdr ...string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	for i := 0; i+1 < len(hdr); i += 2 {
		ctx.Request.Header.Set(hdr[i], hdr[i+1])
	}
	return ctx
}

// swBody returns the response body, draining a body stream.
func swBody(t *testing.T, ctx *fasthttp.RequestCtx) string {
	t.Helper()
	if !ctx.Response.IsBodyStream() {
		return string(ctx.Response.Body())
	}
	b, _ := io.ReadAll(ctx.Response.BodyStream())
	_ = ctx.Response.CloseBodyStream()
	return string(b)
}

// swAssertRefused fails unless the response is an error status that carries
// nothing of the secret: no body bytes and no length or validators.
func swAssertRefused(t *testing.T, what string, ctx *fasthttp.RequestCtx, wantStatus int) {
	t.Helper()
	if got := ctx.Response.StatusCode(); got != wantStatus {
		t.Errorf("%s: status = %d, want %d", what, got, wantStatus)
	}
	if body := swBody(t, ctx); strings.Contains(body, swSecret) {
		t.Errorf("%s: served the secret: %q", what, body)
	}
	for _, k := range []string{"ETag", "Last-Modified"} {
		if v := ctx.Response.Header.Peek(k); len(v) != 0 {
			t.Errorf("%s: %s = %q on a refused response", what, k, v)
		}
	}
}

func TestSwap_LargeFileParentDirToOutside(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			e := swSetup(t, true)
			absPath := filepath.Join(e.root, "sub", "big.txt") // as validated
			e.swapDirForOutside(t)
			ctx := swCtx(method, "/sub/big.txt")
			e.h.serveLargeFile(ctx, absPath, "/sub/big.txt")
			swAssertRefused(t, method+" large file via swapped parent", ctx, fasthttp.StatusForbidden)
		})
	}
}

func TestSwap_LargeFileFinalComponentToSymlink(t *testing.T) {
	for _, target := range []string{
		"outside", // absolute link out of the root
		".env",    // relative link to an in-root dotfile
	} {
		t.Run(target, func(t *testing.T) {
			e := swSetup(t, true)
			absPath := filepath.Join(e.root, "sub", "big.txt")
			link := filepath.Join(e.outside, "big.txt")
			if target == ".env" {
				link = "../.env"
			}
			e.swapFileFor(t, "big.txt", link)
			ctx := swCtx("GET", "/sub/big.txt")
			e.h.serveLargeFile(ctx, absPath, "/sub/big.txt")
			swAssertRefused(t, "large file swapped for a symlink", ctx, fasthttp.StatusForbidden)
		})
	}
}

// The small-file miss path, cache on and off: a read through a swapped
// parent is refused and nothing is cached; a final component swapped for a
// symlink goes to the changed-file fallback, which must refuse it too (it
// used to stat and stream the link's target).
func TestSwap_SmallFile(t *testing.T) {
	for _, cacheOn := range []bool{true, false} {
		name := map[bool]string{true: "cache", false: "nocache"}[cacheOn]
		t.Run(name+"/parent", func(t *testing.T) {
			e := swSetup(t, cacheOn)
			absPath := filepath.Join(e.root, "sub", "small.txt")
			info, err := os.Stat(absPath)
			if err != nil {
				t.Fatal(err)
			}
			e.swapDirForOutside(t)
			ctx := swCtx("GET", "/sub/small.txt")
			e.h.serveFromDisk(ctx, absPath, "/sub/small.txt", info, nil)
			swAssertRefused(t, "small file via swapped parent", ctx, fasthttp.StatusForbidden)
			if _, ok := e.c.Get("/sub/small.txt"); ok {
				t.Error("the outside file was cached")
			}
		})
		for _, target := range []string{"outside", ".env"} {
			t.Run(name+"/final-"+target, func(t *testing.T) {
				e := swSetup(t, cacheOn)
				absPath := filepath.Join(e.root, "sub", "small.txt")
				info, err := os.Stat(absPath)
				if err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(e.outside, "small.txt")
				if target == ".env" {
					link = "../.env"
				}
				e.swapFileFor(t, "small.txt", link)
				ctx := swCtx("GET", "/sub/small.txt")
				e.h.serveFromDisk(ctx, absPath, "/sub/small.txt", info, nil)
				if body := swBody(t, ctx); strings.Contains(body, swSecret) {
					t.Errorf("served the secret (status %d): %q", ctx.Response.StatusCode(), body)
				}
				if ctx.Response.StatusCode() == fasthttp.StatusOK {
					t.Errorf("status = 200 for a path swapped for a symlink")
				}
				if _, ok := e.c.Get("/sub/small.txt"); ok {
					t.Error("the swapped file was cached")
				}
			})
		}
	}
}

func TestSwap_DirectoryListingParentToOutside(t *testing.T) {
	e := swSetup(t, true)
	absDir := filepath.Join(e.root, "sub")
	// Replace root/sub with a link to the outside directory, which has an
	// entry the inside one lacks.
	if err := os.WriteFile(filepath.Join(e.outside, "outside-only.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.swapDirForOutside(t)
	ctx := swCtx("GET", "/sub/")
	e.h.serveDirectoryListing(ctx, absDir, "/sub/")
	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Errorf("status = %d, want 403", ctx.Response.StatusCode())
	}
	if strings.Contains(string(ctx.Response.Body()), "outside-only.txt") {
		t.Error("listed the outside directory")
	}
}

// A large file replaced (renamed over) after the request's stat is served as
// the new file, described by the handle: its own length and strong ETag, so
// an If-Range with the old ETag gets the full new file, never a 206 of new
// bytes spliced onto an old prefix.
func TestSwap_LargeFileReplacedIsDescribedByHandle(t *testing.T) {
	e := swSetup(t, true)
	big := filepath.Join(e.root, "sub", "big.txt")
	old := swCtx("GET", "/sub/big.txt")
	e.h.serveLargeFile(old, big, "/sub/big.txt")
	oldETag := string(old.Response.Header.Peek("ETag"))
	swBody(t, old)

	v2 := filepath.Join(e.root, "sub", "v2.tmp")
	content := bytes.Repeat([]byte("B"), 8192)
	if err := os.WriteFile(v2, content, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(v2, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(v2, big); err != nil {
		t.Fatal(err)
	}

	ctx := swCtx("GET", "/sub/big.txt", "Range", "bytes=100-", "If-Range", oldETag)
	e.h.serveLargeFile(ctx, big, "/sub/big.txt")
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Errorf("If-Range with the replaced file's ETag: status = %d, want 200", got)
	}
	if got := string(ctx.Response.Header.Peek("ETag")); got == oldETag {
		t.Errorf("new content served under the old ETag %s", got)
	}
	if got := ctx.Response.Header.ContentLength(); got != len(content) {
		t.Errorf("Content-Length = %d, want %d (the new file)", got, len(content))
	}
	if body := swBody(t, ctx); body != string(content) {
		t.Errorf("body is not the new file (len %d)", len(body))
	}
}

// End to end: a missing file under an in-root link to a dot-directory is
// refused at validation (403), so nothing created there later is served or
// cached under the link's URL.
func TestSwap_MissingFileUnderLinkToDotDirIsRefused(t *testing.T) {
	e := swSetup(t, true)
	if err := os.MkdirAll(filepath.Join(e.root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".hidden", filepath.Join(e.root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h := BuildHandlerQuiet(e.h.cfg, e.c, security.NewPathCache(100))
	ctx := swCtx("GET", "/link/x.txt")
	h(ctx)
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusForbidden {
		t.Errorf("missing file under a link to a dot-directory: status = %d, want 403", got)
	}
}
