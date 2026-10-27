package cache_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
)

// B13: symlinked files must be described by their target (stat), not the link (lstat).
func TestPreload_SymlinkUsesTargetStat(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), bytes.Repeat([]byte{1}, 5000), 0644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "a.bin"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.bin", filepath.Join(root, "z.bin")); err != nil {
		t.Fatal(err)
	}

	c := cache.NewCache(10 << 20)
	c.Preload(root, cache.PreloadConfig{MaxFileSize: 1 << 20})

	for _, key := range []string{"/a.bin", "/z.bin"} {
		f, ok := c.Get(key)
		if !ok {
			if key == "/a.bin" {
				t.Fatalf("%s not cached", key)
			}
			continue // a symlink alias need not be keyed
		}
		if f.Size != 5000 || len(f.Data) != 5000 {
			t.Errorf("%s: Size=%d len(Data)=%d, want 5000", key, f.Size, len(f.Data))
		}
		if f.CLHeader != "5000" {
			t.Errorf("%s: CLHeader = %q, want \"5000\"", key, f.CLHeader)
		}
		if !f.LastModified.Equal(mtime) {
			t.Errorf("%s: LastModified = %v, want %v (target mtime)", key, f.LastModified, mtime)
		}
	}
}

func TestPreload_SymlinkToOversizedTargetSkipped(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.dat"), bytes.Repeat([]byte{2}, 5000), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("big.dat", filepath.Join(root, "link.dat")); err != nil {
		t.Fatal(err)
	}

	c := cache.NewCache(10 << 20)
	stats := c.Preload(root, cache.PreloadConfig{MaxFileSize: 1000})

	if stats.Files != 0 || stats.Bytes != 0 {
		t.Errorf("stats = %+v, want nothing loaded", stats)
	}
	for _, key := range []string{"/big.dat", "/link.dat"} {
		if _, ok := c.Get(key); ok {
			t.Errorf("%s was cached although the target exceeds MaxFileSize", key)
		}
	}
}
