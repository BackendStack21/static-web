//go:build unix

package handler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// Directory listings show directories and regular files only. FIFOs,
// sockets and devices (directly, or as a symlink's target) were listed with
// links that can only be refused.
func TestDirListing_HidesNonRegularEntries(t *testing.T) {
	root := t.TempDir()
	dlhWrite(t, filepath.Join(root, "d", "file.txt"), "x")
	if err := os.Mkdir(filepath.Join(root, "d", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, filepath.Join(root, "d", "pipe"))
	dlhSymlink(t, "pipe", filepath.Join(root, "d", "pipe-link"))

	ctx := dlhGetListing(t, dlhConfig(root), "/d/")
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	body := string(ctx.Response.Body())
	for _, want := range []string{`href="/d/file.txt"`, `href="/d/sub/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("listing lacks %s", want)
		}
	}
	for _, hidden := range []string{"/d/pipe", "/d/pipe-link"} {
		if strings.Contains(body, `href="`+hidden+`"`) {
			t.Errorf("listing shows the non-regular entry %s", hidden)
		}
	}
}
