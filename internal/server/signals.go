package server

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/static-web/server/internal/cache"
	"github.com/static-web/server/internal/config"
)

// RunSignalHandler blocks until SIGTERM or SIGINT is received, then gracefully
// shuts down srv. It also handles SIGHUP to reload configuration and flush cache.
//
// Parameters:
//   - ctx: parent context (used to derive a shutdown context)
//   - srv: the running Server
//   - c: the cache to flush on SIGHUP
//   - cfgPath: path to the TOML config file (used for SIGHUP reload)
//   - cfgPtr: pointer to the current config pointer, updated on reload
func RunSignalHandler(ctx context.Context, srv *Server, c *cache.Cache, cfgPath string, cfgPtr **config.Config) {
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
				handleHUP(c, cfgPath, cfgPtr)

			case syscall.SIGTERM, syscall.SIGINT:
				log.Printf("signal: received %s, initiating graceful shutdown", sig)
				handleShutdown(ctx, srv, *cfgPtr)
				return
			}
		}
	}
}

// handleHUP reloads the config file and flushes the cache.
func handleHUP(c *cache.Cache, cfgPath string, cfgPtr **config.Config) {
	log.Printf("signal: SIGHUP received — reloading config from %q", cfgPath)

	newCfg, err := config.Load(cfgPath)
	if err != nil {
		log.Printf("signal: config reload failed: %v (keeping existing config)", err)
		return
	}

	*cfgPtr = newCfg
	c.Flush()
	log.Printf("signal: config reloaded and cache flushed successfully")
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
