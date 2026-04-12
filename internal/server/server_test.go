package server

import (
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

func TestNewRedirectsToConfiguredHost(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:         ":8080",
		TLSAddr:      ":8443",
		RedirectHost: "static.example.com",
		TLSCert:      "server.crt",
		TLSKey:       "server.key",
	}

	notFound := func(ctx *fasthttp.RequestCtx) {
		ctx.Error("Not Found", fasthttp.StatusNotFound)
	}
	s := New(cfg, nil, notFound)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/assets/app.js?v=1")
	ctx.Request.Header.SetHost("attacker.test")

	s.fasthttpServer().Handler(&ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusMovedPermanently {
		t.Fatalf("status = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusMovedPermanently)
	}
	if got := string(ctx.Response.Header.Peek("Location")); got != "https://static.example.com:8443/assets/app.js?v=1" {
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

	notFound := func(ctx *fasthttp.RequestCtx) {
		ctx.Error("Not Found", fasthttp.StatusNotFound)
	}
	s := New(cfg, nil, notFound)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("attacker.test")

	s.fasthttpServer().Handler(&ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusMovedPermanently {
		t.Fatalf("status = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusMovedPermanently)
	}
	if got := string(ctx.Response.Header.Peek("Location")); got != "https://secure.example.com/" {
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

	notFound := func(ctx *fasthttp.RequestCtx) {
		ctx.Error("Not Found", fasthttp.StatusNotFound)
	}
	s := New(cfg, nil, notFound)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/login")
	ctx.Request.Header.SetHost("attacker.test")

	s.fasthttpServer().Handler(&ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("status = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusBadRequest)
	}
	if got := string(ctx.Response.Header.Peek("Location")); got != "" {
		t.Fatalf("Location = %q, want empty", got)
	}
}

func TestRedirectAuthorityRejectsInvalidConfiguredHost(t *testing.T) {
	if _, err := redirectAuthority("https://evil.example/path", ":443"); err == nil {
		t.Fatal("redirectAuthority accepted invalid configured host")
	}
}

// ---------------------------------------------------------------------------
// Security-hardening defaults on fasthttp.Server (SEC-007, SEC-014, SEC-015)
// ---------------------------------------------------------------------------

func TestNew_HTTPOnly_SecurityDefaults(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:          ":8080",
		MaxConnsPerIP: 50,
	}
	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	s := New(cfg, nil, handler)

	// SEC-007: Server name must be empty to suppress identity disclosure.
	if s.http.Name != "" {
		t.Errorf("HTTP Name = %q, want empty (SEC-007)", s.http.Name)
	}

	// SEC-014: MaxRequestBodySize must be 1024 (small explicit limit).
	if s.http.MaxRequestBodySize != 1024 {
		t.Errorf("HTTP MaxRequestBodySize = %d, want 1024 (SEC-014)", s.http.MaxRequestBodySize)
	}

	// SEC-015: MaxConnsPerIP must match configuration.
	if s.http.MaxConnsPerIP != 50 {
		t.Errorf("HTTP MaxConnsPerIP = %d, want 50 (SEC-015)", s.http.MaxConnsPerIP)
	}

	// No HTTPS server when TLS is not configured.
	if s.https != nil {
		t.Error("https server should be nil when TLS is not configured")
	}
}

func TestNew_TLS_SecurityDefaults(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:          ":8080",
		TLSAddr:       ":8443",
		TLSCert:       "dummy.crt",
		TLSKey:        "dummy.key",
		RedirectHost:  "example.com",
		MaxConnsPerIP: 100,
	}
	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	s := New(cfg, nil, handler)

	// --- HTTP server (redirect handler) ---

	if s.http.Name != "" {
		t.Errorf("HTTP Name = %q, want empty (SEC-007)", s.http.Name)
	}
	if s.http.MaxRequestBodySize != 1024 {
		t.Errorf("HTTP MaxRequestBodySize = %d, want 1024 (SEC-014)", s.http.MaxRequestBodySize)
	}
	if s.http.MaxConnsPerIP != 100 {
		t.Errorf("HTTP MaxConnsPerIP = %d, want 100 (SEC-015)", s.http.MaxConnsPerIP)
	}

	// --- HTTPS server ---

	if s.https == nil {
		t.Fatal("https server should not be nil when TLS is configured")
	}
	if s.https.Name != "" {
		t.Errorf("HTTPS Name = %q, want empty (SEC-007)", s.https.Name)
	}
	if s.https.MaxRequestBodySize != 1024 {
		t.Errorf("HTTPS MaxRequestBodySize = %d, want 1024 (SEC-014)", s.https.MaxRequestBodySize)
	}
	if s.https.MaxConnsPerIP != 100 {
		t.Errorf("HTTPS MaxConnsPerIP = %d, want 100 (SEC-015)", s.https.MaxConnsPerIP)
	}

	// TLS config must be present with minimum TLS 1.2.
	if s.https.TLSConfig == nil {
		t.Fatal("HTTPS TLSConfig should not be nil")
	}
	if s.https.TLSConfig.MinVersion != 0x0303 { // tls.VersionTLS12
		t.Errorf("HTTPS TLS MinVersion = %#x, want %#x (TLS 1.2)", s.https.TLSConfig.MinVersion, 0x0303)
	}
}

func TestNew_MaxConnsPerIP_Zero(t *testing.T) {
	cfg := &config.ServerConfig{
		Addr:          ":8080",
		MaxConnsPerIP: 0, // disabled
	}
	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	s := New(cfg, nil, handler)

	if s.http.MaxConnsPerIP != 0 {
		t.Errorf("HTTP MaxConnsPerIP = %d, want 0 (disabled)", s.http.MaxConnsPerIP)
	}
}
