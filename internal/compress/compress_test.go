package compress_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BackendStack21/static-web/internal/compress"
	"github.com/BackendStack21/static-web/internal/config"
)

func TestIsCompressible(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"text/css", true},
		{"application/javascript", true},
		{"application/json", true},
		{"image/svg+xml", true},
		{"image/png", false},
		{"image/jpeg", false},
		{"video/mp4", false},
		{"application/zip", false},
		{"", false},
	}

	for _, tc := range cases {
		t.Run(tc.ct, func(t *testing.T) {
			got := compress.IsCompressible(tc.ct)
			if got != tc.want {
				t.Errorf("IsCompressible(%q) = %v, want %v", tc.ct, got, tc.want)
			}
		})
	}
}

func TestAcceptsEncoding(t *testing.T) {
	makeReq := func(ae string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if ae != "" {
			r.Header.Set("Accept-Encoding", ae)
		}
		return r
	}

	if !compress.AcceptsEncoding(makeReq("gzip"), "gzip") {
		t.Error("expected gzip accepted")
	}
	if !compress.AcceptsEncoding(makeReq("gzip, br, zstd"), "br") {
		t.Error("expected br accepted in multi-value list")
	}
	if compress.AcceptsEncoding(makeReq(""), "gzip") {
		t.Error("expected gzip rejected when no Accept-Encoding")
	}
	if compress.AcceptsEncoding(makeReq("br"), "gzip") {
		t.Error("expected gzip rejected when only br is listed")
	}
}

func TestGzipBytes(t *testing.T) {
	src := []byte(strings.Repeat("Hello, World! ", 100))
	compressed, err := compress.GzipBytes(src, 5)
	if err != nil {
		t.Fatalf("GzipBytes error: %v", err)
	}
	if len(compressed) == 0 {
		t.Fatal("GzipBytes returned empty result")
	}
	if len(compressed) >= len(src) {
		t.Errorf("compressed (%d) should be smaller than original (%d)", len(compressed), len(src))
	}

	// Decompress and verify.
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("decompressed content does not match original")
	}
}

func TestMiddleware_CompressesEligibleResponse(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5}

	body := strings.Repeat("Hello compressed world! ", 50) // 1200 bytes
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", rr.Header().Get("Content-Encoding"))
	}

	// Decompress and verify body.
	gr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if string(got) != body {
		t.Errorf("decompressed body mismatch")
	}
}

func TestMiddleware_SkipsIneligibleContentType(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("fake png data that is long enough to pass min size check in normal flow"))
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/img.png", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("Content-Encoding") == "gzip" {
		t.Error("Content-Encoding should not be gzip for image/png")
	}
}

func TestMiddleware_SkipsWhenNoAcceptEncoding(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 10, Level: 5}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, strings.Repeat("x", 500))
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("Content-Encoding") == "gzip" {
		t.Error("should not compress when Accept-Encoding absent")
	}
}

func TestMiddleware_DisabledConfig(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: false}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next should be called when compression disabled")
	}
	if rr.Header().Get("Content-Encoding") != "" {
		t.Error("should not set Content-Encoding when compression disabled")
	}
}

func TestMiddleware_SetsVaryHeader(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "hello")
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if vary := rr.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary = %q, should contain Accept-Encoding", vary)
	}
}

// ---------------------------------------------------------------------------
// Additional compression coverage
// ---------------------------------------------------------------------------

// TestMiddleware_SkipsPostMethod verifies non-GET/HEAD methods bypass compression.
func TestMiddleware_SkipsPostMethod(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, strings.Repeat("x", 500))
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("data"))
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next should be called for POST requests")
	}
	if rr.Header().Get("Content-Encoding") == "gzip" {
		t.Error("POST response must not be compressed by middleware")
	}
}

// TestMiddleware_SkipsBelowMinSize verifies small responses are not compressed.
func TestMiddleware_SkipsBelowMinSize(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1000, Level: 5}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "tiny") // only 4 bytes — below MinSize
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("Content-Encoding") == "gzip" {
		t.Error("response below MinSize should not be gzip-encoded")
	}
	if rr.Body.String() != "tiny" {
		t.Errorf("body = %q, want tiny", rr.Body.String())
	}
}

// TestMiddleware_SkipsAlreadyEncoded ensures pre-encoded responses are passed through.
func TestMiddleware_SkipsAlreadyEncoded(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "br") // pre-compressed brotli
		io.WriteString(w, strings.Repeat("compressed!", 100))
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("Content-Encoding") != "br" {
		t.Errorf("Content-Encoding = %q, want br (should not re-compress)", rr.Header().Get("Content-Encoding"))
	}
}

