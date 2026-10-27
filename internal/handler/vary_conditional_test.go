package handler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

func varyTokens(ctx *fasthttp.RequestCtx) (lines int, tokens []string) {
	for _, v := range ctx.Response.Header.PeekAll("Vary") {
		lines++
		for _, tok := range strings.Split(string(v), ",") {
			tokens = append(tokens, strings.ToLower(strings.TrimSpace(tok)))
		}
	}
	return
}

func hasToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == strings.ToLower(want) {
			return true
		}
	}
	return false
}

func writeBig(t *testing.T, root string) string {
	t.Helper()
	content := strings.Repeat("body { color: red; }\n", 200)
	if err := os.WriteFile(filepath.Join(root, "big.css"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return content
}

// B7: one merged Vary header with Origin and Accept-Encoding, no duplicates.
func TestVary_MergedSingleHeader(t *testing.T) {
	root, cfg := setupTestDir(t)
	writeBig(t, root)
	cfg.Security.CORSOrigins = []string{"https://a.example", "https://b.example"}
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	for i, label := range []string{"MISS", "HIT"} {
		ctx := newTestCtx("GET", "/big.css")
		ctx.Request.Header.Set("Origin", "https://a.example")
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		h(ctx)
		if ctx.Response.StatusCode() != 200 {
			t.Fatalf("%s (req %d): status = %d", label, i, ctx.Response.StatusCode())
		}
		lines, toks := varyTokens(ctx)
		if lines != 1 {
			t.Errorf("%s: %d Vary header lines, want exactly 1 (%q)", label, lines, ctx.Response.Header.PeekAll("Vary"))
		}
		if !hasToken(toks, "Origin") || !hasToken(toks, "Accept-Encoding") {
			t.Errorf("%s: Vary tokens = %v, want Origin and Accept-Encoding", label, toks)
		}
		seen := map[string]bool{}
		for _, tk := range toks {
			if seen[tk] {
				t.Errorf("%s: duplicate Vary token %q in %v", label, tk, toks)
			}
			seen[tk] = true
		}
	}
}

// B8: 304 responses carry the same Cache-Control, ETag and Vary as the 200.
func TestNotModified_CarriesValidatorsAndCacheHeaders(t *testing.T) {
	root, cfg := setupTestDir(t)
	writeBig(t, root)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	first := newTestCtx("GET", "/big.css")
	first.Request.Header.Set("Accept-Encoding", "gzip")
	h(first)
	etag := string(first.Response.Header.Peek("ETag"))
	lm := string(first.Response.Header.Peek("Last-Modified"))
	cc := string(first.Response.Header.Peek("Cache-Control"))
	if etag == "" || lm == "" || cc == "" {
		t.Fatalf("200 lacks ETag/Last-Modified/Cache-Control: %q %q %q", etag, lm, cc)
	}

	cases := map[string]func(*fasthttp.RequestCtx){
		"If-None-Match":     func(c *fasthttp.RequestCtx) { c.Request.Header.Set("If-None-Match", etag) },
		"If-Modified-Since": func(c *fasthttp.RequestCtx) { c.Request.Header.Set("If-Modified-Since", lm) },
	}
	for name, set := range cases {
		ctx := newTestCtx("GET", "/big.css")
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		set(ctx)
		h(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusNotModified {
			t.Errorf("%s: status = %d, want 304", name, ctx.Response.StatusCode())
			continue
		}
		if got := string(ctx.Response.Header.Peek("Cache-Control")); got != cc {
			t.Errorf("%s: Cache-Control = %q, want %q", name, got, cc)
		}
		if got := string(ctx.Response.Header.Peek("ETag")); got != etag {
			t.Errorf("%s: ETag = %q, want %q", name, got, etag)
		}
		if _, toks := varyTokens(ctx); !hasToken(toks, "Accept-Encoding") {
			t.Errorf("%s: Vary tokens = %v, want Accept-Encoding", name, toks)
		}
	}
}

// B9: HEAD reports the same Content-Length / Content-Encoding as GET.
func TestHead_MatchesGetWithGzip(t *testing.T) {
	root, cfg := setupTestDir(t)
	writeBig(t, root)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	do := func(method string) *fasthttp.RequestCtx {
		ctx := newTestCtx(method, "/big.css")
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
		h(ctx)
		return ctx
	}
	do("GET") // warm cache
	get := do("GET")
	head := do("HEAD")

	if g, hd := string(get.Response.Header.Peek("Content-Encoding")), string(head.Response.Header.Peek("Content-Encoding")); g != hd {
		t.Errorf("Content-Encoding GET=%q HEAD=%q", g, hd)
	}
	gl := get.Response.Header.ContentLength()
	if gl <= 0 {
		gl = len(get.Response.Body())
	}
	if hl := head.Response.Header.ContentLength(); hl != gl {
		t.Errorf("Content-Length GET=%d HEAD=%d", gl, hl)
	}
	if len(head.Response.Body()) != 0 {
		t.Errorf("HEAD body length = %d, want 0", len(head.Response.Body()))
	}
}

// B11: If-None-Match uses weak comparison (RFC 9110 §13.1.2).
func TestIfNoneMatch_WeakComparison(t *testing.T) {
	root, cfg := setupTestDir(t)
	writeBig(t, root)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))

	first := newTestCtx("GET", "/big.css")
	h(first)
	etag := string(first.Response.Header.Peek("ETag"))
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("ETag = %q, expected weak validator", etag)
	}
	strong := strings.TrimPrefix(etag, "W/")

	for _, inm := range []string{strong, `"nomatch", ` + etag, `"nomatch", ` + strong, "*"} {
		ctx := newTestCtx("GET", "/big.css")
		ctx.Request.Header.Set("If-None-Match", inm)
		h(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusNotModified {
			t.Errorf("If-None-Match: %s -> status %d, want 304", inm, ctx.Response.StatusCode())
		}
	}
	ctx := newTestCtx("GET", "/big.css")
	ctx.Request.Header.Set("If-None-Match", `"different"`)
	h(ctx)
	if ctx.Response.StatusCode() != 200 {
		t.Errorf("non-matching If-None-Match -> status %d, want 200", ctx.Response.StatusCode())
	}
}
