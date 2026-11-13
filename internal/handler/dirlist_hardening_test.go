package handler_test

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

func dlhWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dlhSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

func dlhConfig(root string) *config.Config {
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Cache.MaxBytes = 10 << 20
	cfg.Cache.MaxFileSize = 1 << 20
	cfg.Security.BlockDotfiles = true
	cfg.Security.DirectoryListing = true
	return cfg
}

func dlhGetListing(t *testing.T, cfg *config.Config, uri string) *fasthttp.RequestCtx {
	t.Helper()
	h := handler.BuildHandlerQuiet(cfg, nil)
	ctx := newTestCtx("GET", uri)
	h(ctx)
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("GET %s: status %d, want 200; body %q", uri, got, ctx.Response.Body())
	}
	return ctx
}

// dlhRow returns the size cell of the row whose link text is label, and
// whether the row exists.
func dlhRow(body, href, label string) (size string, ok bool) {
	re := regexp.MustCompile(`<a href="` + regexp.QuoteMeta(href) + `">` + regexp.QuoteMeta(label) +
		`</a>\s*</td>\s*<td class="size">([^<]*)</td>`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// FS-5: names are path-escaped in hrefs, so "#" and "?" do not turn into a
// fragment or a query.
func TestDirListing_HrefsArePathEscaped(t *testing.T) {
	root := realTempDir(t)
	for _, name := range []string{"a#b.txt", "a?b.txt", "100%.txt", "sp ace.txt", "x#y/inner.txt"} {
		dlhWrite(t, filepath.Join(root, "d", name), "x")
	}
	cfg := dlhConfig(root)

	body := string(dlhGetListing(t, cfg, "/d/").Response.Body())
	for _, want := range []string{
		`href="/d/a%23b.txt"`,
		`href="/d/a%3Fb.txt"`,
		`href="/d/100%25.txt"`,
		`href="/d/sp%20ace.txt"`,
		`href="/d/x%23y/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("listing of /d/ lacks %s", want)
		}
	}
	for _, bad := range []string{`href="/d/a#b.txt"`, `href="/d/a?b.txt"`, `href="/d/x#y/"`} {
		if strings.Contains(body, bad) {
			t.Errorf("listing of /d/ contains unescaped %s", bad)
		}
	}

	// Breadcrumb and parent links of a directory whose name needs escaping.
	dlhWrite(t, filepath.Join(root, "d", "x#y", "sub", "f.txt"), "x")
	body = string(dlhGetListing(t, cfg, "/d/x%23y/sub/").Response.Body())
	for _, want := range []string{`href="/d/x%23y/"`, `href="/d/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("listing of /d/x#y/sub/ lacks breadcrumb/parent link %s", want)
		}
	}
	if strings.Contains(body, `href="/d/x#y/"`) {
		t.Error("listing of /d/x#y/sub/ has an unescaped breadcrumb/parent link")
	}
}

// html/template escaping must still neutralise markup in file names.
func TestDirListing_MarkupInNamesIsEscaped(t *testing.T) {
	root := realTempDir(t)
	name := `"><img src=x onerror=alert(1)>#.txt` // no "/" in a file name
	dlhWrite(t, filepath.Join(root, name), "x")

	body := string(dlhGetListing(t, dlhConfig(root), "/").Response.Body())
	if strings.Contains(body, "<img") {
		t.Fatalf("listing contains raw <img markup: %s", body)
	}
	if !strings.Contains(body, "&#34;&gt;&lt;img src=x onerror=alert(1)&gt;#.txt</a>") {
		t.Errorf("file name text is not HTML-escaped: %s", body)
	}
	if !strings.Contains(strings.ToUpper(body), strings.ToUpper(`href="/%22%3E%3Cimg%20src=x%20onerror=alert%281%29%3E%23.txt"`)) {
		t.Errorf("href is not percent-encoded: %s", body)
	}
}

// FS-5: symlink entries are described by their target (type, size) and are
// hidden when the target escapes the root, is a dotfile (BlockDotfiles) or
// does not exist.
func TestDirListing_SymlinkEntriesUseTargetAndHideUnsafeOnes(t *testing.T) {
	root := realTempDir(t)
	outside := realTempDir(t)
	dlhWrite(t, filepath.Join(outside, "secret.txt"), "TOP SECRET")
	dlhWrite(t, filepath.Join(outside, "dir", "f.txt"), "x")
	dlhWrite(t, filepath.Join(root, "realdir", "f.txt"), "x")
	dlhWrite(t, filepath.Join(root, "real.txt"), strings.Repeat("r", 2048))
	dlhWrite(t, filepath.Join(root, ".env"), "DB_PASSWORD=x")

	dlhSymlink(t, "realdir", filepath.Join(root, "dirlink-in"))
	dlhSymlink(t, "real.txt", filepath.Join(root, "filelink-in"))
	dlhSymlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "link-out"))
	dlhSymlink(t, filepath.Join(outside, "dir"), filepath.Join(root, "dirlink-out"))
	dlhSymlink(t, ".env", filepath.Join(root, "dotlink"))
	dlhSymlink(t, "missing.txt", filepath.Join(root, "dangling"))

	body := string(dlhGetListing(t, dlhConfig(root), "/").Response.Body())

	if size, ok := dlhRow(body, "/dirlink-in/", "dirlink-in/"); !ok {
		t.Errorf("in-root directory symlink is not listed as a directory with a trailing slash")
	} else if size != "&mdash;" && size != "—" {
		t.Errorf("in-root directory symlink size cell = %q, want an em dash", size)
	}
	if size, ok := dlhRow(body, "/filelink-in", "filelink-in"); !ok {
		t.Errorf("in-root file symlink is not listed")
	} else if size != "2.0 KB" {
		t.Errorf("in-root file symlink size = %q, want the target's 2.0 KB", size)
	}
	for _, hidden := range []string{"link-out", "dirlink-out", "dotlink", "dangling"} {
		if strings.Contains(body, ">"+hidden) {
			t.Errorf("symlink %q is listed, want it hidden", hidden)
		}
	}

	// With BlockDotfiles off, a link to an in-root dotfile is listed again.
	cfg := dlhConfig(root)
	cfg.Security.BlockDotfiles = false
	body = string(dlhGetListing(t, cfg, "/").Response.Body())
	if _, ok := dlhRow(body, "/dotlink", "dotlink"); !ok {
		t.Error("with BlockDotfiles=false, a symlink to an in-root dotfile should be listed")
	}
	if strings.Contains(body, ">link-out") {
		t.Error("with BlockDotfiles=false, a symlink escaping the root must still be hidden")
	}
}

