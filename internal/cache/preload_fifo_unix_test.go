//go:build unix

package cache_test

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
)

// Robustness: opening a FIFO blocks until a writer appears, so a FIFO
// sidecar must never be opened. Preload would otherwise hang startup.
func TestPreload_NeverOpensFIFOSidecar(t *testing.T) {
	root := t.TempDir()
	src := textBody("console.log(5);\n", 2000)
	writeTestFile(t, filepath.Join(root, "app.js"), src)
	fifo := filepath.Join(root, "app.js.gz")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	c := cache.NewCache(10 << 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Preload(root, variantPreloadConfig())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Unblock the stuck open so the test can exit.
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("Preload blocked on a FIFO sidecar")
	}

	f := mustCached(t, c, "/app.js")
	if !bytes.Equal(gunzipBytes(t, f.GzipData), src) {
		t.Error("GzipData should be generated: the .gz sidecar is a FIFO")
	}
}
