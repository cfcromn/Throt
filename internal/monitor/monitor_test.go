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

func TestProbeLatency(t *testing.T) {
	if l := ProbeLatency(context.Background(), time.Millisecond); l < 0 || l > time.Second {
		t.Fatalf("latency = %v", l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l := ProbeLatency(ctx, time.Hour); l != 0 {
		t.Fatalf("cancelled probe = %v, want 0", l)
	}
}
