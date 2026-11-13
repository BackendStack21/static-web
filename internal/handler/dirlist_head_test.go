package handler_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/BackendStack21/static-web/internal/handler"
)

// HEAD of a directory listing must carry the headers GET does. HEAD used to
// return before rendering, so the compress middleware saw an empty body: GET
// came back gzipped while HEAD claimed identity with Content-Length 0.
func TestDirListing_HeadMatchesGetEncodingAndLength(t *testing.T) {
	root := realTempDir(t)
	for i := 0; i < 40; i++ {
		dlhWrite(t, filepath.Join(root, "d", fmt.Sprintf("file-%02d.txt", i)), "x")
	}
	cfg := dlhConfig(root)
	cfg.Compression.Enabled = true
	cfg.Compression.MinSize = 10
	cfg.Compression.Level = 5
	h := handler.BuildHandlerQuiet(cfg, nil)

	get := newTestCtx("GET", "/d/")
	get.Request.Header.Set("Accept-Encoding", "gzip")
	h(get)
	head := newTestCtx("HEAD", "/d/")
	head.Request.Header.Set("Accept-Encoding", "gzip")
	h(head)

	if get.Response.StatusCode() != 200 || head.Response.StatusCode() != 200 {
		t.Fatalf("status GET=%d HEAD=%d, want 200", get.Response.StatusCode(), head.Response.StatusCode())
	}
	if g := string(get.Response.Header.Peek("Content-Encoding")); g != "gzip" {
		t.Fatalf("GET Content-Encoding=%q, want gzip (test precondition)", g)
	}
	if g, hd := string(get.Response.Header.Peek("Content-Encoding")), string(head.Response.Header.Peek("Content-Encoding")); g != hd {
		t.Errorf("Content-Encoding GET=%q HEAD=%q, want equal", g, hd)
	}
	if g, hd := len(get.Response.Body()), len(head.Response.Body()); g != hd {
		t.Errorf("body length GET=%d HEAD=%d, want equal (fasthttp omits HEAD bodies on the wire but sizes Content-Length from it)", g, hd)
	}
}
