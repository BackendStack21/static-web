package handler

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// drStaleInfo is a FileInfo whose size is out of date, standing in for a
// file that changed between HandleRequest's stat and serveFromDisk's read.
type drStaleInfo struct {
	os.FileInfo
	size int64
}

func (s drStaleInfo) Size() int64 { return s.size }

func drSetup(t *testing.T, content []byte) (*FileHandler, *cache.Cache, string, os.FileInfo) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "f.txt")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Files.Root = root
	cfg.Files.Index = "index.html"
	cfg.Files.MaxServeFileSize = 1 << 30
	cfg.Cache.Enabled = true
	cfg.Cache.MaxBytes = 10 << 20
	cfg.Cache.MaxFileSize = 64
	cfg.Server.WriteTimeout = 10 * time.Second
	c := cache.NewCache(cfg.Cache.MaxBytes)
	return NewFileHandler(cfg, c), c, p, info
}

// The cached size must describe the bytes actually read, not an earlier stat.
func TestServeFromDisk_CachedSizeComesFromTheBytesRead(t *testing.T) {
	h, c, p, info := drSetup(t, bytes.Repeat([]byte("a"), 20))
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("HEAD")
	ctx.Request.SetRequestURI("/f.txt")
	h.serveFromDisk(ctx, p, "/f.txt", drStaleInfo{info, 10}, nil)

	f, ok := c.Get("/f.txt")
	if !ok {
		t.Fatal("entry not cached")
	}
	if f.Size != int64(len(f.Data)) {
		t.Errorf("cached Size=%d, len(Data)=%d, want equal", f.Size, len(f.Data))
	}
	if got := ctx.Response.Header.ContentLength(); got != 20 {
		t.Errorf("HEAD Content-Length=%d, want 20", got)
	}
}

// A file that grew past cache.max_file_size after it was stat'ed must not be
// read into the cache; it is served as a large file instead.
func TestServeFromDisk_FileThatGrewPastCacheLimitIsNotCached(t *testing.T) {
	h, c, p, info := drSetup(t, bytes.Repeat([]byte("b"), 500))
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/f.txt")
	h.serveFromDisk(ctx, p, "/f.txt", drStaleInfo{info, 10}, nil)

	if _, ok := c.Get("/f.txt"); ok {
		t.Error("a file larger than max_file_size was cached")
	}
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status %d, want 200", got)
	}
	if got := ctx.Response.Header.ContentLength(); got != 500 {
		t.Errorf("Content-Length=%d, want 500", got)
	}
}
