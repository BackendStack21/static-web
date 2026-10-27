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
	"sync/atomic"

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

	mu       sync.Mutex // guards httpLn, httpsLn and errCh (OPS-004)
	errCh    chan error
	stopping atomic.Bool // set by Shutdown so serve-loop exits are not reported as errors
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
		Name:               "", // SEC-007: suppress server identity disclosure
		ReadTimeout:        cfg.ReadTimeout,
		WriteTimeout:       cfg.WriteTimeout,
		IdleTimeout:        cfg.IdleTimeout,
		MaxRequestBodySize: 1024,              // SEC-014: explicit small limit (static server has no request bodies)
		MaxConnsPerIP:      cfg.MaxConnsPerIP, // SEC-015: per-IP connection rate limiting
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
			// SEC-009: PreferServerCipherSuites removed — deprecated since Go 1.17
			// and a no-op since Go 1.22. The runtime now always selects the most
			// secure cipher agreed by both parties.
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
			Name:               "", // SEC-007: suppress server identity disclosure
			TLSConfig:          tlsCfg,
			ReadTimeout:        cfg.ReadTimeout,
			WriteTimeout:       cfg.WriteTimeout,
			IdleTimeout:        cfg.IdleTimeout,
			MaxRequestBodySize: 1024,              // SEC-014: explicit small limit
			MaxConnsPerIP:      cfg.MaxConnsPerIP, // SEC-015: per-IP connection rate limiting
			DisableKeepalive:   false,
		}
	}

	return s
}

// Start binds the HTTP listener (and the HTTPS listener when TLS is
// configured) and then serves them in background goroutines.
//
// Start does not block while serving: it returns as soon as every listener is
// bound. A non-nil error means nothing is being served — the TLS keypair could
// not be loaded or a listen address could not be bound — and any listener that
// was already opened has been closed again (OPS-001). The TLS keypair is
// loaded and validated before any socket is opened so that a bad cert/key path
// is reported as an error instead of panicking inside a goroutine (OPS-002).
//
// Errors that occur later while serving are delivered on Errors(). The cfg
// parameter is retained for API compatibility; addresses and TLS paths are
// taken from the configuration passed to New.
func (s *Server) Start(cfg *config.ServerConfig) error {
	var certPEM, keyPEM []byte
	if s.https != nil {
		var err error
		certPEM, keyPEM, err = loadKeyPair(s.tlsCert, s.tlsKey)
		if err != nil {
			return err
		}
	}

	lc := newListenConfig()

	httpLn, err := lc.Listen(context.Background(), listenNetwork(s.httpAddr), s.httpAddr)
	if err != nil {
		return fmt.Errorf("server: HTTP listen on %s: %w", s.httpAddr, err)
	}

	var httpsLn net.Listener
	if s.https != nil {
		httpsLn, err = lc.Listen(context.Background(), listenNetwork(s.tlsAddr), s.tlsAddr)
		if err != nil {
			_ = httpLn.Close()
			return fmt.Errorf("server: HTTPS listen on %s: %w", s.tlsAddr, err)
		}
	}

	errCh := make(chan error, 2)

	// OPS-004: listeners and the error channel are published under s.mu so
	// that accessors called from other goroutines never race with Start.
	s.mu.Lock()
	s.httpLn = httpLn
	s.httpsLn = httpsLn
	s.errCh = errCh
	s.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	log.Printf("server: HTTP listening on %s", httpLn.Addr())
	go func() {
		defer wg.Done()
		if err := s.http.Serve(httpLn); err != nil && !s.stopping.Load() {
			errCh <- fmt.Errorf("server: HTTP serve: %w", err)
		}
	}()

	if httpsLn != nil {
		wg.Add(1)
		log.Printf("server: HTTPS listening on %s", httpsLn.Addr())
		go func() {
			defer wg.Done()
			if err := s.https.ServeTLSEmbed(httpsLn, certPEM, keyPEM); err != nil && !s.stopping.Load() {
				errCh <- fmt.Errorf("server: HTTPS serve: %w", err)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	return nil
}

// Errors returns a channel that receives fatal errors from the serve loops
// after a successful Start. The channel is closed once every serve loop has
// exited. Errors caused by Shutdown are not reported. Before Start succeeds
// it returns nil (a channel that never delivers).
func (s *Server) Errors() <-chan error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errCh
}

// HTTPAddr returns the bound HTTP listener address, or nil if not listening.
func (s *Server) HTTPAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpLn == nil {
		return nil
	}
	return s.httpLn.Addr()
}

// HTTPSAddr returns the bound HTTPS listener address, or nil if not listening.
func (s *Server) HTTPSAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpsLn == nil {
		return nil
	}
	return s.httpsLn.Addr()
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

	s.stopping.Store(true)

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

// loadKeyPair reads the PEM certificate and key and verifies that they form a
// valid pair (OPS-002). The raw PEM bytes are returned for ServeTLSEmbed.
func loadKeyPair(certPath, keyPath string) (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("server: cannot read TLS certificate %q: %w", certPath, err)
	}
	keyPEM, err = os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("server: cannot read TLS key %q: %w", keyPath, err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, nil, fmt.Errorf("server: invalid TLS keypair (cert %q, key %q): %w", certPath, keyPath, err)
	}
	return certPEM, keyPEM, nil
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

// listenNetwork picks the network for net.ListenConfig.Listen (OPS-003).
//
// An empty host (":8080") or an IPv4 literal keeps the IPv4-only "tcp4"
// listener, which avoids dual-stack overhead and was measured to be
// significantly faster. IPv6 literals ("[::1]:8080", "[::]:8080") and
// hostnames use "tcp" so they are no longer rejected by a hard-coded tcp4.
// Use host "::" to listen dual-stack on all interfaces.
func listenNetwork(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return "tcp4"
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return "tcp4"
	}
	return "tcp"
}

// newListenConfig returns a net.ListenConfig with platform-specific options.
// The actual implementation varies by OS (see server_linux.go / server_other.go).
var newListenConfig = func() net.ListenConfig {
	return platformListenConfig()
}
