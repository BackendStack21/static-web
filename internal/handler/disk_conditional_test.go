package handler_test

import (
	"bufio"
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// loWireHead serialises the response in ctx without its body, as the server
// writes a HEAD or 304 response, and parses it back with net/http. It
// returns the status and the headers, without Date.
func loWireHead(t *testing.T, ctx *fasthttp.RequestCtx) (int, http.Header) {
	t.Helper()
	var resp fasthttp.Response
	ctx.Response.CopyTo(&resp)
	resp.SkipBody = true
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := resp.Write(bw); err != nil {
		t.Fatalf("serialise response: %v", err)
	}
	_ = bw.Flush()
	hr, err := http.ReadResponse(bufio.NewReader(&buf), &http.Request{Method: "HEAD"})
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	hr.Body.Close()
	hr.Header.Del("Date")
	return hr.StatusCode, hr.Header
}

// loCondConfigs returns two configurations sharing one root that holds the
// compressible /big.css: cache on and cache off. A specific CORS list makes
// every response carry a merged Vary: Origin, Accept-Encoding.
func loCondConfigs(t *testing.T) (on, off *config.Config) {
	t.Helper()
	root, cfg := setupTestDir(t)
	writeBig(t, root)
	cfg.Security.CORSOrigins = []string{"https://a.example", "https://b.example"}
	c := *cfg
	c.Cache.Enabled = false
	return cfg, &c
}

// loCondDo requests /big.css from an allowed origin, accepting gzip, with
// the extra header name/value pairs in hdrs.
func loCondDo(h fasthttp.RequestHandler, method string, hdrs ...string) *fasthttp.RequestCtx {
	return irDo(h, method, "/big.css", append([]string{"Origin", "https://a.example", "Accept-Encoding", "gzip"}, hdrs...)...)
}

// loCondValidators returns the ETag, Last-Modified and Cache-Control of a
// 200 response to GET /big.css.
func loCondValidators(t *testing.T, h fasthttp.RequestHandler) (etag, lm, cc string) {
	t.Helper()
	ctx := loCondDo(h, "GET")
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("GET status = %d, want 200", ctx.Response.StatusCode())
	}
	etag = string(ctx.Response.Header.Peek("ETag"))
	lm = string(ctx.Response.Header.Peek("Last-Modified"))
	cc = string(ctx.Response.Header.Peek("Cache-Control"))
	if etag == "" || lm == "" || cc == "" {
		t.Fatalf("200 lacks ETag/Last-Modified/Cache-Control: %q %q %q", etag, lm, cc)
	}
	return etag, lm, cc
}

// loAssert304 checks that ctx is a 304 carrying the validators, the merged
// Vary and the security headers, and returns its wire headers.
func loAssert304(t *testing.T, label string, ctx *fasthttp.RequestCtx, etag, lm, cc string) http.Header {
	t.Helper()
	status, hdr := loWireHead(t, ctx)
	if status != fasthttp.StatusNotModified {
		t.Errorf("%s: status = %d, want 304", label, status)
		return hdr
	}
	if n := len(ctx.Response.Body()); n != 0 {
		t.Errorf("%s: 304 holds a %d-byte body", label, n)
	}
	for k, want := range map[string]string{
		"ETag":                    etag,
		"Last-Modified":           lm,
		"Cache-Control":           cc,
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "SAMEORIGIN",
		"Content-Security-Policy": "default-src 'self'",
	} {
		if got := hdr.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", label, k, got, want)
		}
	}
	lines, toks := varyTokens(ctx)
	if lines != 1 || !hasToken(toks, "Origin") || !hasToken(toks, "Accept-Encoding") {
		t.Errorf("%s: Vary = %q, want one line with Origin and Accept-Encoding", label, ctx.Response.Header.PeekAll("Vary"))
	}
	return hdr
}

