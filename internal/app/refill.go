package app

import (
	"context"
	"time"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// ExitChecker is a liveness check cheap enough to poll: it never runs a
// command, and reports true only when the OS confirms that key's process
// is gone. The platform process controllers implement it.
type ExitChecker interface {
	Exited(ctx context.Context, key domain.ProcessKey) bool
}

// refillCheck is how often, while work waits for a heavy slot, the
// sampling goroutine checks whether a running heavy workload exited.
const refillCheck = 250 * time.Millisecond

// sampleLoop calls tick at once and then every interval. When a tick
// returns a non-nil exited, it also calls exited every check until the next
// tick, and ticks early, restarting the interval, as soon as exited reports
// true. A tick that returns nil arms no timer, and everything runs on the
// caller's goroutine.
func sampleLoop(ctx context.Context, interval, check time.Duration, tick func(context.Context) (exited func() bool)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var poll *time.Timer
	defer func() {
		if poll != nil {
			poll.Stop()
		}
	}()
	for {
		exited := tick(ctx)
		if check >= interval {
			exited = nil // the next tick comes first anyway
		}
	wait:
		for {
			var due <-chan time.Time
			if exited != nil {
				if poll == nil {
					poll = time.NewTimer(check)
				} else {
					poll.Reset(check)
				}
				due = poll.C
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				break wait
			case <-due:
				if exited() {
					t.Reset(interval)
					break wait
				}
			}
		}
		if poll != nil {
			poll.Stop()
		}
	}
}

// tickAndWatch runs one tick and returns the check sampleLoop polls until
// the next: whether a heavy workload that was running at the end of the
// tick has exited, or nil when there is nothing to watch.
func (d *Daemon) tickAndWatch(ctx context.Context) func() bool {
	d.tick(ctx)
	if d.exits == nil || len(d.watch) == 0 {
		return nil
	}
	watch := d.watch
	return func() bool {
		for _, k := range watch {
			if d.exits.Exited(ctx, k) {
				return true
			}
		}
		return false
	}
}

// refillWatch returns the heavy workloads whose exit would free a slot for
// waiting work: those running, when the scheduler enforces and it holds a
// queued workload or a lease waiter is waiting. Otherwise it returns nil,
// and nothing is polled. An early tick is a full sample, so every
// scheduler rule applies to it as to any other, and an exit is credited
// once, when the sample first misses the process.
func (d *Daemon) refillWatch(heavy []process.WorkloadView, waiters int) []domain.ProcessKey {
	if d.sched.Policy().Mode != scheduler.ModeEnforce {
		return nil
	}
	paused := d.ctl.Paused()
	queued := waiters > 0
	stopped := make(map[domain.ProcessKey]bool, len(paused))
	for _, p := range paused {
		stopped[p.Key] = true
		queued = queued || p.By == control.OriginScheduler
	}
	if !queued {
		return nil
	}
	var watch []domain.ProcessKey
	for _, h := range heavy {
		if !stopped[h.Key] {
			watch = append(watch, h.Key)
		}
	}
	return watch
}
