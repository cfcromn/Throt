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
