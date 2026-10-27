package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

func okHandler(ctx *fasthttp.RequestCtx) { ctx.SetStatusCode(fasthttp.StatusOK) }

// writeSelfSignedCert writes a throwaway ECDSA cert/key pair into dir.
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func shutdownServer(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// OPS-001: a listen failure must be returned synchronously from Start.
func TestStart_ReturnsErrorWhenPortInUse(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg := &config.ServerConfig{Addr: occupied.Addr().String()}
	s := New(cfg, nil, okHandler)

	done := make(chan error, 1)
	go func() { done <- s.Start(cfg) }()

	select {
	case err := <-done:
		if err == nil {
			shutdownServer(t, s)
			t.Fatal("Start returned nil for an occupied port")
		}
		if !strings.Contains(err.Error(), "HTTP listen") {
			t.Errorf("error = %v, want HTTP listen error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return for an occupied port")
	}
}

// OPS-002: a missing TLS cert must produce an error, not a goroutine panic.
func TestStart_ReturnsErrorForMissingTLSCert(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.ServerConfig{
		Addr:         "127.0.0.1:0",
		TLSAddr:      "127.0.0.1:0",
		RedirectHost: "localhost",
		TLSCert:      filepath.Join(dir, "missing.crt"),
		TLSKey:       filepath.Join(dir, "missing.key"),
	}
	s := New(cfg, nil, okHandler)
	err := s.Start(cfg)
	if err == nil {
		shutdownServer(t, s)
		t.Fatal("Start returned nil for a missing TLS cert")
	}
	if !strings.Contains(err.Error(), "TLS") {
		t.Errorf("error = %v, want TLS keypair error", err)
	}
	if s.HTTPAddr() != nil {
		t.Error("HTTP listener must not be opened when the TLS keypair is invalid")
	}
}

func TestStart_ReturnsErrorForMismatchedKeypair(t *testing.T) {
	certA, _ := writeSelfSignedCert(t, t.TempDir())
	_, keyB := writeSelfSignedCert(t, t.TempDir())
	cfg := &config.ServerConfig{
		Addr:         "127.0.0.1:0",
		TLSAddr:      "127.0.0.1:0",
		RedirectHost: "localhost",
		TLSCert:      certA,
		TLSKey:       keyB,
	}
	s := New(cfg, nil, okHandler)
	if err := s.Start(cfg); err == nil {
		shutdownServer(t, s)
		t.Fatal("Start returned nil for a mismatched cert/key pair")
	}
}

// OPS-001: when HTTPS fails to bind, the already-bound HTTP listener is closed.
func TestStart_HTTPSListenFailureClosesHTTPListener(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg := &config.ServerConfig{
		Addr:         "127.0.0.1:0",
		TLSAddr:      occupied.Addr().String(),
		RedirectHost: "localhost",
		TLSCert:      certPath,
		TLSKey:       keyPath,
	}
	s := New(cfg, nil, okHandler)
	err = s.Start(cfg)
	if err == nil {
		shutdownServer(t, s)
		t.Fatal("Start returned nil for an occupied HTTPS port")
	}
	if !strings.Contains(err.Error(), "HTTPS listen") {
		t.Errorf("error = %v, want HTTPS listen error", err)
	}
	if s.HTTPAddr() != nil {
		t.Error("HTTP listener should be released after HTTPS bind failure")
	}
}

func TestListenNetwork(t *testing.T) {
	cases := map[string]string{
		":8080":            "tcp4",
		"0.0.0.0:8080":     "tcp4",
		"127.0.0.1:8080":   "tcp4",
		"[::1]:8080":       "tcp",
		"[::]:8080":        "tcp",
		"localhost:8080":   "tcp",
		"example.com:8443": "tcp",
	}
	for addr, want := range cases {
		if got := listenNetwork(addr); got != want {
			t.Errorf("listenNetwork(%q) = %q, want %q", addr, got, want)
		}
	}
}

// OPS-003: IPv6 listen addresses work (previously rejected by tcp4).
func TestStart_IPv6Loopback(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	probe.Close()

	cfg := &config.ServerConfig{Addr: "[::1]:0"}
	s := New(cfg, nil, okHandler)
	if err := s.Start(cfg); err != nil {
		t.Fatalf("Start on [::1]:0: %v", err)
	}
	defer shutdownServer(t, s)

	addr := s.HTTPAddr()
	if addr == nil {
		t.Fatal("HTTPAddr() = nil after Start")
	}
	conn, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	conn.Close()
}

func TestStart_ServesHTTPS(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	cfg := &config.ServerConfig{
		Addr:         "127.0.0.1:0",
		TLSAddr:      "127.0.0.1:0",
		RedirectHost: "localhost",
		TLSCert:      certPath,
		TLSKey:       keyPath,
	}
	s := New(cfg, nil, okHandler)
	if err := s.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.HTTPAddr() == nil || s.HTTPSAddr() == nil {
		t.Fatalf("listeners not bound: http=%v https=%v", s.HTTPAddr(), s.HTTPSAddr())
	}

	client := &fasthttp.Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only self-signed cert
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI("https://" + s.HTTPSAddr().String() + "/")
	if err := client.DoTimeout(req, resp, 5*time.Second); err != nil {
		t.Fatalf("HTTPS request: %v", err)
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		t.Errorf("HTTPS status = %d, want 200", resp.StatusCode())
	}

	shutdownServer(t, s)

	// After a graceful shutdown the serve loops exit without reporting errors
	// and the Errors channel is closed.
	select {
	case err, ok := <-s.Errors():
		if ok {
			t.Errorf("unexpected serve error after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Errors() channel was not closed after shutdown")
	}
}
