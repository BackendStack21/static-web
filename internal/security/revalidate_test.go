package security_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

func TestCheckLexical(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path          string
		blockDotfiles bool
		want          error
	}{
		{"/", true, nil},
		{"/a/b.js", true, nil},
		{"/a/b.c.js", true, nil},
		{"/dir/", true, nil},
		{"a/b.js", true, nil},
		{"//a//b.js", true, nil},
		{"/a/./b.js", true, nil},
		{"/../../etc/passwd", true, nil}, // cleaned to /etc/passwd under root
		{"/a/..", true, nil},
		{"/.env", true, security.ErrDotfile},
		{"/a/.git/config", true, security.ErrDotfile},
		{"/a/../.env", true, security.ErrDotfile},
		{"/.well-known/x", true, security.ErrDotfile},
		{"/.env", false, nil},
		{"/a\x00b", true, security.ErrNullByte},
		{"/a\x00b", false, security.ErrNullByte},
	}
	for _, tc := range cases {
		err := security.CheckLexical(tc.path, root, tc.blockDotfiles)
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("CheckLexical(%q, block=%v) = %v, want %v", tc.path, tc.blockDotfiles, err, tc.want)
		}
		// PathSafe must apply the same lexical verdict.
		if tc.want != nil {
			if _, err := security.PathSafe(tc.path, root, tc.blockDotfiles); !errors.Is(err, tc.want) {
				t.Errorf("PathSafe(%q, block=%v) = %v, want %v", tc.path, tc.blockDotfiles, err, tc.want)
			}
		}
	}
}

func TestCheckLexical_PlainPathDoesNotAllocate(t *testing.T) {
	root := t.TempDir()
	if n := testing.AllocsPerRun(100, func() {
		_ = security.CheckLexical("/assets/js/app.min.js", root, true)
	}); n != 0 {
		t.Errorf("CheckLexical allocated %.0f times for a plain path", n)
	}
}

// Dot-named URL components are refused lexically, even when they are
// symlinks to a directory with a plain name.
func TestPathSafe_DotNamedSymlinkIsBlocked(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "visible"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("visible", filepath.Join(root, ".hidden")); err != nil {
		t.Fatal(err)
	}
	if _, err := security.PathSafe("/.hidden/x.txt", root, true); !errors.Is(err, security.ErrDotfile) {
		t.Errorf("PathSafe(/.hidden/x.txt) = %v, want ErrDotfile", err)
	}
	if _, err := security.PathSafe("/visible/x.txt", root, true); err != nil {
		t.Errorf("PathSafe(/visible/x.txt) = %v, want nil", err)
	}
}

func TestRevalidatePath_RefreshesPathCache(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "f.txt")
	if err := os.WriteFile(file, []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := security.NewPathCache(10)

	// Valid, existing path: stored.
	if p, err := security.RevalidatePath(pc, "/f.txt", root, true); err != nil || p != file {
		t.Fatalf("RevalidatePath = %q, %v; want %q, nil", p, err, file)
	}
	if got, ok := pc.Lookup("/f.txt"); !ok || got != file {
		t.Errorf("after a valid revalidation, Lookup = %q, %v", got, ok)
	}

	// Swapped for a symlink leaving the root: refused and dropped.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	if _, err := security.RevalidatePath(pc, "/f.txt", root, true); !errors.Is(err, security.ErrPathTraversal) {
		t.Errorf("RevalidatePath after swap = %v, want ErrPathTraversal", err)
	}
	if _, ok := pc.Lookup("/f.txt"); ok {
		t.Error("entry kept after failed revalidation")
	}

	// Path that no longer exists: allowed (404 later) but not cached.
	pc.Store("/gone.txt", filepath.Join(root, "gone.txt"))
	if _, err := security.RevalidatePath(pc, "/gone.txt", root, true); err != nil {
		t.Errorf("RevalidatePath(missing) = %v, want nil", err)
	}
	if _, ok := pc.Lookup("/gone.txt"); ok {
		t.Error("entry kept for a path that no longer exists")
	}

	// A nil PathCache is allowed.
	if _, err := security.RevalidatePath(nil, "/x", root, true); err != nil {
		t.Errorf("RevalidatePath(nil cache) = %v", err)
	}
}

// The middleware hands a path to the next handler only when it validated it
// during this request; after a PathCache hit it ran only the lexical checks.
func TestMiddleware_HandsOverOnlyFreshlyValidatedPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	var handed []bool
	next := func(ctx *fasthttp.RequestCtx) {
		_, ok := security.SafePathFromCtx(ctx)
		handed = append(handed, ok)
	}
	cfg := &config.SecurityConfig{BlockDotfiles: true}

	pc := security.NewPathCache(10)
	withPC := security.Middleware(cfg, root, next, pc)
	for _, uri := range []string{"/a.txt", "/a.txt", "/missing.txt", "/missing.txt"} {
		withPC(newSecurityCtx("GET", uri))
	}
	if want := []bool{true, false, true, true}; !equalBools(handed, want) {
		t.Errorf("with PathCache: handed over = %v, want %v", handed, want)
	}

	handed = nil
	withoutPC := security.Middleware(cfg, root, next)
	withoutPC(newSecurityCtx("GET", "/a.txt"))
	withoutPC(newSecurityCtx("GET", "/a.txt"))
	if want := []bool{true, true}; !equalBools(handed, want) {
		t.Errorf("without PathCache: handed over = %v, want %v", handed, want)
	}

	// Lexical checks still apply on a PathCache hit.
	pc.Store("/.env", filepath.Join(root, ".env"))
	ctx := newSecurityCtx("GET", "/.env")
	withPC(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Errorf("dotfile with a PathCache entry: status = %d, want 403", ctx.Response.StatusCode())
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
