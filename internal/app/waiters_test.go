package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

func TestWaitListBoundAndExpiry(t *testing.T) {
	t0 := time.Unix(1000, 0)
	k := func(pid int) domain.ProcessKey { return domain.ProcessKey{PID: pid, Start: time.Unix(int64(pid), 0)} }
	var l waitList
	for i := range maxWaiters {
		l.touch(k(100+i), "", t0)
	}
	// Beyond the bound a caller is answered as if it just arrived, and not
	// recorded.
	if since := l.touch(k(9999), "", t0.Add(time.Second)); !since.Equal(t0.Add(time.Second)) || len(l.w) != maxWaiters {
		t.Fatalf("over the bound: since %v, %d waiters", since, len(l.w))
	}
	// A repeated touch keeps the first-asked time.
	if since := l.touch(k(100), "a", t0.Add(time.Second)); !since.Equal(t0) {
		t.Fatalf("re-touch since = %v, want %v", since, t0)
	}
	// Silence beyond waiterSilence drops everyone but the waiter that
	// polled again.
	l.prune(t0.Add(time.Second+waiterSilence), t0, nil)
	if len(l.w) != 1 || l.w[0].key != k(100) || l.w[0].owner != "a" {
		t.Fatalf("after silence: %+v", l.w)
	}
	// A waiter missing from a sample taken after it asked has exited; one
	// that asked after the sample started may just not be in it yet.
	l.touch(k(200), "", t0.Add(2*time.Second))
	l.prune(t0.Add(2*time.Second), t0.Add(1500*time.Millisecond), []domain.ProcessSample{{PID: 1}})
	if len(l.w) != 1 || l.w[0].key != k(200) {
		t.Fatalf("after exit: %+v", l.w)
	}
	alive := domain.ProcessSample{PID: 200, Start: time.Unix(200, 0)}
	l.prune(t0.Add(3*time.Second), t0.Add(2500*time.Millisecond), []domain.ProcessSample{alive})
	if len(l.w) != 1 {
		t.Fatalf("live waiter dropped: %+v", l.w)
	}
	l.remove(k(200))
	if len(l.w) != 0 {
		t.Fatalf("after remove: %+v", l.w)
	}
}

func TestAcquireWaitsInLine(t *testing.T) {
	// One of two slots is free, and the scheduler has queued workload 21
	// since t=-10s.
	running := process.WorkloadView{Key: domain.ProcessKey{PID: 11}, Name: "go", Owner: "claude-1"}
	queued := process.WorkloadView{Key: domain.ProcessKey{PID: 21}, Name: "go", Owner: "codex-1"}
	st := oneSlot(2, running, queued)
	st.Scheduler.Queued = []domain.ProcessKey{queued.Key}
	d, _ := leaseDaemon(t, st)
	d.queuedLine = []scheduler.Waiter{{Key: queued.Key, Owner: "codex-1", Since: time.Now().Add(-10 * time.Second)}}

	// The newcomer is behind the queued workload, so it is refused and
	// remembered.
	r, err := acquireAs(t, d, context.Background(), AcquireParams{PID: 100})
	if err != nil || r.Granted || !strings.Contains(r.Reason, "older heavy work is waiting") {
		t.Fatalf("acquire behind queued work = %+v, %v", r, err)
	}
	if len(d.waiting.w) != 1 {
		t.Fatalf("waiters = %+v", d.waiting.w)
	}
	// It has now waited longer than the queued workload: its turn.
	d.queuedLine[0].Since = time.Now()
	r, err = acquireAs(t, d, context.Background(), AcquireParams{PID: 100})
	if err != nil || !r.Granted || r.LeaseID == "" {
		t.Fatalf("acquire first in line = %+v, %v", r, err)
	}
	if len(d.waiting.w) != 0 {
		t.Fatalf("granted caller still waiting: %+v", d.waiting.w)
	}
	// With the scheduler off there is no line.
	d, _ = leaseDaemon(t, st)
	d.status.Scheduler.Mode = scheduler.ModeOff
	d.queuedLine = []scheduler.Waiter{{Key: queued.Key, Owner: "codex-1", Since: time.Now().Add(-10 * time.Second)}}
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 100}); !r.Granted {
		t.Fatalf("acquire with the scheduler off = %+v", r)
	}
}
