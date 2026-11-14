package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
)

// FS-2: files.root (default ./public) is resolved to an absolute,
// symlink-free path once at startup, so preload, the path cache and the
// handler chain all agree on it whatever the working directory.
func TestResolveFilesRoot_RelativeRootBecomesAbsoluteAndResolved(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "public")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	cfg := &config.Config{}
	cfg.Files.Root = "./public"
	if err := resolveFilesRoot(cfg); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "real"); cfg.Files.Root != want {
		t.Errorf("root = %q, want %q", cfg.Files.Root, want)
	}

	// A root that does not exist yet is made absolute, not rejected: the
	// server falls back to its embedded default pages.
	cfg.Files.Root = "./missing"
	if err := resolveFilesRoot(cfg); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "missing"); cfg.Files.Root != want {
		t.Errorf("missing root = %q, want %q", cfg.Files.Root, want)
	}
}

// Files are opened through an os.Root, which opens the root directory for
// reading: a root the server may only traverse would 403 every request, so
// startup fails with a clear error instead.
func TestResolveFilesRoot_UnreadableRootFailsFast(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permission bits enforced for the test user")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	cfg := &config.Config{}
	cfg.Files.Root = root
	err = resolveFilesRoot(cfg)
	if err == nil || !strings.Contains(err.Error(), "must be readable") {
		t.Fatalf("resolveFilesRoot on a search-only root: err = %v, want a readability error", err)
	}
}
