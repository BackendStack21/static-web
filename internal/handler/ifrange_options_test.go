package handler_test

import (
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// B10: If-Range (RFC 9110 §13.1.5).
func TestIfRange(t *testing.T) {
	root, cfg := setupTestDir(t)
	content := writeBig(t, root)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	first := newTestCtx("GET", "/big.css")
	h(first)
	etag := string(first.Response.Header.Peek("ETag"))
	lm := string(first.Response.Header.Peek("Last-Modified"))
	if etag == "" || lm == "" {
		t.Fatalf("missing validators: %q %q", etag, lm)
	}
	lmTime, err := time.Parse(cache.HTTPTimeFormat, lm)
	if err != nil {
		t.Fatalf("parse Last-Modified %q: %v", lm, err)
	}
	older := lmTime.Add(-time.Hour).UTC().Format(cache.HTTPTimeFormat)

	cases := []struct {
		name   string
		ifRng  string
		status int
	}{
		{"non-matching ETag", `"stale"`, 200},
		{"weak ETag never matches", etag, 200},
		{"exact Last-Modified", lm, 206},
		{"older date", older, 200},
		{"garbage", "not-a-validator", 200},
	}
	for _, tc := range cases {
		// twice: cold and warm cache
		for i := 0; i < 2; i++ {
			ctx := newTestCtx("GET", "/big.css")
			ctx.Request.Header.Set("Range", "bytes=0-9")
			ctx.Request.Header.Set("If-Range", tc.ifRng)
			h(ctx)
			if ctx.Response.StatusCode() != tc.status {
				t.Errorf("%s (req %d): status = %d, want %d", tc.name, i, ctx.Response.StatusCode(), tc.status)
				continue
			}
			if tc.status == 200 && string(ctx.Response.Body()) != content {
				t.Errorf("%s: body len %d, want full %d", tc.name, len(ctx.Response.Body()), len(content))
			}
			if tc.status == 206 && string(ctx.Response.Body()) != content[:10] {
				t.Errorf("%s: 206 body = %q", tc.name, ctx.Response.Body())
			}
		}
	}
}

// B12: OPTIONS without CORS and Allow on 405 (RFC 9110 §9.3.7, §15.5.6).
func TestOptionsAndAllow(t *testing.T) {
	const allow = "GET, HEAD, OPTIONS"
	for _, cors := range [][]string{nil, {"https://a.example"}, {"*"}} {
		root, cfg := setupTestDir(t)
		writeBig(t, root)
		cfg.Security.CORSOrigins = cors
		h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

		ctx := newTestCtx("OPTIONS", "/big.css")
		h(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusNoContent {
			t.Errorf("cors=%v OPTIONS status = %d, want 204", cors, ctx.Response.StatusCode())
		}
		if got := string(ctx.Response.Header.Peek("Allow")); got != allow {
			t.Errorf("cors=%v OPTIONS Allow = %q, want %q", cors, got, allow)
		}
		if len(ctx.Response.Body()) != 0 {
			t.Errorf("cors=%v OPTIONS body length = %d, want 0", cors, len(ctx.Response.Body()))
		}

		post := newTestCtx("POST", "/big.css")
		h(post)
		if post.Response.StatusCode() != fasthttp.StatusMethodNotAllowed {
			t.Errorf("cors=%v POST status = %d, want 405", cors, post.Response.StatusCode())
		}
		if got := string(post.Response.Header.Peek("Allow")); got != allow {
			t.Errorf("cors=%v POST Allow = %q, want %q", cors, got, allow)
		}
		if strings.Contains(string(post.Response.Body()), "body {") {
			t.Errorf("cors=%v POST served file content", cors)
		}
	}
}
