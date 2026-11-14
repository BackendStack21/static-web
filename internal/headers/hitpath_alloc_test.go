package headers_test

import (
	"math/rand"
	"path"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/headers"
	"github.com/valyala/fasthttp"
)

// P-F3: AddVary, SetCacheHeaders and CacheKeyForPath run on every cache hit
// and must not allocate in the common cases.

func TestAddVary_TokenAlreadyPresentDoesNotAllocate(t *testing.T) {
	var h fasthttp.ResponseHeader
	allocs := testing.AllocsPerRun(200, func() {
		h.Reset()
		h.Set("Vary", "Origin, Accept-Encoding")
		headers.AddVary(&h, "Accept-Encoding")
	})
	if allocs != 0 {
		t.Errorf("AddVary with the token already present: %v allocs/op, want 0", allocs)
	}
	if got := string(h.Peek("Vary")); got != "Origin, Accept-Encoding" {
		t.Errorf("Vary = %q, want unchanged", got)
	}
}

func TestAddVary_AppendToSingleLineDoesNotAllocate(t *testing.T) {
	var h fasthttp.ResponseHeader
	allocs := testing.AllocsPerRun(200, func() {
		h.Reset()
		h.Set("Vary", "Origin")
		headers.AddVary(&h, "Accept-Encoding")
	})
	if allocs != 0 {
		t.Errorf("AddVary appending a token: %v allocs/op, want 0", allocs)
	}
	if got := string(h.Peek("Vary")); got != "Origin, Accept-Encoding" {
		t.Errorf("Vary = %q, want %q", got, "Origin, Accept-Encoding")
	}
}

func TestSetCacheHeaders_CachedHitDoesNotAllocate(t *testing.T) {
	f := benchCachedFile()
	cfg := &config.HeadersConfig{EnableETags: true, StaticMaxAge: 3600}
	var ctx fasthttp.RequestCtx
	for _, preset := range []string{"", "Accept-Encoding", "Origin"} {
		allocs := testing.AllocsPerRun(200, func() {
			ctx.Response.Header.Reset()
			if preset != "" {
				ctx.Response.Header.Set("Vary", preset)
			}
			headers.SetCacheHeaders(&ctx, "/app.js", f, cfg)
		})
		if allocs != 0 {
			t.Errorf("SetCacheHeaders with Vary preset %q: %v allocs/op, want 0", preset, allocs)
		}
	}
}

func TestCacheKeyForPath_CleanPathDoesNotAllocate(t *testing.T) {
	for _, p := range []string{"/app.js", "/assets/js/app.min.js", "/a/b.c/d"} {
		allocs := testing.AllocsPerRun(200, func() {
			_ = headers.CacheKeyForPath(p, "index.html")
		})
		if allocs != 0 {
			t.Errorf("CacheKeyForPath(%q): %v allocs/op, want 0", p, allocs)
		}
	}
}

// ---------------------------------------------------------------------------
// Semantics: the fast paths must give exactly the pre-2026-11 results.
// ---------------------------------------------------------------------------

// refAddVary is AddVary as of v1.7.0 (before the allocation fast paths).
func refAddVary(h *fasthttp.ResponseHeader, token string) {
	existing := h.PeekAll("Vary")
	if len(existing) == 0 {
		h.Set("Vary", token)
		return
	}
	var tokens []string
	found := false
	for _, v := range existing {
		for _, t := range strings.Split(string(v), ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if t == "*" {
				return
			}
			if strings.EqualFold(t, token) {
				if found {
					continue
				}
				found = true
			}
			dup := false
			for _, e := range tokens {
				if strings.EqualFold(e, t) {
					dup = true
					break
				}
			}
			if !dup {
				tokens = append(tokens, t)
			}
		}
	}
	if !found {
		tokens = append(tokens, token)
	}
	h.Set("Vary", strings.Join(tokens, ", "))
}

// refCacheKeyForPath is CacheKeyForPath as of v1.7.0.
func refCacheKeyForPath(urlPath, indexFile string) string {
	if indexFile == "" {
		indexFile = "index.html"
	}
	isDir := urlPath == "" || urlPath == "/" || strings.HasSuffix(urlPath, "/")
	urlPath = path.Clean("/" + urlPath)
	if urlPath == "/" {
		return "/" + indexFile
	}
	if isDir {
		return urlPath + "/" + indexFile
	}
	return urlPath
}

