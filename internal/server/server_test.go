package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
)

func TestNewRedirectsToConfiguredHost(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:         ":8080",
		TLSAddr:      ":8443",
		RedirectHost: "static.example.com",
		TLSCert:      "server.crt",
		TLSKey:       "server.key",
	}

	s := New(cfg, nil, http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "http://attacker.test/assets/app.js?v=1", nil)
	req.Host = "attacker.test"
	rr := httptest.NewRecorder()

	s.httpServer().Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMovedPermanently)
	}
	if got := rr.Header().Get("Location"); got != "https://static.example.com:8443/assets/app.js?v=1" {
		t.Fatalf("Location = %q, want %q", got, "https://static.example.com:8443/assets/app.js?v=1")
	}
}

func TestNewRedirectsToTLSAddrHostWhenConfigured(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:    ":8080",
		TLSAddr: "secure.example.com:443",
		TLSCert: "server.crt",
		TLSKey:  "server.key",
	}

	s := New(cfg, nil, http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "http://attacker.test/", nil)
	req.Host = "attacker.test"
	rr := httptest.NewRecorder()

	s.httpServer().Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMovedPermanently)
	}
	if got := rr.Header().Get("Location"); got != "https://secure.example.com/" {
		t.Fatalf("Location = %q, want %q", got, "https://secure.example.com/")
	}
}

func TestNewRejectsRedirectWithoutCanonicalHost(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:    ":8080",
		TLSAddr: ":8443",
		TLSCert: "server.crt",
		TLSKey:  "server.key",
	}

	s := New(cfg, nil, http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "http://attacker.test/login", nil)
	req.Host = "attacker.test"
	rr := httptest.NewRecorder()

	s.httpServer().Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if got := rr.Header().Get("Location"); got != "" {
		t.Fatalf("Location = %q, want empty", got)
	}
}

func TestRedirectAuthorityRejectsInvalidConfiguredHost(t *testing.T) {
	if _, err := redirectAuthority("https://evil.example/path", ":443"); err == nil {
		t.Fatal("redirectAuthority accepted invalid configured host")
	}
}
