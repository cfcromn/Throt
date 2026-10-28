package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
	"github.com/cfcromn/Throt/internal/transport"
)

// world is a mutable fake host: it samples, signals, and confirms exits
// against one process table.
type world struct {
	mu    sync.Mutex
	procs map[int]domain.ProcessSample
	sent  []string
}

func newWorld(procs ...domain.ProcessSample) *world {
	w := &world{procs: map[int]domain.ProcessSample{}}
	for _, p := range procs {
		w.add(p)
	}
	return w
}

func (w *world) add(p domain.ProcessSample) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p.UID, p.Start = uint32(os.Getuid()), time.Unix(int64(p.PID), 0)
	w.procs[p.PID] = p
}

func (w *world) exit(pid int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.procs, pid)
}

func (w *world) SampleHost(context.Context) (domain.HostSample, error) {
	return domain.HostSample{Time: time.Now(), MemTotal: 100, MemAvailable: 80}, nil
}

func (w *world) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]domain.ProcessSample, 0, len(w.procs))
	for _, p := range w.procs {
		out = append(out, p)
	}
	return out, nil
}

func (w *world) signal(op string, k domain.ProcessKey, uid uint32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[k.PID]
	if !ok || !p.Start.Equal(k.Start) || p.UID != uid {
		return domain.ErrProcessGone
	}
	w.sent = append(w.sent, fmt.Sprintf("%s:%d", op, k.PID))
	switch {
	case op == "stop":
		p.State = domain.StateStopped
	case p.State == domain.StateStopped:
		p.State = domain.StateRunning
	}
	w.procs[k.PID] = p
	return nil
}

func (w *world) Stop(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return w.signal("stop", k, uid)
}

func (w *world) Continue(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return w.signal("cont", k, uid)
}

func (w *world) Identify(_ context.Context, pid int) (domain.ProcessSample, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[pid]
	if !ok {
		return domain.ProcessSample{}, domain.ErrProcessGone
	}
	return p, nil
}

func (w *world) Gone(_ context.Context, k domain.ProcessKey) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[k.PID]
	return !ok || !p.Start.Equal(k.Start), nil
}

func (w *world) signals() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.sent, " ")
}

// debugEvents, when set by a test, dumps daemon events on a waitFor timeout.
var debugEvents func() []domain.Event

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			if debugEvents != nil {
				for _, e := range debugEvents() {
					t.Log(e.Kind, e.Message)
				}
			}
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSchedulerQueuesAndResumes(t *testing.T) {
	for _, mode := range []string{"enforce", "observe"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(
				domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
				domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
			)
			cfg := config.Default()
			cfg.SocketPath = filepath.Join(shortTempDir(t), "throtd.sock")
			cfg.SampleInterval = config.Duration(100 * time.Millisecond)
			cfg.Scheduler.Mode, cfg.Scheduler.HeavyLimit, cfg.Scheduler.ResumeCooldown = mode, 1, 0
			ctx, stop := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- NewDaemon(cfg, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
			}()
			defer func() { stop(); <-done }()

			debugEvents = func() []domain.Event {
				var events []domain.Event
				transport.Call(context.Background(), cfg.SocketPath, "events", nil, &events)
				return events
			}
			defer func() { debugEvents = nil }()
			status := func() StatusResult {
				var st StatusResult
				transport.Call(context.Background(), cfg.SocketPath, "status", nil, &st)
				return st
			}
			// The scheduler never queues while pressure is unknown (warm-up).
			waitFor(t, "normal pressure", func() bool {
				st := status()
				return len(st.Heavy) == 1 && st.Pressure == domain.PressureNormal
			})

			// A second build (with a compiler child) starts while the only slot is taken.
			w.add(domain.ProcessSample{PID: 12, PPID: 10, Name: "go"})
			w.add(domain.ProcessSample{PID: 13, PPID: 12, Name: "cc1"})
			if mode == "observe" {
				waitFor(t, "observed decision", func() bool {
					var events []domain.Event
					transport.Call(context.Background(), cfg.SocketPath, "events", nil, &events)
					for _, e := range events {
						if e.Kind == domain.EventSchedulerDecision && e.Attrs["observed"] == "true" && e.Attrs["pid"] == "12" {
							return true
						}
					}
					return false
				})
				if got := w.signals(); got != "" {
					t.Fatalf("observe mode sent signals: %q", got)
				}
				return
			}
			waitFor(t, "pid 12 queued", func() bool {
				q := status().Scheduler.Queued
				return len(q) == 1 && q[0].PID == 12
			})
			if got := w.signals(); got != "stop:12 stop:13" {
				t.Fatalf("signals = %q, want stop:12 stop:13", got)
			}

			// The queued build's top process is killed while stopped; its child
			// must not stay stopped forever.
			w.exit(12)
			waitFor(t, "orphaned child resumed", func() bool { return strings.HasPrefix(w.signals(), "stop:12 stop:13 cont:13") })
			w.exit(13) // keep the rest of the scenario to one queued workload
			waitFor(t, "queue empty", func() bool { return len(status().Scheduler.Queued) == 0 })

			// Another build queues; the running build finishes, so it gets the slot.
			w.add(domain.ProcessSample{PID: 14, PPID: 10, Name: "go"})
			waitFor(t, "pid 14 queued", func() bool {
				q := status().Scheduler.Queued
				return len(q) == 1 && q[0].PID == 14
			})
			w.exit(11)
			waitFor(t, "pid 14 resumed", func() bool { return len(status().Scheduler.Queued) == 0 })
			if got := w.signals(); !strings.HasSuffix(got, "stop:14 cont:14") {
				t.Fatalf("signals = %q, want ... stop:14 cont:14", got)
			}
		})
	}
}

