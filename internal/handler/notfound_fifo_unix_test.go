//go:build unix

package handler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// The custom 404 page is read once, when the handler is built. A FIFO there
// blocked that read, and so server startup, until something opened the FIFO
// for writing. A non-regular file is now refused (the embedded 404 page is
// served instead), without opening it.
func TestCustomNotFound_FIFOIsRefusedWithoutBlocking(t *testing.T) {
	root, cfg := setupTestDir(t)
	fifo := filepath.Join(root, "missing.html")
	mkfifo(t, fifo)
	cfg.Files.NotFound = "missing.html"

	built := make(chan fasthttp.RequestHandler, 1)
	go func() { built <- handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes)) }()
	var h fasthttp.RequestHandler
	select {
	case h = <-built:
	case <-time.After(2 * time.Second):
		for { // unblock the reader so the test can finish
			if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
				f.Close()
			}
			select {
			case <-built:
				t.Fatal("building the handler blocked on a FIFO custom 404 page")
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	ctx := newTestCtx("GET", "/does-not-exist.txt")
	h(ctx)
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("status = %d, want 404", got)
	}
	if ct := string(ctx.Response.Header.Peek("Content-Type")); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want the embedded HTML 404 page", ct)
	}
}
