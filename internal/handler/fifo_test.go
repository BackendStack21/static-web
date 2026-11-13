//go:build unix

package handler_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// FS-4: a FIFO (or any non-regular file) under the root must be refused
// without opening it; opening a FIFO for reading blocks until a writer
// appears, which would pin the handler goroutine forever.
func TestNonRegularFile_FIFORefusedWithoutBlocking(t *testing.T) {
	root, cfg := setupTestDir(t)
	if err := os.MkdirAll(filepath.Join(root, "fifodir"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifos := map[string]string{
		"/pipe":      filepath.Join(root, "pipe"),
		"/pipe.html": filepath.Join(root, "pipe.html"),
		"/fifodir/":  filepath.Join(root, "fifodir", "index.html"),
		"/fifodir":   filepath.Join(root, "fifodir", "index.html"),
	}
	for _, p := range fifos {
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	}

	for uri, fifo := range fifos {
		for _, size := range []int64{1 << 20, 0} { // in-cache and large-file paths
			cfg.Cache.MaxFileSize = size
			h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
			done := make(chan int, 1)
			go func() {
				ctx := newTestCtx("GET", uri)
				h(ctx)
				done <- ctx.Response.StatusCode()
			}()
			select {
			case sc := <-done:
				if sc != fasthttp.StatusForbidden {
					t.Errorf("GET %s (max_file_size %d): status = %d, want 403", uri, size, sc)
				}
			case <-time.After(2 * time.Second):
				// Unblock the handler: O_RDWR on a FIFO never blocks.
				if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
					f.Close()
				}
				<-done
				t.Fatalf("GET %s (max_file_size %d): handler blocked on a FIFO", uri, size)
			}
		}
	}
}
