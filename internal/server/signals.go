package server

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
	"github.com/BackendStack21/static-web/internal/security"
)

// ReloadFunc produces a fresh configuration on SIGHUP. Callers should build it
// the same way as at startup (file + env + CLI flag overrides + Validate) so
// that a reload never silently drops command-line settings (OPS-009).
type ReloadFunc func() (*config.Config, error)

// RunSignalHandler blocks until SIGTERM or SIGINT is received, then gracefully
// shuts down srv. It returns early, without shutting down, when ctx is
// cancelled (e.g. because a serve loop failed); the caller is then responsible
// for shutdown.
//
// On SIGHUP it calls reload, stores the result in *cfgPtr and flushes the file
// cache and path cache. The running handler chain and listeners keep the
// configuration they were built with, so only settings read through *cfgPtr
// (currently the shutdown timeout) change; everything else needs a restart.
// A failed reload keeps the existing configuration and does not flush caches.
//
// Parameters:
//   - ctx: parent context (used to derive a shutdown context)
//   - srv: the running Server
//   - c: the cache to flush on SIGHUP (may be nil when caching is disabled)
//   - reload: builds the new config on SIGHUP (may be nil to only flush caches)
//   - cfgPtr: pointer to the current config pointer, updated on reload
//   - pc: optional path cache to flush on SIGHUP (may be nil)
func RunSignalHandler(ctx context.Context, srv *Server, c *cache.Cache, reload ReloadFunc, cfgPtr **config.Config, pc ...*security.PathCache) {
	var pathCache *security.PathCache
	if len(pc) > 0 {
		pathCache = pc[0]
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-ctx.Done():
			return

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				handleHUP(c, pathCache, reload, cfgPtr)

			case syscall.SIGTERM, syscall.SIGINT:
				log.Printf("signal: received %s, initiating graceful shutdown", sig)
				handleShutdown(ctx, srv, *cfgPtr)
				return
			}
		}
	}
}

// handleHUP re-runs reload and flushes both file and path caches.
func handleHUP(c *cache.Cache, pc *security.PathCache, reload ReloadFunc, cfgPtr **config.Config) {
	log.Printf("signal: SIGHUP received — re-reading config and flushing caches")

	if reload != nil {
		newCfg, err := reload()
		if err != nil {
			log.Printf("signal: config reload failed: %v (keeping existing config, caches not flushed)", err)
			return
		}
		*cfgPtr = newCfg
	}

	if c != nil {
		c.Flush()
	}
	if pc != nil {
		pc.Flush()
	}
	log.Printf("signal: caches flushed; config re-read (listeners and handler settings are unchanged until restart)")
}

// handleShutdown performs a graceful shutdown with a timeout derived from config.
func handleShutdown(ctx context.Context, srv *Server, cfg *config.Config) {
	timeout := cfg.Server.ShutdownTimeout
	if timeout <= 0 {
		timeout = 15e9 // 15 seconds fallback
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Printf("signal: waiting up to %v for connections to drain", timeout)

	err := srv.Shutdown(shutdownCtx)
	if shutdownCtx.Err() == context.DeadlineExceeded {
		log.Printf("signal: shutdown timed out after %v — forcing close", timeout)
	} else if err != nil {
		log.Printf("signal: shutdown error: %v", err)
	} else {
		log.Printf("signal: graceful shutdown complete")
	}
}
