package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// Exited makes world an ExitChecker.
func (w *world) Exited(ctx context.Context, k domain.ProcessKey) bool {
	gone, _ := w.Gone(ctx, k)
	return gone
}

// TestSampleLoopTicksEarlyOnExit checks that a tick returning a check makes
// the loop poll it and tick as soon as it reports an exit, and that a tick
// returning nil arms nothing: no poll, no tick before the interval.
func TestSampleLoopTicksEarlyOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ticks, polls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		sampleLoop(ctx, time.Hour, time.Millisecond, func(context.Context) func() bool {
			if ticks.Add(1) > 1 {
				return nil // nothing queued after the early tick
			}
			return func() bool { return polls.Add(1) >= 3 } // exits on the third poll
		})
	}()
	waitFor(t, "early tick", func() bool { return ticks.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if n, p := ticks.Load(), polls.Load(); n != 2 || p != 3 {
		t.Errorf("ticks = %d, polls = %d; want 2 and 3", n, p)
	}
	cancel()
	<-done
}

// TestSampleLoopNoPollAtShortInterval checks that the loop does not poll
// when the interval is no longer than the poll period.
func TestSampleLoopNoPollAtShortInterval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var polls atomic.Int32
	sampleLoop(ctx, 5*time.Millisecond, 10*time.Millisecond, func(context.Context) func() bool {
		return func() bool { polls.Add(1); return true }
	})
	if p := polls.Load(); p != 0 {
		t.Errorf("polls = %d, want 0", p)
	}
}

// TestRefillWatch checks that workloads are watched only in enforce mode
// and only while something waits for a slot. TestRefillOnExit covers a
// queued workload, which is not watched itself.
func TestRefillWatch(t *testing.T) {
	a := process.WorkloadView{Key: domain.ProcessKey{PID: 11, Start: time.Unix(11, 0)}, Name: "go", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10}
	for _, mode := range []string{"enforce", "observe"} {
		t.Run(mode, func(t *testing.T) {
			d := controlDaemon(t, t.TempDir(), &fakeSignaler{})
			c := config.Default().Scheduler
			c.Mode, c.HeavyLimit, c.Adaptive = mode, 1, false
			d.sched = newScheduler(c)
			now := time.Now()
			for _, heavy := range [][]process.WorkloadView{{a}, {a}} {
				now = now.Add(time.Second)
				d.schedule(context.Background(), scheduler.Input{Now: now, Pressure: domain.PressureNormal, Heavy: heavy}, nil)
			}
			if got := d.refillWatch([]process.WorkloadView{a}, 0); got != nil {
				t.Errorf("nothing queued: watch = %v, want nil", got)
			}
			var want []domain.ProcessKey
			if mode == "enforce" {
				want = []domain.ProcessKey{a.Key}
			}
			if got := d.refillWatch([]process.WorkloadView{a}, 1); !reflect.DeepEqual(got, want) {
				t.Errorf("lease waiter: watch = %v, want %v", got, want)
			}
		})
	}
}

// TestRefillOnExit checks the whole path: with a queued build, the exit of
// the running one resumes it at once instead of at the next sample, an
// hour away, and only once.
func TestRefillOnExit(t *testing.T) {
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
	)
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(t.TempDir(), "throtd.sock")
	cfg.SampleInterval = config.Duration(100 * time.Millisecond)
	cfg.Scheduler.Mode, cfg.Scheduler.HeavyLimit, cfg.Scheduler.Adaptive = "enforce", 1, false
	cfg.Scheduler.ResumeCooldown = 0
	d := NewDaemon(cfg, w, w, w, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Warm up by hand: the scheduler never queues while pressure is unknown.
	waitFor(t, "normal pressure", func() bool {
		d.tick(ctx)
		return d.engine.Level() == domain.PressureNormal
	})
	w.add(domain.ProcessSample{PID: 12, PPID: 10, Name: "go"})
	d.tick(ctx)
	if got := w.signals(); got != "stop:12" {
		t.Fatalf("signals = %q, want stop:12", got)
	}
	if want := []domain.ProcessKey{{PID: 11, Start: time.Unix(11, 0)}}; !reflect.DeepEqual(d.watch, want) {
		t.Fatalf("watch = %v, want %v", d.watch, want)
	}

	// finished counts ticks that returned: the exit must come after the
	// first tick's sample, or that tick sees it and resumes by itself.
	var ticks, finished atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		sampleLoop(ctx, time.Hour, time.Millisecond, func(ctx context.Context) func() bool {
			ticks.Add(1)
			defer finished.Add(1)
			return d.tickAndWatch(ctx)
		})
	}()
	waitFor(t, "first loop tick", func() bool { return finished.Load() == 1 })
	w.exit(11)
	waitFor(t, "pid 12 resumed", func() bool { return strings.HasSuffix(w.signals(), "cont:12") })
	cancel()
	<-done
	if n := ticks.Load(); n != 2 {
		t.Errorf("ticks = %d, want 2 (the first and the early one)", n)
	}
	if d.watch != nil {
		t.Errorf("watch after the queue emptied = %v, want nil", d.watch)
	}
	if n := countEvents(d, domain.EventSchedulerDecision, "resume go"); n != 1 {
		t.Errorf("%d resume decisions, want 1", n)
	}
}