func varyLines(h *fasthttp.ResponseHeader) []string {
	var out []string
	for _, v := range h.PeekAll("Vary") {
		out = append(out, string(v))
	}
	return out
}

func TestAddVary_MatchesReferenceImplementation(t *testing.T) {
	pieces := []string{"Accept-Encoding", "accept-encoding", "Origin", "ORIGIN", "Cookie", "*", "", " ", "\t", " ", "X-Long-Header-Name-For-Testing"}
	seps := []string{",", ", ", " ,", ",  ", "\t,"}
	rng := rand.New(rand.NewSource(1))
	randLine := func() string {
		var b strings.Builder
		n := 1 + rng.Intn(4)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(seps[rng.Intn(len(seps))])
			}
			if rng.Intn(6) == 0 {
				b.WriteString(" ")
			}
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		return b.String()
	}
	tokens := []string{"Accept-Encoding", "Origin", "accept-encoding", "Cookie", "*"}

	cases := [][]string{
		{"Accept-Encoding"}, {"Origin"}, {"Origin, Accept-Encoding"}, {"Origin,Accept-Encoding"},
		{"Accept-Encoding, accept-encoding"}, {"*"}, {"Origin, *"}, {""}, {" Origin"}, {"Origin "},
		{"Origin,  Cookie"}, {"Origin", "Accept-Encoding"}, {"Origin", "Origin"},
	}
	for i := 0; i < 5000; i++ {
		lines := []string{randLine()}
		if rng.Intn(5) == 0 {
			lines = append(lines, randLine())
		}
		cases = append(cases, lines)
	}

	for _, lines := range cases {
		for _, tok := range tokens {
			var got, want fasthttp.ResponseHeader
			for _, l := range lines {
				got.Add("Vary", l)
				want.Add("Vary", l)
			}
			headers.AddVary(&got, tok)
			refAddVary(&want, tok)
			g, w := varyLines(&got), varyLines(&want)
			if strings.Join(g, "\n") != strings.Join(w, "\n") || len(g) != len(w) {
				t.Fatalf("AddVary(%q, %q) = %q, reference gives %q", lines, tok, g, w)
			}
		}
	}
}

func TestSetCacheHeaders_VaryMatchesReference(t *testing.T) {
	cfg := &config.HeadersConfig{EnableETags: true, StaticMaxAge: 3600}
	for _, vary := range []string{"Accept-Encoding", " Accept-Encoding ", "Accept-Encoding, Origin", "Accept-Encoding,,Origin", ",", "Origin,"} {
		for _, preset := range []string{"", "Accept-Encoding", "Origin", "*"} {
			f := benchCachedFile()
			f.VaryHeader = vary
			var got, want fasthttp.RequestCtx
			if preset != "" {
				got.Response.Header.Set("Vary", preset)
				want.Response.Header.Set("Vary", preset)
			}
			headers.SetCacheHeaders(&got, "/app.js", f, cfg)
			for _, tok := range strings.Split(vary, ",") {
				if tok = strings.TrimSpace(tok); tok != "" {
					refAddVary(&want.Response.Header, tok)
				}
			}
			g, w := varyLines(&got.Response.Header), varyLines(&want.Response.Header)
			if strings.Join(g, "\n") != strings.Join(w, "\n") {
				t.Errorf("VaryHeader %q with preset %q: Vary = %q, reference gives %q", vary, preset, g, w)
			}
		}
	}
}

func TestCacheKeyForPath_MatchesReferenceImplementation(t *testing.T) {
	parts := []string{"/", "//", "a", "b.c", ".", "..", ".x", "x.", "...", "/.", "./", "%2e", "\\"}
	rng := rand.New(rand.NewSource(2))
	paths := []string{"", "/", "a", "/a", "/a/", "/.a", "/a.", "/a/./b", "/a/../b", "//a", "/a//b", "/a/b/", "/.."}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		if rng.Intn(4) != 0 {
			b.WriteString("/")
		}
		n := rng.Intn(6)
		for j := 0; j < n; j++ {
			b.WriteString(parts[rng.Intn(len(parts))])
		}
		paths = append(paths, b.String())
	}
	for _, p := range paths {
		for _, idx := range []string{"index.html", "", "home.htm"} {
			if got, want := headers.CacheKeyForPath(p, idx), refCacheKeyForPath(p, idx); got != want {
				t.Fatalf("CacheKeyForPath(%q, %q) = %q, reference gives %q", p, idx, got, want)
			}
		}
	}
}
