// Package monitor runs periodic sampling on behalf of the daemon.
package monitor

import (
	"context"
	"time"
)

// Run calls tick immediately and then every interval until ctx is
// cancelled. It runs in the caller's goroutine. A slow tick delays (rather
// than queues) the next one, so work never piles up.
func Run(ctx context.Context, interval time.Duration, tick func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ProbeLatency measures how late a timer of duration d fires. When the
// machine's CPUs are saturated, runnable threads wait to be scheduled and
// the lateness grows, so it is a cheap proxy for foreground responsiveness.
// ok is false if ctx ended first, so callers can mark the metric missing.
// Inside a CPU-quota cgroup, the daemon's own throttling also shows up as
// lateness.
func ProbeLatency(ctx context.Context, d time.Duration) (latency time.Duration, ok bool) {
	start := time.Now()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return 0, false
	case <-t.C:
		return max(time.Since(start)-d, 0), true
	}
}