// TestMiddleware_WriteHeaderExplicit exercises the lazyGzipWriter.WriteHeader path
// by having the handler call WriteHeader before Write.
func TestMiddleware_WriteHeaderExplicit(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK) // explicit WriteHeader before Write
		io.WriteString(w, strings.Repeat("Hello explicit! ", 80))
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Response must still be compressed.
	if rr.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip after explicit WriteHeader", rr.Header().Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if !strings.Contains(string(got), "Hello explicit!") {
		t.Error("decompressed body should contain original content")
	}
}

// TestMiddleware_NoWriteAtAll covers the close() path when the handler never
// calls Write (status-only responses).
func TestMiddleware_NoWriteAtAll(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204 — no body
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodGet, "/empty", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rr.Code)
	}
	if rr.Header().Get("Content-Encoding") == "gzip" {
		t.Error("no-body 204 response should not have Content-Encoding: gzip")
	}
}

// TestGzipBytes_InvalidLevel verifies out-of-range levels fall back to DefaultCompression.
func TestGzipBytes_InvalidLevel(t *testing.T) {
	src := []byte(strings.Repeat("test data for invalid level ", 50))

	// Level 0 and 10 are both outside [BestSpeed, BestCompression].
	for _, level := range []int{0, 10, -5, 100} {
		t.Run(fmt.Sprintf("level=%d", level), func(t *testing.T) {
			compressed, err := compress.GzipBytes(src, level)
			if err != nil {
				t.Fatalf("GzipBytes(%d) unexpected error: %v", level, err)
			}
			// Must decompress correctly.
			gr, err := gzip.NewReader(bytes.NewReader(compressed))
			if err != nil {
				t.Fatalf("gzip.NewReader: %v", err)
			}
			got, err := io.ReadAll(gr)
			if err != nil {
				t.Fatalf("io.ReadAll: %v", err)
			}
			if !bytes.Equal(got, src) {
				t.Error("decompressed content does not match original for invalid level")
			}
		})
	}
}

// TestAcceptsEncoding_MultipleValues covers more Accept-Encoding negotiation cases.
func TestAcceptsEncoding_MultipleValues(t *testing.T) {
	cases := []struct {
		header string
		enc    string
		want   bool
	}{
		{"br, gzip, zstd", "gzip", true},
		{"br, gzip, zstd", "br", true},
		{"br, gzip, zstd", "zstd", true},
		{"br, gzip, zstd", "deflate", false},
		{"identity", "gzip", false},
		{"gzip;q=1.0", "gzip", true}, // quality parameter is stripped per RFC 7231
	}

	for _, tc := range cases {
		t.Run(tc.header+"|"+tc.enc, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", tc.header)
			got := compress.AcceptsEncoding(req, tc.enc)
			if got != tc.want {
				t.Errorf("AcceptsEncoding(%q, %q) = %v, want %v", tc.header, tc.enc, got, tc.want)
			}
		})
	}
}

// TestMiddleware_HeadRequest verifies HEAD passes through without body compression.
func TestMiddleware_HeadRequest(t *testing.T) {
	cfg := &config.CompressionConfig{Enabled: true, MinSize: 1, Level: 5}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusOK)
		// HEAD: no body written
	})

	handler := compress.Middleware(cfg, next)
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next should be called for HEAD requests")
	}
}

// ---------------------------------------------------------------------------
// SEC-013: q=0 explicit rejection
// ---------------------------------------------------------------------------

// TestAcceptsEncoding_QZeroRejection verifies that q=0 signals explicit denial.
func TestAcceptsEncoding_QZeroRejection(t *testing.T) {
	cases := []struct {
		header string
		enc    string
		want   bool
	}{
		{"gzip;q=0", "gzip", false},
		{"gzip;q=0.0", "gzip", false},
		{"gzip;q=0.00", "gzip", false},
		{"gzip;q=0.000", "gzip", false},
		{"gzip;q=0.1", "gzip", true}, // non-zero q is still accepted
		{"gzip;q=1.0", "gzip", true},
		{"br, gzip;q=0", "gzip", false}, // gzip explicitly rejected
		{"br, gzip;q=0", "br", true},    // br still accepted
	}

	for _, tc := range cases {
		t.Run(tc.header+"|"+tc.enc, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", tc.header)
			got := compress.AcceptsEncoding(req, tc.enc)
			if got != tc.want {
				t.Errorf("AcceptsEncoding(%q, %q) = %v, want %v", tc.header, tc.enc, got, tc.want)
			}
		})
	}
}
