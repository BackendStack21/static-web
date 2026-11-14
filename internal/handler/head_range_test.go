package handler_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/handler"
	"github.com/valyala/fasthttp"
)

// loHeadRanges are Range requests that a GET answers with 206 (or 416 for
// the unsatisfiable one). Each entry is a list of header name/value pairs;
// lm is the resource's Last-Modified, which satisfies If-Range.
func loHeadRanges(lm string) map[string][]string {
	return map[string][]string{
		"single range":                 {"Range", "bytes=0-99"},
		"multiple ranges":              {"Range", "bytes=0-9,20-29"},
		"suffix range":                 {"Range", "bytes=-50"},
		"unsatisfiable range":          {"Range", "bytes=999999999-"},
		"range with matching If-Range": {"Range", "bytes=0-99", "If-Range", lm},
	}
}

// loCheckHeadIgnoresRange checks, for each of loHeadRanges and each
// Accept-Encoding, that GET honours the Range header (206/416), and that a
// HEAD with it gets a 200 with exactly the headers of a plain HEAD.
// newHandler returns the handler for each request; uri is the resource.
func loCheckHeadIgnoresRange(t *testing.T, newHandler func() fasthttp.RequestHandler, uri string) {
	t.Helper()
	lm := string(irDo(newHandler(), "GET", uri).Response.Header.Peek("Last-Modified"))
	if lm == "" {
		t.Fatalf("GET %s has no Last-Modified", uri)
	}
	for name, rh := range loHeadRanges(lm) {
		for _, ae := range []string{"", "gzip"} {
			label := name + " / Accept-Encoding " + ae
			hdrs := append([]string{"Accept-Encoding", ae}, rh...)

			want := fasthttp.StatusPartialContent
			if name == "unsatisfiable range" {
				want = fasthttp.StatusRequestedRangeNotSatisfiable
			}
			if got := irDo(newHandler(), "GET", uri, hdrs...).Response.StatusCode(); got != want {
				t.Errorf("%s: GET status = %d, want %d (the Range must still apply to GET)", label, got, want)
			}

			plainStatus, plain := loWireHead(t, irDo(newHandler(), "HEAD", uri, "Accept-Encoding", ae))
			if plainStatus != fasthttp.StatusOK {
				t.Fatalf("%s: plain HEAD status = %d, want 200", label, plainStatus)
			}
			status, got := loWireHead(t, irDo(newHandler(), "HEAD", uri, hdrs...))
			if status != fasthttp.StatusOK {
				t.Errorf("%s: HEAD status = %d, want 200 (Range is defined for GET only)", label, status)
				continue
			}
			if !reflect.DeepEqual(got, plain) {
				t.Errorf("%s: HEAD headers differ from a plain HEAD:\n got %v\nwant %v", label, got, plain)
			}
		}
	}
}

// loHeadRangeFile writes a 64 KB compressible script to root.
func loHeadRangeFile(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "lo.js"), rbJS(64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
}

// RFC 9110 §14.2 defines Range for GET only; a server must ignore it for
// other methods. A HEAD with a satisfiable Range was answered with a 206
// (and with an unsatisfiable one, a 416) instead of the 200 headers a plain
// HEAD gets. These tests cover each serving path.

func TestHeadRange_CacheHitIgnoresRange(t *testing.T) {
	root, cfg := setupTestDir(t)
	loHeadRangeFile(t, root)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	// Fill the cache first, so that every request below, plain HEAD
	// included, is a hit (X-Cache: HIT).
	if got := irDo(h, "GET", "/lo.js").Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("warm-up GET status = %d", got)
	}
	loCheckHeadIgnoresRange(t, func() fasthttp.RequestHandler { return h }, "/lo.js")
}

func TestHeadRange_CacheMissIgnoresRange(t *testing.T) {
	root, cfg := setupTestDir(t)
	loHeadRangeFile(t, root)
	// A fresh cache for every request: each one is a disk miss that fills it.
	loCheckHeadIgnoresRange(t, func() fasthttp.RequestHandler {
		return handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	}, "/lo.js")
}

func TestHeadRange_CacheDisabledIgnoresRange(t *testing.T) {
	root, cfg := setupTestDir(t)
	loHeadRangeFile(t, root)
	cfg.Cache.Enabled = false
	h := handler.BuildHandlerQuiet(cfg, nil)
	loCheckHeadIgnoresRange(t, func() fasthttp.RequestHandler { return h }, "/lo.js")
}

func TestHeadRange_LargeFileIgnoresRange(t *testing.T) {
	_, cfg, _ := largeSetup(t)
	h := handler.BuildHandlerQuiet(cfg, cache.NewCache(cfg.Cache.MaxBytes))
	loCheckHeadIgnoresRange(t, func() fasthttp.RequestHandler { return h }, "/large.css")
}
