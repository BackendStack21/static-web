package cache

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// orSetup builds a resolved root and a resolved directory outside it:
//
//	root/in.txt        "IN"
//	root/sub/a.txt     "SUB"
//	outside/secret.txt "SECRET"
//	outside/a.txt      "OUTSIDE-A"
func orSetup(t *testing.T) (root, outside string) {
	t.Helper()
	var err error
	if root, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if outside, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for p, s := range map[string]string{
		filepath.Join(root, "in.txt"):        "IN",
		filepath.Join(root, "sub", "a.txt"):  "SUB",
		filepath.Join(outside, "secret.txt"): "SECRET",
		filepath.Join(outside, "a.txt"):      "OUTSIDE-A",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

func orSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func orRead(t *testing.T, f *os.File) string {
	t.Helper()
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenInRoot_RegularFileAndRoot(t *testing.T) {
	root, _ := orSetup(t)
	f, err := OpenInRoot(root, filepath.Join(root, "sub", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := orRead(t, f); got != "SUB" {
		t.Errorf("read %q, want SUB", got)
	}
	d, err := OpenInRoot(root, root)
	if err != nil {
		t.Fatalf("opening the root itself: %v", err)
	}
	d.Close()
}

// A path validated before one of its components was swapped for a symlink
// out of the root must not be opened. Each case stands for a swap that
// happened after EvalSymlinks returned the path.
func TestOpenInRoot_RefusesComponentsLeadingOutside(t *testing.T) {
	root, outside := orSetup(t)

	// Parent directory replaced by a symlink to the outside directory.
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	orSymlink(t, outside, filepath.Join(root, "sub"))
	// Final component replaced by an absolute symlink out of the root.
	orSymlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "abs.txt"))
	// Final component replaced by a relative symlink out of the root.
	rel, err := filepath.Rel(root, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	orSymlink(t, rel, filepath.Join(root, "rel.txt"))

	for _, p := range []string{
		filepath.Join(root, "sub", "a.txt"),
		filepath.Join(root, "abs.txt"),
		filepath.Join(root, "rel.txt"),
	} {
		f, err := OpenInRoot(root, p)
		if err == nil {
			t.Errorf("%s: opened %q, want an escape error", p, orRead(t, f))
			continue
		}
		if !IsEscape(err) {
			t.Errorf("%s: err = %v, want IsEscape", p, err)
		}
	}
}

// Callers pass symlink-resolved paths, so an absolute symlink that points
// inside the root is resolved before OpenInRoot sees it. Handed the link
// itself, os.Root refuses it (absolute links are never followed).
func TestOpenInRoot_AbsoluteInRootLinkIsRefusedUnresolved(t *testing.T) {
	root, _ := orSetup(t)
	link := filepath.Join(root, "abs-in.txt")
	orSymlink(t, filepath.Join(root, "in.txt"), link)

	if _, err := OpenInRoot(root, link); !IsEscape(err) {
		t.Errorf("unresolved absolute in-root link: err = %v, want IsEscape", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	f, err := OpenInRoot(root, resolved)
	if err != nil {
		t.Fatalf("resolved path: %v", err)
	}
	if got := orRead(t, f); got != "IN" {
		t.Errorf("read %q, want IN", got)
	}
}

func TestOpenInRoot_PathOutsideRoot(t *testing.T) {
	root, outside := orSetup(t)
	_, err := OpenInRoot(root, filepath.Join(outside, "secret.txt"))
	if !errors.Is(err, ErrNotInRoot) || !IsEscape(err) {
		t.Errorf("err = %v, want ErrNotInRoot", err)
	}
}

func TestOpenInRoot_MissingIsNotExistNotEscape(t *testing.T) {
	root, _ := orSetup(t)
	_, err := OpenInRoot(root, filepath.Join(root, "nope.txt"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want ErrNotExist", err)
	}
	if IsEscape(err) {
		t.Errorf("IsEscape(%v) = true for a missing file", err)
	}
}

// TestIsEscape pins the text of os.Root's unexported escape error that
// IsEscape matches.
func TestIsEscape(t *testing.T) {
	root, _ := orSetup(t)
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = r.Open("../x")
	if err == nil || !IsEscape(err) {
		t.Fatalf("os.Root escape error %v not recognised by IsEscape", err)
	}
	if IsEscape(nil) || IsEscape(errors.New("path escapes from parent")) {
		t.Error("IsEscape matched an error that is not a *fs.PathError from os.Root")
	}
}
