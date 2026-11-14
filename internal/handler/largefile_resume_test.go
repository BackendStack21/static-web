package handler_test

import (
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// HTTP-F1: browsers resume an interrupted download with Range plus If-Range
// set to the ETag they saw, and If-Range only matches a strong ETag. Every
// ETag was weak, so each resume restarted at byte 0. Large-file responses
// are never encoded, so their ETag is strong; cached entries, whose encoded
// variants share one ETag, keep weak ones.
func TestLargeFile_StrongETagLetsIfRangeResume(t *testing.T) {
	_, cfg, content := largeSetup(t)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	first := largeGet(h, "GET", map[string]string{"Accept-Encoding": "gzip"})
	etag := string(first.Response.Header.Peek("ETag"))
	if !strings.HasPrefix(etag, `"`) {
		t.Fatalf("large-file ETag = %q, want a strong ETag", etag)
	}

	resume := largeGet(h, "GET", map[string]string{
		"Accept-Encoding": "gzip",
		"Range":           "bytes=1000-",
		"If-Range":        etag,
	})
	if got := resume.Response.StatusCode(); got != fasthttp.StatusPartialContent {
		t.Fatalf("Range with If-Range = own ETag: status %d, want 206", got)
	}
	if string(resume.Response.Body()) != content[1000:] {
		t.Errorf("206 body is not bytes 1000- of the file")
	}

	// A weak form of the tag, or another tag, still gets the full 200.
	for _, ir := range []string{"W/" + etag, `"other"`} {
		ctx := largeGet(h, "GET", map[string]string{"Range": "bytes=1000-", "If-Range": ir})
		if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
			t.Errorf("If-Range %s: status %d, want 200", ir, got)
		}
	}

	// Conditional GET with the strong tag still answers 304.
	if got := largeGet(h, "GET", map[string]string{"If-None-Match": etag}).Response.StatusCode(); got != fasthttp.StatusNotModified {
		t.Errorf("If-None-Match with the strong ETag: status %d, want 304", got)
	}

	// A cached file keeps its weak ETag.
	small := newTestCtx("GET", "/app.js")
	h(small)
	if got := string(small.Response.Header.Peek("ETag")); !strings.HasPrefix(got, `W/"`) {
		t.Errorf("cached file ETag = %q, want a weak ETag", got)
	}
}
