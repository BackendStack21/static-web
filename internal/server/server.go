// Package server provides an HTTP/HTTPS server with graceful shutdown support,
// built on top of fasthttp.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/BackendStack21/static-web/internal/config"
	"github.com/valyala/fasthttp"
)

// Server wraps one HTTP and one optional HTTPS fasthttp.Server.
type Server struct {
	http     *fasthttp.Server
	https    *fasthttp.Server // nil when TLS is not configured
	httpLn   net.Listener
	httpsLn  net.Listener
	tlsCert  string
	tlsKey   string
	httpAddr string
	tlsAddr  string
}

var errInvalidRedirectHost = errors.New("invalid redirect host")

// New creates a Server from the provided configuration and handler.
// HTTPS is only configured when both TLSCert and TLSKey are non-empty.
// When TLS is configured, the HTTP server is replaced with a redirect handler
// that sends all requests to the HTTPS address (SEC-004).
func New(cfg *config.ServerConfig, secCfg *config.SecurityConfig, handler fasthttp.RequestHandler) *Server {
	s := &Server{
		httpAddr: cfg.Addr,
		tlsAddr:  cfg.TLSAddr,
		tlsCert:  cfg.TLSCert,
		tlsKey:   cfg.TLSKey,
	}

	httpHandler := handler
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		// Replace plain-HTTP handler with a permanent redirect to HTTPS.
		redirectAuth, err := redirectAuthority(cfg.RedirectHost, cfg.TLSAddr)
		httpHandler = func(ctx *fasthttp.RequestCtx) {
			if err != nil {
				ctx.Error("Bad Request: invalid redirect host", fasthttp.StatusBadRequest)
				return
			}
			target := (&url.URL{
				Scheme:   "https",
				Host:     redirectAuth,
				Path:     string(ctx.Path()),
				RawPath:  string(ctx.URI().PathOriginal()),
				RawQuery: string(ctx.QueryArgs().QueryString()),
			}).String()
			ctx.Redirect(target, fasthttp.StatusMovedPermanently)
		}
	}

	s.http = &fasthttp.Server{
		Handler:            httpHandler,
		Name:               "static-web",
		ReadTimeout:        cfg.ReadTimeout,
		WriteTimeout:       cfg.WriteTimeout,
		IdleTimeout:        cfg.IdleTimeout,
		MaxRequestBodySize: 0, // no request bodies for a static server
		DisableKeepalive:   false,
	}

	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
			},
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			},
			PreferServerCipherSuites: true, //nolint:staticcheck // intentional for TLS1.2 compat
		}

		// Wrap the handler to inject HSTS header on HTTPS responses.
		httpsHandler := handler
		if secCfg != nil && secCfg.HSTSMaxAge > 0 {
			hsts := fmt.Sprintf("max-age=%d", secCfg.HSTSMaxAge)
			if secCfg.HSTSIncludeSubdomains {
				hsts += "; includeSubDomains"
			}
			hstsValue := hsts
			httpsHandler = func(ctx *fasthttp.RequestCtx) {
				ctx.Response.Header.Set("Strict-Transport-Security", hstsValue)
				handler(ctx)
			}
		}

		s.https = &fasthttp.Server{
			Handler:            httpsHandler,
			Name:               "static-web",
			TLSConfig:          tlsCfg,
			ReadTimeout:        cfg.ReadTimeout,
			WriteTimeout:       cfg.WriteTimeout,
			IdleTimeout:        cfg.IdleTimeout,
			MaxRequestBodySize: 0,
			DisableKeepalive:   false,
		}
	}

	return s
}