// HTTP-F4: a directory that cannot be read answers 403 without losing the
// security headers set by the security middleware.
func TestDirListing_UnreadableDirKeepsSecurityHeaders(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 directory")
	}
	root, cfg := setupDirListingRoot(t)
	cfg.Security.CSP = "default-src 'self'"
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	ctx := newTestCtx("GET", "/locked/")
	h(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "SAMEORIGIN",
	} {
		if got := string(ctx.Response.Header.Peek(k)); got != want {
			t.Errorf("%s = %q on 403, want %q", k, got, want)
		}
	}
}

// dlhStyleHashSource returns the CSP hash source of the body's inline <style>.
func dlhStyleHashSource(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "<style>")
	if !ok {
		t.Fatalf("no <style> in body: %s", body)
	}
	css, _, ok := strings.Cut(rest, "</style>")
	if !ok {
		t.Fatalf("unterminated <style> in body: %s", body)
	}
	sum := sha256.Sum256([]byte(css))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// HTTP-F7: the default CSP ("default-src 'self'") blocked the listing's own
// inline <style>. The listing response must carry the configured policy with
// the hash of exactly the served style bytes added, and nothing loosened.
func TestDirListing_CSPAllowsServedInlineStyleByHash(t *testing.T) {
	root := realTempDir(t)
	dlhWrite(t, filepath.Join(root, "a.txt"), "x")
	cfg := dlhConfig(root)
	cfg.Security.CSP = "default-src 'self'; img-src https://img.example"

	ctx := dlhGetListing(t, cfg, "/")
	src := dlhStyleHashSource(t, string(ctx.Response.Body()))
	want := "default-src 'self'; img-src https://img.example; style-src 'self' " + src
	if got := ctx.Response.Header.PeekAll("Content-Security-Policy"); len(got) != 1 || string(got[0]) != want {
		t.Errorf("listing Content-Security-Policy = %q, want exactly [%q]", got, want)
	}

	// HEAD carries the same policy.
	h := handler.BuildHandlerQuiet(cfg, nil)
	head := newTestCtx("HEAD", "/")
	h(head)
	if got := string(head.Response.Header.Peek("Content-Security-Policy")); got != want {
		t.Errorf("HEAD listing Content-Security-Policy = %q, want %q", got, want)
	}

	// Files keep the configured policy unchanged.
	file := newTestCtx("GET", "/a.txt")
	h(file)
	if got := string(file.Response.Header.Peek("Content-Security-Policy")); got != cfg.Security.CSP {
		t.Errorf("file Content-Security-Policy = %q, want the configured %q", got, cfg.Security.CSP)
	}
}

func TestDirListing_EmptyCSPStaysEmpty(t *testing.T) {
	root := realTempDir(t)
	dlhWrite(t, filepath.Join(root, "a.txt"), "x")
	cfg := dlhConfig(root)
	cfg.Security.CSP = ""

	ctx := dlhGetListing(t, cfg, "/")
	if got := ctx.Response.Header.PeekAll("Content-Security-Policy"); len(got) != 0 {
		t.Errorf("listing Content-Security-Policy = %q, want none when CSP is not configured", got)
	}
}
