package server

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

const testHSTS = "max-age=31536000; includeSubDomains"

func hstsSecurityConfig() *config.SecurityConfig {
	return &config.SecurityConfig{HSTSMaxAge: 31536000, HSTSIncludeSubdomains: true}
}

// HTTP-F4: ctx.Error resets the response, so an HSTS header set before the
// handler ran was lost on every error response from the HTTPS listener.
func TestHTTPSHandler_ErrorResponseKeepsHSTS(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:         ":0",
		TLSAddr:      ":0",
		TLSCert:      "dummy.crt", // not loaded by New
		TLSKey:       "dummy.key",
		RedirectHost: "example.com",
	}
	failing := func(ctx *fasthttp.RequestCtx) {
		ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
	}
	s := New(cfg, hstsSecurityConfig(), failing)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/x")
	s.https.Handler(&ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got)
	}
	if got := string(ctx.Response.Header.Peek("Strict-Transport-Security")); got != testHSTS {
		t.Errorf("Strict-Transport-Security = %q on error response, want %q", got, testHSTS)
	}
}

// HSTS must reach the wire for streamed bodies too: fasthttp writes the
// headers only after the handler has returned.
func TestHTTPS_HSTSOnWireForStreamedAndErrorResponses(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	cfg := &config.ServerConfig{
		Addr:         "127.0.0.1:0",
		TLSAddr:      "127.0.0.1:0",
		RedirectHost: "localhost",
		TLSCert:      certPath,
		TLSKey:       keyPath,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	h := func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/stream":
			ctx.SetContentType("text/plain")
			ctx.SetBodyStream(strings.NewReader(strings.Repeat("s", 70000)), 70000)
		default:
			ctx.Error("Forbidden", fasthttp.StatusForbidden)
		}
	}
	s := New(cfg, hstsSecurityConfig(), h)
	if err := s.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer shutdownServer(t, s)

	for _, tc := range []struct {
		path       string
		wantStatus int
		wantLen    int
	}{
		{"/stream", 200, 70000},
		{"/denied", 403, len("Forbidden")},
	} {
		conn, err := tls.Dial("tcp", s.HTTPSAddr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only self-signed cert
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", tc.path)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			conn.Close()
			t.Fatalf("%s: read response: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != tc.wantStatus || len(body) != tc.wantLen {
			t.Errorf("%s: status %d len %d, want %d len %d", tc.path, resp.StatusCode, len(body), tc.wantStatus, tc.wantLen)
		}
		if got := resp.Header.Get("Strict-Transport-Security"); got != testHSTS {
			t.Errorf("%s: Strict-Transport-Security = %q, want %q", tc.path, got, testHSTS)
		}
	}
}
