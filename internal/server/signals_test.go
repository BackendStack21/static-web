package server

import (
	"errors"
	"testing"
	"time"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/config"
)

// OPS-009: SIGHUP uses the caller-supplied reload func (which re-applies CLI
// overrides) and swaps the config pointer.
func TestHandleHUP_UsesReloadFunc(t *testing.T) {
	old := &config.Config{}
	old.Files.Root = "/from/cli"
	cfg := old

	reloaded := &config.Config{}
	reloaded.Files.Root = "/from/cli"
	reloaded.Server.ShutdownTimeout = 3 * time.Second

	c := cache.NewCache(1<<20, 0)
	c.Put("/a", &cache.CachedFile{Data: []byte("x")})

	calls := 0
	handleHUP(c, nil, func() (*config.Config, error) {
		calls++
		return reloaded, nil
	}, &cfg)

	if calls != 1 {
		t.Fatalf("reload called %d times, want 1", calls)
	}
	if cfg != reloaded {
		t.Error("config pointer not updated after successful reload")
	}
	if _, ok := c.Get("/a"); ok {
		t.Error("cache not flushed after successful reload")
	}
}

func TestHandleHUP_FailedReloadKeepsConfig(t *testing.T) {
	old := &config.Config{}
	cfg := old
	c := cache.NewCache(1<<20, 0)
	c.Put("/a", &cache.CachedFile{Data: []byte("x")})

	handleHUP(c, nil, func() (*config.Config, error) {
		return nil, errors.New("boom")
	}, &cfg)

	if cfg != old {
		t.Error("config pointer replaced despite reload failure")
	}
	if _, ok := c.Get("/a"); !ok {
		t.Error("cache flushed despite reload failure")
	}
}

// A nil cache (cache disabled via --no-cache) must not panic on SIGHUP.
func TestHandleHUP_NilCache(t *testing.T) {
	cfg := &config.Config{}
	handleHUP(nil, nil, nil, &cfg)
}
