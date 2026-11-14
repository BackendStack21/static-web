package handler

// Regression tests from the 2026-11-14 adversarial review.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With directory listing on, a directory URL gets the listing even when its
// index file is in the file cache: the answer must not depend on what has
// been requested before.
func TestDirectoryListing_NotShadowedByCachedIndex(t *testing.T) {
	e := swSetup(t, true)
	if err := os.WriteFile(filepath.Join(e.root, "sub", "index.html"), []byte("<h1>index</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	isListing := func() bool {
		ctx := swCtx("GET", "/sub/")
		e.h.HandleRequest(ctx)
		return strings.Contains(string(ctx.Response.Body()), "Index of /sub/")
	}
	if !isListing() {
		t.Fatal("cold cache: /sub/ is not the listing")
	}
	ctx := swCtx("GET", "/sub/index.html")
	e.h.HandleRequest(ctx)
	if _, ok := e.c.Get("/sub/index.html"); !ok {
		t.Fatal("index.html was not cached")
	}
	if !isListing() {
		t.Error("after /sub/index.html was cached, /sub/ served the index instead of the listing")
	}
}
