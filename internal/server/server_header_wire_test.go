package server

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// rawExchange writes req on a fresh TCP connection to addr and returns the
// raw response head (status line and header lines) and status code.
func rawExchange(t *testing.T, addr, req string) (status int, head string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return readHead(t, bufio.NewReader(conn))
}

func readHead(t *testing.T, br *bufio.Reader) (status int, head string) {
	t.Helper()
	tp := strings.Builder{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read response head (after %q): %v", tp.String(), err)
		}
		if line == "\r\n" {
			break
		}
		tp.WriteString(line)
	}
	head = tp.String()
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(head+"\r\n")), nil)
	if err != nil {
		t.Fatalf("parse response head %q: %v", head, err)
	}
	resp.Body.Close()
	return resp.StatusCode, head
}

func assertNoServerHeader(t *testing.T, label, head string) {
	t.Helper()
	for _, line := range strings.Split(head, "\r\n") {
		if name, _, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Server") {
			t.Errorf("%s: response carries %q, want no Server header (SEC-007/HTTP-F6)", label, line)
		}
	}
}

// HTTP-F6: Name "" alone does not suppress the header; fasthttp falls back to
// "Server: fasthttp" unless NoDefaultServerHeader is set. Check the wire,
// including the error responses fasthttp writes itself.
func TestServerHeader_AbsentOnWire(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:         "127.0.0.1:0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  5 * time.Second,
	}
	s := New(cfg, nil, okHandler)
	if err := s.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer shutdownServer(t, s)
	addr := s.HTTPAddr().String()

	// Normal 200 from the handler.
	status, head := rawExchange(t, addr, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if status != fasthttp.StatusOK {
		t.Fatalf("normal request: status %d, want 200\n%s", status, head)
	}
	assertNoServerHeader(t, "200", head)

	// 400 written by fasthttp for a malformed request line.
	status, head = rawExchange(t, addr, "GARBAGE\r\nHost: x\r\n\r\n")
	if status != fasthttp.StatusBadRequest {
		t.Fatalf("malformed request line: status %d, want 400\n%s", status, head)
	}
	assertNoServerHeader(t, "400", head)

	// 431 written by fasthttp when the header block exceeds the 4 KiB
	// default ReadBufferSize.
	big := "GET / HTTP/1.1\r\nHost: x\r\nX-Big: " + strings.Repeat("a", 6000) + "\r\n\r\n"
	status, head = rawExchange(t, addr, big)
	if status != fasthttp.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized headers: status %d, want 431\n%s", status, head)
	}
	assertNoServerHeader(t, "431", head)
}

func TestServerHeader_AbsentOnWireHTTPSAndRedirect(t *testing.T) {
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
	s := New(cfg, nil, okHandler)
	if err := s.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer shutdownServer(t, s)

	// Plain-HTTP listener: 301 redirect to HTTPS.
	status, head := rawExchange(t, s.HTTPAddr().String(), "GET /a HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if status != fasthttp.StatusMovedPermanently {
		t.Fatalf("redirect: status %d, want 301\n%s", status, head)
	}
	assertNoServerHeader(t, "301 redirect", head)

	// HTTPS listener: 200 from the handler.
	conn, err := tls.Dial("tcp", s.HTTPSAddr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only self-signed cert
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	status, head = readHead(t, bufio.NewReader(conn))
	if status != fasthttp.StatusOK {
		t.Fatalf("https: status %d, want 200\n%s", status, head)
	}
	assertNoServerHeader(t, "https 200", head)
}
