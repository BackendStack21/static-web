package server

import (
	"bufio"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

func sehSecurityConfig() *config.SecurityConfig {
	return &config.SecurityConfig{
		CSP:                   "default-src 'self'",
		ReferrerPolicy:        "no-referrer",
		PermissionsPolicy:     "camera=()",
		HSTSMaxAge:            31536000,
		HSTSIncludeSubdomains: true,
	}
}

// sehHeaders parses the response head returned by rawExchange/readHead.
func sehHeaders(t *testing.T, head string) http.Header {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(head+"\r\n")), nil)
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	resp.Body.Close()
	return resp.Header
}

func sehAssert(t *testing.T, label string, h http.Header, wantHSTS bool) {
	t.Helper()
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "SAMEORIGIN",
		"Referrer-Policy":         "no-referrer",
		"Permissions-Policy":      "camera=()",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", label, k, got, want)
		}
	}
	hsts := h.Get("Strict-Transport-Security")
	if wantHSTS && hsts != testHSTS {
		t.Errorf("%s: Strict-Transport-Security = %q, want %q", label, hsts, testHSTS)
	}
	if !wantHSTS && hsts != "" {
		t.Errorf("%s: Strict-Transport-Security = %q over plain HTTP, want none", label, hsts)
	}
}

const (
	sehGarbage = "GARBAGE\r\nHost: x\r\n\r\n"
)

var sehBigHeaders = "GET / HTTP/1.1\r\nHost: x\r\nX-Big: " + strings.Repeat("a", 6000) + "\r\n\r\n"

// fasthttp answers a request it cannot parse (400) or whose header block is
// too large (431) itself, before any handler runs. Those responses carried
// none of the security headers and, on the TLS listener, no HSTS. The
// servers' ErrorHandler now adds them, keeping fasthttp's status codes.
func TestServerErrors_CarrySecurityHeaders(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		cfg := &config.ServerConfig{Addr: "127.0.0.1:0", ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
		s := New(cfg, sehSecurityConfig(), okHandler)
		if err := s.Start(cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer shutdownServer(t, s)

		status, head := rawExchange(t, s.HTTPAddr().String(), sehGarbage)
		if status != fasthttp.StatusBadRequest {
			t.Fatalf("malformed request: status %d, want 400", status)
		}
		sehAssert(t, "400", sehHeaders(t, head), false)

		status, head = rawExchange(t, s.HTTPAddr().String(), sehBigHeaders)
		if status != fasthttp.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("oversized headers: status %d, want 431", status)
		}
		sehAssert(t, "431", sehHeaders(t, head), false)
	})

	t.Run("https", func(t *testing.T) {
		certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
		cfg := &config.ServerConfig{
			Addr: "127.0.0.1:0", TLSAddr: "127.0.0.1:0", RedirectHost: "localhost",
			TLSCert: certPath, TLSKey: keyPath,
			ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		}
		s := New(cfg, sehSecurityConfig(), okHandler)
		if err := s.Start(cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer shutdownServer(t, s)

		for _, tc := range []struct {
			name, req string
			status    int
		}{
			{"400", sehGarbage, fasthttp.StatusBadRequest},
			{"431", sehBigHeaders, fasthttp.StatusRequestHeaderFieldsTooLarge},
		} {
			conn, err := tls.Dial("tcp", s.HTTPSAddr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only self-signed cert
			if err != nil {
				t.Fatalf("tls dial: %v", err)
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(conn, tc.req); err != nil {
				t.Fatalf("write: %v", err)
			}
			status, head := readHead(t, bufio.NewReader(conn))
			conn.Close()
			if status != tc.status {
				t.Fatalf("https %s: status %d, want %d", tc.name, status, tc.status)
			}
			sehAssert(t, "https "+tc.name, sehHeaders(t, head), true)
		}

		// The plain-HTTP listener only redirects; its own errors get the
		// security headers but never HSTS.
		status, head := rawExchange(t, s.HTTPAddr().String(), sehGarbage)
		if status != fasthttp.StatusBadRequest {
			t.Fatalf("redirect listener: status %d, want 400", status)
		}
		sehAssert(t, "redirect listener 400", sehHeaders(t, head), false)
	})
}
