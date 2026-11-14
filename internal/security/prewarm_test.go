package security_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/security"
)

// FS-2: PreWarm with a relative root (the default files.root is ./public)
// must store absolute, symlink-resolved paths, not cwd-relative ones.
func TestPathCache_PreWarmRelativeRootStoresAbsolutePaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(root))

	pc := security.NewPathCache(10)
	pc.PreWarm([]string{"/a.txt"}, "./"+filepath.Base(root), true)

	got, ok := pc.Lookup("/a.txt")
	if !ok {
		t.Fatal("/a.txt was not pre-warmed")
	}
	if want := filepath.Join(root, "a.txt"); got != want {
		t.Errorf("pre-warmed path = %q, want %q", got, want)
	}
}
