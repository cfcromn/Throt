package main

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestReloadOnHangup checks that each SIGHUP reloads the config, a real
// one included, and that the loop stops with its context.
func TestReloadOnHangup(t *testing.T) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	var reloads atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		reloadOnHangup(ctx, hup, func() { reloads.Add(1) })
		close(done)
	}()
	waitFor := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for reloads.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d reloads, want %d", reloads.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(1)
	hup <- syscall.SIGHUP
	waitFor(2)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop on cancel")
	}
	if n := reloads.Load(); n != 2 {
		t.Errorf("%d reloads, want 2", n)
	}
}
