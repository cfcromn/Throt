package monitor

import (
	"context"
	"testing"
	"time"
)

func TestProbeLatency(t *testing.T) {
	if l, ok := ProbeLatency(context.Background(), time.Millisecond); !ok || l < 0 || l > time.Second {
		t.Fatalf("latency = %v, %v", l, ok)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l, ok := ProbeLatency(ctx, time.Hour); ok || l != 0 {
		t.Fatalf("cancelled probe = %v, %v; want 0, false", l, ok)
	}
}
