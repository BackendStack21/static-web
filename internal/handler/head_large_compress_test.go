package handler_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/BackendStack21/static-web/internal/security"
)

// A large file is streamed as identity by GET, which the compress middleware
// never touches. HEAD carries only headers with an explicit Content-Length.
// With compression.min_size = 0 the middleware used to gzip HEAD's empty body,
// advertising Content-Encoding: gzip (next to the strong large-file ETag) for
// a representation GET never sends.
func TestHeadLargeFile_MinSizeZeroMatchesGet(t *testing.T) {
	root, cfg := setupTestDir(t)
	cfg.Cache.MaxFileSize = 64
	cfg.Compression.MinSize = 0
	if err := os.WriteFile(filepath.Join(root, "big.js"), bytes.Repeat([]byte("var a=1;\n"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	h := handler.BuildHandler(cfg, cache.NewCache(cfg.Cache.MaxBytes), security.NewPathCache(100))

	get := newTestCtx("GET", "/big.js")
	get.Request.Header.Set("Accept-Encoding", "gzip")
	h(get)
	head := newTestCtx("HEAD", "/big.js")
	head.Request.Header.Set("Accept-Encoding", "gzip")
	h(head)

	if get.Response.StatusCode() != 200 || head.Response.StatusCode() != 200 {
		t.Fatalf("status GET=%d HEAD=%d, want 200", get.Response.StatusCode(), head.Response.StatusCode())
	}
	if g, hd := string(get.Response.Header.Peek("Content-Encoding")), string(head.Response.Header.Peek("Content-Encoding")); g != hd {
		t.Errorf("Content-Encoding GET=%q HEAD=%q, want equal", g, hd)
	}
	if g, hd := get.Response.Header.ContentLength(), head.Response.Header.ContentLength(); g != hd {
		t.Errorf("Content-Length GET=%d HEAD=%d, want equal", g, hd)
	}
}
