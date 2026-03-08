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
