// Package monitor runs periodic sampling on behalf of the daemon.
package monitor

import (
	"context"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// HostSampler reads host-wide metrics.
type HostSampler interface {
	SampleHost(ctx context.Context) (domain.HostSample, error)
}

// Run samples every interval until ctx is cancelled, calling onSample with
// each result or onError with each failure. It runs in the caller's
// goroutine; callbacks must not block for long, since a slow callback delays
// (rather than queues) the next sample.
func Run(ctx context.Context, interval time.Duration, s HostSampler, onSample func(domain.HostSample), onError func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if sample, err := s.SampleHost(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			onError(err)
		} else {
			onSample(sample)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