// Start begins listening on HTTP (and HTTPS if configured) concurrently.
// It blocks until both listeners have started or one returns an error.
// Returns the first error encountered.
func (s *Server) Start(cfg *config.ServerConfig) error {
	lc := newListenConfig()

	errCh := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ln, err := lc.Listen(context.Background(), "tcp4", s.httpAddr)
		if err != nil {
			errCh <- fmt.Errorf("server: HTTP listen on %s: %w", s.httpAddr, err)
			return
		}
		s.httpLn = ln
		log.Printf("server: HTTP listening on %s", s.httpAddr)
		if err := s.http.Serve(ln); err != nil {
			errCh <- fmt.Errorf("server: HTTP serve: %w", err)
		}
	}()

	if s.https != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ln, err := lc.Listen(context.Background(), "tcp4", s.tlsAddr)
			if err != nil {
				errCh <- fmt.Errorf("server: HTTPS listen on %s: %w", s.tlsAddr, err)
				return
			}
			s.httpsLn = ln
			log.Printf("server: HTTPS listening on %s", s.tlsAddr)
			// fasthttp uses ServeTLS with cert/key files.
			if err := s.https.ServeTLSEmbed(ln, mustReadFile(cfg.TLSCert), mustReadFile(cfg.TLSKey)); err != nil {
				errCh <- fmt.Errorf("server: HTTPS serve: %w", err)
			}
		}()
	}

	// Return the first error, if any.
	go func() {
		wg.Wait()
		close(errCh)
	}()

	if err, ok := <-errCh; ok {
		return err
	}
	return nil
}

// Shutdown gracefully drains all active connections.
// It calls Shutdown on both HTTP and HTTPS servers concurrently.
// Returns the first error encountered, or nil if both complete cleanly.
func (s *Server) Shutdown(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	shutdown := func(srv *fasthttp.Server, name string) {
		defer wg.Done()
		if err := srv.ShutdownWithContext(ctx); err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("server: %s shutdown: %w", name, err))
			mu.Unlock()
		}
	}

	wg.Add(1)
	go shutdown(s.http, "HTTP")

	if s.https != nil {
		wg.Add(1)
		go shutdown(s.https, "HTTPS")
	}

	wg.Wait()

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// fasthttpServer returns the internal HTTP fasthttp server for testing purposes.
func (s *Server) fasthttpServer() *fasthttp.Server {
	return s.http
}

// mustReadFile reads a file and panics on error. Used only at startup for TLS certs.
func mustReadFile(path string) []byte {
	data, err := readFileBytes(path)
	if err != nil {
		panic(fmt.Sprintf("server: cannot read %q: %v", path, err))
	}
	return data
}

// readFileBytes reads a file and returns its contents.
func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func redirectAuthority(configuredHost, tlsAddr string) (string, error) {
	if configuredHost != "" {
		return authorityWithTLSPort(configuredHost, tlsAddr)
	}

	host, _, err := net.SplitHostPort(tlsAddr)
	if err != nil {
		return "", errInvalidRedirectHost
	}
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return "", errInvalidRedirectHost
	}

	return authorityWithTLSPort(host, tlsAddr)
}

func authorityWithTLSPort(hostOrAuthority, tlsAddr string) (string, error) {
	if strings.ContainsAny(hostOrAuthority, "/\\@?#%") {
		return "", errInvalidRedirectHost
	}

	host, port, hasPort, err := splitHostPortOptional(hostOrAuthority)
	if err != nil {
		return "", errInvalidRedirectHost
	}
	if !validRedirectHost(host) {
		return "", errInvalidRedirectHost
	}
	if hasPort {
		if !validPort(port) {
			return "", errInvalidRedirectHost
		}
		return joinHostPort(host, port), nil
	}

	_, tlsPort, err := net.SplitHostPort(tlsAddr)
	if err != nil {
		return "", errInvalidRedirectHost
	}
	if tlsPort == "443" {
		return hostForURL(host), nil
	}
	return joinHostPort(host, tlsPort), nil
}

func splitHostPortOptional(value string) (host, port string, hasPort bool, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", false, errInvalidRedirectHost
	}
	if h, p, err := net.SplitHostPort(value); err == nil {
		return trimIPv6Brackets(h), p, true, nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return trimIPv6Brackets(value), "", false, nil
	}
	if strings.Count(value, ":") == 1 {
		return "", "", false, errInvalidRedirectHost
	}
	return trimIPv6Brackets(value), "", false, nil
}

func trimIPv6Brackets(host string) string {
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

func validRedirectHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/\\@?#%") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func hostForURL(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func joinHostPort(host, port string) string {
	return net.JoinHostPort(host, port)
}

func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// newListenConfig returns a net.ListenConfig with platform-specific options.
// The actual implementation varies by OS (see server_linux.go / server_other.go).
var newListenConfig = func() net.ListenConfig {
	return platformListenConfig()
}
