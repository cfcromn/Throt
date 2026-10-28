package monitor

import (
	"context"
	"testing"
	"time"
)

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, time.Millisecond, func(context.Context) {
			ticks++
			if ticks == 3 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if ticks != 3 {
		t.Fatalf("ticks = %d, want 3", ticks)
	}
}