// With the file cache disabled every request is a disk miss, and
// serveFromDisk never evaluated If-None-Match or If-Modified-Since: a
// conditional request whose validators matched got a full 200. It must get
// the 304 a cache hit gets, with the same headers, for GET and HEAD alike.
func TestDiskMiss_CacheDisabled_MatchingValidatorsGet304LikeCacheHit(t *testing.T) {
	on, off := loCondConfigs(t)
	hOff := handler.BuildHandlerQuiet(off, nil)
	etag, lm, cc := loCondValidators(t, hOff)

	// The reference: the 304 answered from a file-cache hit.
	hOn := handler.BuildHandlerQuiet(on, cache.NewCache(on.Cache.MaxBytes))
	loCondDo(hOn, "GET") // fill the cache
	hit := loCondDo(hOn, "GET", "If-None-Match", etag)
	if got := string(hit.Response.Header.Peek("X-Cache")); got != "" {
		t.Fatalf("reference cache-hit 304 carries X-Cache %q", got)
	}
	want := loAssert304(t, "cache-hit 304", hit, etag, lm, cc)

	for name, cond := range map[string][]string{
		"If-None-Match":                {"If-None-Match", etag},
		"If-None-Match list":           {"If-None-Match", `"other", ` + etag},
		"If-None-Match strong form":    {"If-None-Match", strings.TrimPrefix(etag, "W/")},
		"If-Modified-Since":            {"If-Modified-Since", lm},
		"If-Modified-Since and INM ok": {"If-Modified-Since", "Mon, 01 Jan 2001 00:00:00 GMT", "If-None-Match", etag},
	} {
		get := loCondDo(hOff, "GET", cond...)
		head := loCondDo(hOff, "HEAD", cond...)
		gotGet := loAssert304(t, name+" GET", get, etag, lm, cc)
		gotHead := loAssert304(t, name+" HEAD", head, etag, lm, cc)
		if !reflect.DeepEqual(gotGet, want) {
			t.Errorf("%s: GET 304 headers differ from the cache-hit 304:\n got %v\nwant %v", name, gotGet, want)
		}
		if !reflect.DeepEqual(gotHead, gotGet) {
			t.Errorf("%s: HEAD and GET 304 headers differ:\nHEAD %v\n GET %v", name, gotHead, gotGet)
		}
	}
}

// Validators that do not match still get the full 200, for GET and HEAD
// alike. If-None-Match takes precedence over If-Modified-Since (RFC 9110
// §13.2.2), as on a cache hit.
func TestDiskMiss_CacheDisabled_NonMatchingValidatorsGet200(t *testing.T) {
	_, off := loCondConfigs(t)
	h := handler.BuildHandlerQuiet(off, nil)
	_, lm, _ := loCondValidators(t, h)
	plainGet := loCondDo(h, "GET")
	plainHead := loCondDo(h, "HEAD")
	_, plainHeadHdr := loWireHead(t, plainHead)

	for name, cond := range map[string][]string{
		"If-None-Match other":              {"If-None-Match", `"other"`},
		"If-Modified-Since earlier":        {"If-Modified-Since", "Mon, 01 Jan 2001 00:00:00 GMT"},
		"If-Modified-Since malformed":      {"If-Modified-Since", "yesterday"},
		"INM other overrides matching IMS": {"If-None-Match", `"other"`, "If-Modified-Since", lm},
		"If-None-Match list of others":     {"If-None-Match", `"a", "b"`},
	} {
		get := loCondDo(h, "GET", cond...)
		if got := get.Response.StatusCode(); got != fasthttp.StatusOK {
			t.Errorf("%s: GET status = %d, want 200", name, got)
			continue
		}
		if !bytes.Equal(get.Response.Body(), plainGet.Response.Body()) {
			t.Errorf("%s: GET body differs from an unconditional GET", name)
		}
		head := loCondDo(h, "HEAD", cond...)
		status, hdr := loWireHead(t, head)
		if status != fasthttp.StatusOK {
			t.Errorf("%s: HEAD status = %d, want 200", name, status)
			continue
		}
		if !reflect.DeepEqual(hdr, plainHeadHdr) {
			t.Errorf("%s: HEAD headers differ from an unconditional HEAD:\n got %v\nwant %v", name, hdr, plainHeadHdr)
		}
	}
}

// With the file cache enabled, the first request for a file is a disk miss
// that fills the cache. A conditional first request whose validators match
// must get a 304 too, and still leave the entry in the cache.
func TestDiskMiss_CacheEnabledFirstConditionalRequestGets304AndFillsCache(t *testing.T) {
	on, off := loCondConfigs(t)
	// Validators from a handler that shares the root but not the cache.
	etag, lm, cc := loCondValidators(t, handler.BuildHandlerQuiet(off, nil))

	for name, cond := range map[string][]string{
		"If-None-Match":     {"If-None-Match", etag},
		"If-Modified-Since": {"If-Modified-Since", lm},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			label := name + " " + method
			c := cache.NewCache(on.Cache.MaxBytes)
			h := handler.BuildHandlerQuiet(on, c)
			loAssert304(t, label+" (disk miss)", loCondDo(h, method, cond...), etag, lm, cc)
			if _, ok := c.Get("/big.css"); !ok {
				t.Errorf("%s: the disk-miss 304 left /big.css out of the cache", label)
				continue
			}
			next := loCondDo(h, "GET")
			if next.Response.StatusCode() != fasthttp.StatusOK {
				t.Errorf("%s: follow-up GET status = %d, want 200", label, next.Response.StatusCode())
			}
			if got := string(next.Response.Header.Peek("X-Cache")); got != "HIT" {
				t.Errorf("%s: follow-up GET X-Cache = %q, want HIT", label, got)
			}
		}
	}
}
