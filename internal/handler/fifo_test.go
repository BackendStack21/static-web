//go:build unix

package handler_test

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
}

// getWithoutBlocking runs GET uri (with Accept-Encoding ae when non-empty)
// and returns the finished request. If the handler is still blocked after 2s
// it fails, after unblocking the handler by repeatedly opening the fifos
// (O_RDWR on a FIFO never blocks) until it returns.
func getWithoutBlocking(t *testing.T, h fasthttp.RequestHandler, uri, ae string, fifos ...string) *fasthttp.RequestCtx {
	t.Helper()
	ctx := newTestCtx("GET", uri)
	if ae != "" {
		ctx.Request.Header.Set("Accept-Encoding", ae)
	}
	done := make(chan struct{})
	go func() {
		h(ctx)
		close(done)
	}()
	select {
	case <-done:
		return ctx
	case <-time.After(2 * time.Second):
		for {
			for _, fifo := range fifos {
				if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
					f.Close()
				}
			}
			select {
			case <-done:
				t.Fatalf("GET %s (Accept-Encoding %q): handler blocked on a FIFO", uri, ae)
				return nil
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

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
		mkfifo(t, p)
	}

	for uri, fifo := range fifos {
		for _, size := range []int64{1 << 20, 0} { // in-cache and large-file paths
			cfg.Cache.MaxFileSize = size
			h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
			ctx := getWithoutBlocking(t, h, uri, "", fifo)
			if sc := ctx.Response.StatusCode(); sc != fasthttp.StatusForbidden {
				t.Errorf("GET %s (max_file_size %d): status = %d, want 403", uri, size, sc)
			}
		}
	}
}

// FS-4 sibling: a precompressed sidecar that is a FIFO must be skipped, not
// opened; the request falls back to another encoding.
func TestNonRegularFile_FIFOSidecarSkippedWithoutBlocking(t *testing.T) {
	root, cfg := setupTestDir(t)
	js := bytes.Repeat([]byte("var a=1;"), 50)
	if err := os.WriteFile(filepath.Join(root, "big.js"), js, 0o644); err != nil {
		t.Fatal(err)
	}
	var fifos []string
	for _, ext := range []string{".br", ".gz", ".zst"} {
		p := filepath.Join(root, "big.js"+ext)
		mkfifo(t, p)
		fifos = append(fifos, p)
	}

	for _, ae := range []string{"br", "gzip", "zstd"} {
		h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
		ctx := getWithoutBlocking(t, h, "/big.js", ae, fifos...)
		if sc := ctx.Response.StatusCode(); sc != fasthttp.StatusOK {
			t.Errorf("Accept-Encoding %q: status = %d, want 200", ae, sc)
		}
		if ae == "br" {
			if enc := string(ctx.Response.Header.Peek("Content-Encoding")); enc == "br" {
				t.Errorf("Content-Encoding = br, but the only br sidecar is a FIFO")
			}
		}
	}
}
