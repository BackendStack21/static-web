package main

import (
	"os"
	"path/filepath"
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
