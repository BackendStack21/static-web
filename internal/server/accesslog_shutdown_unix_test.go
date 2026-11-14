//go:build unix

package server

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// syscall.Kill is Unix-only, so this test lives in its own build-tagged file.
func TestRunSignalHandler_SIGTERMFlushesBufferedAccessLog(t *testing.T) {
	// Keep SIGTERM from killing the test binary should it arrive before
	// RunSignalHandler has registered for it.
	guard := make(chan os.Signal, 16)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	s, cfg, logs := alStartLoggedServer(t)
	alRequestN(t, s, 4)

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunSignalHandler(context.Background(), s, nil, nil, &cfg)
	}()
	deadline := time.After(10 * time.Second)
	for stopped := false; !stopped; {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case <-done:
			stopped = true
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatal("RunSignalHandler did not return after SIGTERM")
		}
	}
	if got := len(alAccessLine.FindAllString(logs.String(), -1)); got != 4 {
		t.Errorf("after SIGTERM shutdown, %d access-log lines were written, want 4; log:\n%s", got, logs.String())
	}
}
