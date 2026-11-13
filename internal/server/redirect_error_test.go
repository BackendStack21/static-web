package server

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

// loRedirectBadHostServer returns a Server whose HTTP listener redirects to
// HTTPS but cannot derive a redirect host (no redirect_host and a wildcard
// TLS address), so every plain-HTTP request is answered with a 400.
func loRedirectBadHostServer() *Server {
	cfg := &config.ServerConfig{
		Addr:    ":8080",
		TLSAddr: ":8443",
		TLSCert: "server.crt",
		TLSKey:  "server.key",
	}
	return New(cfg, sehSecurityConfig(), okHandler)
}

// loWireResponse serialises resp as the server writes it and parses it back
// with net/http, returning the response and its body.
func loWireResponse(t *testing.T, resp *fasthttp.Response, method string) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := resp.Write(bw); err != nil {
		t.Fatalf("serialise response: %v", err)
	}
	_ = bw.Flush()
	hr, err := http.ReadResponse(bufio.NewReader(&buf), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	body, err := io.ReadAll(hr.Body)
	hr.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return hr, body
}

// The redirect listener's own responses (the 301 and this 400) carry no
// security headers; it has no security middleware. Its invalid-host 400 was
// written with ctx.Error, whose Response.Reset drops every header already on
// the response, so a header set before the redirect handler ran (by an outer
// layer, as security.Middleware sets the security headers before calling
// next) was lost. The 400 must keep such headers and still be a well-formed
// plain-text response.
func TestRedirectListener_InvalidHost400KeepsEarlierHeaders(t *testing.T) {
	s := loRedirectBadHostServer()

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/login?next=%2F")
	ctx.Request.Header.SetHost("attacker.test")
	earlier := security.ResponseHeaders(sehSecurityConfig())
	for _, h := range earlier {
		ctx.Response.Header.Set(h.Key, h.Value)
	}
	ctx.Response.Header.Set("X-Lo-Earlier", "kept")

	s.fasthttpServer().Handler(&ctx)

	hr, _ := loWireResponse(t, &ctx.Response, "GET")
	if hr.StatusCode != fasthttp.StatusBadRequest {
		t.Fatalf("status = %d, want 400", hr.StatusCode)
	}
	for _, h := range earlier {
		if got := hr.Header.Get(h.Key); got != h.Value {
			t.Errorf("%s = %q, want %q (set before the handler ran)", h.Key, got, h.Value)
		}
	}
	if got := hr.Header.Get("X-Lo-Earlier"); got != "kept" {
		t.Errorf("X-Lo-Earlier = %q, want %q (set before the handler ran)", got, "kept")
	}
}

// The invalid-host 400 is a well-formed plain-text response for GET and
// HEAD: the status, a text/plain Content-Type, the message as the body with
// a matching Content-Length, and no Location.
func TestRedirectListener_InvalidHost400IsWellFormed(t *testing.T) {
	s := loRedirectBadHostServer()
	const msg = "Bad Request: invalid redirect host"

	for _, method := range []string{"GET", "HEAD"} {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod(method)
		ctx.Request.SetRequestURI("/login")
		ctx.Request.Header.SetHost("attacker.test")

		s.fasthttpServer().Handler(&ctx)

		// fasthttp.Server sets SkipBody on a HEAD response once the handler
		// has returned.
		if method == "HEAD" {
			ctx.Response.SkipBody = true
		}
		hr, body := loWireResponse(t, &ctx.Response, method)
		if hr.StatusCode != fasthttp.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", method, hr.StatusCode)
		}
		if got, want := hr.Header.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
			t.Errorf("%s: Content-Type = %q, want %q", method, got, want)
		}
		if got := hr.Header.Get("Content-Length"); got != strconv.Itoa(len(msg)) {
			t.Errorf("%s: Content-Length = %q, want %d", method, got, len(msg))
		}
		if got := hr.Header.Get("Location"); got != "" {
			t.Errorf("%s: Location = %q, want none", method, got)
		}
		wantBody := msg
		if method == "HEAD" {
			wantBody = ""
		}
		if string(body) != wantBody {
			t.Errorf("%s: body = %q, want %q", method, body, wantBody)
		}
	}
}