func TestDaemonStats(t *testing.T) {
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude", RSS: 1 << 20},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go", RSS: 4 << 20},
		domain.ProcessSample{PID: 50, PPID: 1, Name: "make"},
	)
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(shortTempDir(t), "throtd.sock")
	cfg.SampleInterval = config.Duration(50 * time.Millisecond)
	cfg.Scheduler.Mode, cfg.Scheduler.HeavyLimit, cfg.Scheduler.ResumeCooldown = "enforce", 1, 0
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewDaemon(cfg, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()
	defer func() { stop(); <-done }()

	get := func() StatsResult {
		var r StatsResult
		transport.Call(context.Background(), cfg.SocketPath, "stats", nil, &r)
		return r
	}
	// The scheduler never queues while pressure is unknown (warm-up).
	waitFor(t, "normal pressure", func() bool {
		var st StatusResult
		transport.Call(context.Background(), cfg.SocketPath, "status", nil, &st)
		return len(st.Heavy) == 2 && st.Pressure == domain.PressureNormal && len(get().Agents) == 1
	})
	// The agent's build burns CPU time, and a second build arrives while
	// every slot is taken, so it queues.
	w.add(domain.ProcessSample{PID: 12, PPID: 10, Name: "go"})
	var cpu time.Duration
	waitFor(t, "cpu and queue time accounted", func() bool {
		cpu += 100 * time.Millisecond
		w.add(domain.ProcessSample{PID: 11, PPID: 10, Name: "go", RSS: 4 << 20, CPUTime: cpu})
		r := get()
		return len(r.Agents) == 1 && r.Agents[0].CPUSeconds > 0 && r.Agents[0].QueuedSeconds > 0
	})
	r := get()
	a := r.Agents[0]
	if a.Agent != "claude-1" || a.Process.PID != 10 || a.HeavyStarted != 2 || a.PeakRSS != 5<<20 || r.Since.IsZero() {
		t.Fatalf("row = %+v, since %v", a, r.Since)
	}
	if r.Total.HeavyStarted != 2 || r.Total.QueuedSeconds != a.QueuedSeconds || r.Unattributed.HeavyStarted != 1 {
		t.Fatalf("total = %+v, unattributed = %+v", r.Total, r.Unattributed)
	}
}

// TestObserveWouldQueue checks that observe mode exposes the workloads the
// last round would have queued, and that enforce mode never does.
func TestObserveWouldQueue(t *testing.T) {
	a := process.WorkloadView{Key: domain.ProcessKey{PID: 11, Start: time.Unix(11, 0)}, Name: "go", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10}
	b := process.WorkloadView{Key: domain.ProcessKey{PID: 21, Start: time.Unix(21, 0)}, Name: "make", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10}
	d := controlDaemon(t, t.TempDir(), &fakeSignaler{})
	c := config.Default().Scheduler
	c.Mode, c.HeavyLimit, c.Adaptive = "observe", 1, false
	d.sched = newScheduler(c)
	now := time.Now()
	round := func(heavy ...process.WorkloadView) []domain.ProcessKey {
		now = now.Add(time.Second)
		d.schedule(context.Background(), scheduler.Input{Now: now, Pressure: domain.PressureNormal, Heavy: heavy}, nil)
		return d.schedulerStatus().WouldQueue
	}
	if got := round(a); len(got) != 0 {
		t.Fatalf("first round would queue %v, want nothing", got)
	}
	if got := round(a, b); !reflect.DeepEqual(got, []domain.ProcessKey{b.Key}) {
		t.Fatalf("would queue %v, want %v", got, b.Key)
	}
	if got := round(a, b); got == nil || len(got) != 0 {
		t.Fatalf("next round would queue %v, want an empty list", got)
	}
	if st := d.schedulerStatus(); len(st.Queued) != 0 {
		t.Errorf("observe mode queued %v", st.Queued)
	}
}

// failingProcs is a process sampler that always fails.
type failingProcs struct{}

func (failingProcs) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	return nil, errors.New("ps: fork: resource temporarily unavailable")
}

// TestWouldQueueClearedOnFailedSample checks that a tick whose process
// sample fails, and so skips the scheduler, does not keep showing the
// previous round's would_queue.
func TestWouldQueueClearedOnFailedSample(t *testing.T) {
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(t.TempDir(), "throtd.sock")
	cfg.Scheduler.Mode, cfg.Scheduler.HeavyLimit, cfg.Scheduler.Adaptive = "observe", 1, false
	d := NewDaemon(cfg, fakeSampler{}, failingProcs{}, &fakeSignaler{}, slog.New(slog.DiscardHandler))
	a := process.WorkloadView{Key: domain.ProcessKey{PID: 11, Start: time.Unix(11, 0)}, Name: "go", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10}
	b := process.WorkloadView{Key: domain.ProcessKey{PID: 21, Start: time.Unix(21, 0)}, Name: "make", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10}
	now := time.Now()
	for _, heavy := range [][]process.WorkloadView{{a}, {a, b}} {
		now = now.Add(time.Second)
		d.schedule(context.Background(), scheduler.Input{Now: now, Pressure: domain.PressureNormal, Heavy: heavy}, nil)
	}
	if got := d.schedulerStatus().WouldQueue; len(got) != 1 {
		t.Fatalf("would queue %v, want one workload", got)
	}
	d.tick(context.Background())
	d.mu.Lock()
	got := d.status.Scheduler.WouldQueue
	d.mu.Unlock()
	if len(got) != 0 {
		t.Errorf("would queue after a failed process sample = %v, want empty", got)
	}
}
