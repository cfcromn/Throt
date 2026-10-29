package app

import (
	"slices"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// Lease waiters: `throt run` callers that were refused a lease and keep
// polling. They wait in one FIFO with the workloads the scheduler queued
// (scheduler.Policy.FirstInLine), so steady agent load cannot keep them out
// until their timeout runs them outside the limit.
const (
	// maxWaiters bounds the waiter table; callers beyond it are still
	// answered, but hold no place in line.
	maxWaiters = 256
	// waiterSilence drops a waiter that stopped polling: twice throt run's
	// 1s poll interval.
	waiterSilence = 2 * time.Second
)

// waiter is one lease waiter.
type waiter struct {
	key   domain.ProcessKey
	owner string
	since time.Time // first refused acquire
	seen  time.Time // latest acquire
}

// waitList is the waiter table. Like leaseTable it is guarded by d.mu.
type waitList struct {
	w []waiter
}

// touch records an acquire by key at now and returns when it first asked.
// A caller the full table cannot hold gets now, so it is behind everyone.
func (l *waitList) touch(key domain.ProcessKey, owner string, now time.Time) time.Time {
	for i := range l.w {
		if l.w[i].key == key {
			l.w[i].seen, l.w[i].owner = now, owner
			return l.w[i].since
		}
	}
	if len(l.w) < maxWaiters {
		l.w = append(l.w, waiter{key: key, owner: owner, since: now, seen: now})
	}
	return now
}

// since returns when key first asked, or now if it is not waiting.
func (l *waitList) since(key domain.ProcessKey, now time.Time) time.Time {
	for _, w := range l.w {
		if w.key == key {
			return w.since
		}
	}
	return now
}

func (l *waitList) remove(key domain.ProcessKey) {
	l.w = slices.DeleteFunc(l.w, func(w waiter) bool { return w.key == key })
}

// prune drops waiters silent for waiterSilence and, given a process sample
// that started at sampled, those whose process is gone.
func (l *waitList) prune(now, sampled time.Time, procs []domain.ProcessSample) {
	var alive map[domain.ProcessKey]bool
	if procs != nil {
		alive = make(map[domain.ProcessKey]bool, len(procs))
		for _, p := range procs {
			alive[p.Key()] = true
		}
	}
	l.w = slices.DeleteFunc(l.w, func(w waiter) bool {
		if now.Sub(w.seen) > waiterSilence {
			return true
		}
		return alive != nil && w.since.Before(sampled) && !alive[w.key]
	})
}

// line returns the waiters as scheduler line entries.
func (l *waitList) line() []scheduler.Waiter {
	out := make([]scheduler.Waiter, len(l.w))
	for i, w := range l.w {
		out[i] = scheduler.Waiter{Key: w.key, Owner: w.owner, Since: w.since, Lease: true}
	}
	return out
}

// busyByOwner counts running heavy workloads and leases per owner, the
// budget FirstInLine checks. Idle workloads do not count, as in assess.
func busyByOwner(st StatusResult) map[string]int {
	states := WorkloadStates(st)
	out := map[string]int{}
	for _, h := range st.Heavy {
		if states[h.Key] == StateRunning && !h.Idle {
			out[h.Owner]++
		}
	}
	for _, l := range st.Scheduler.Leases {
		out[l.Agent]++
	}
	return out
}
