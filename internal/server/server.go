// Package server provides an HTTP/HTTPS server with graceful shutdown support.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/static-web/server/internal/config"
)

// Server wraps one HTTP and one optional HTTPS net/http.Server.
type Server struct {
	http  *http.Server
	https *http.Server // nil when TLS is not configured
}

// New creates a Server from the provided configuration and handler.
// HTTPS is only configured when both TLSCert and TLSKey are non-empty.
// When TLS is configured, the HTTP server is replaced with a redirect handler
// that sends all requests to the HTTPS address (SEC-004).
func New(cfg *config.ServerConfig, secCfg *config.SecurityConfig, handler http.Handler) *Server {
	s := &Server{}

	httpHandler := handler
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		// Replace plain-HTTP handler with a permanent redirect to HTTPS.
		tlsAddr := cfg.TLSAddr
		httpHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			// Strip any existing port from the host.
			if idx := strings.LastIndex(host, ":"); idx >= 0 {
				host = host[:idx]
			}
			// Append HTTPS port only when it differs from the default 443.
			target := "https://" + host
			if tlsAddr != ":443" && !strings.HasSuffix(tlsAddr, ":443") {
				target += tlsAddr
			}
			target += r.RequestURI
			http.Redirect(w, r, target, http.StatusMovedPermanently)
		})
	}

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpHandler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    8 * 1024,
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
			httpsHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Strict-Transport-Security", hstsValue)
				handler.ServeHTTP(w, r)
			})
		}

		s.https = &http.Server{
			Addr:              cfg.TLSAddr,
			Handler:           httpsHandler,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			MaxHeaderBytes:    8 * 1024,
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
		ln, err := lc.Listen(context.Background(), "tcp", s.http.Addr)
		if err != nil {
			errCh <- fmt.Errorf("server: HTTP listen on %s: %w", s.http.Addr, err)
			return
		}
		log.Printf("server: HTTP listening on %s", s.http.Addr)
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("server: HTTP serve: %w", err)
		}
	}()

	if s.https != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ln, err := lc.Listen(context.Background(), "tcp", s.https.Addr)
			if err != nil {
				errCh <- fmt.Errorf("server: HTTPS listen on %s: %w", s.https.Addr, err)
				return
			}
			log.Printf("server: HTTPS listening on %s", s.https.Addr)
			if err := s.https.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey); err != nil && err != http.ErrServerClosed {
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

	shutdown := func(srv *http.Server, name string) {
		defer wg.Done()
		if err := srv.Shutdown(ctx); err != nil {
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

// httpServer returns the internal HTTP server for testing purposes.
func (s *Server) httpServer() *http.Server {
	return s.http
}

// newListenConfig returns a net.ListenConfig with platform-specific options.
// The actual implementation varies by OS (see server_linux.go / server_other.go).
var newListenConfig = func() net.ListenConfig {
	return platformListenConfig()
}
