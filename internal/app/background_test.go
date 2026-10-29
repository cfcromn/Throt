package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// prioWorld is a world that can also lower and restore priority. It
// verifies identity like the platform does and records every call.
type prioWorld struct {
	*world
	calls []string
	// low holds the processes currently lowered.
	low map[domain.ProcessKey]bool
	// fail makes Background fail for these PIDs.
	fail map[int]bool
}

func newPrioWorld(procs ...domain.ProcessSample) *prioWorld {
	return &prioWorld{world: newWorld(procs...), low: map[domain.ProcessKey]bool{}, fail: map[int]bool{}}
}

func (w *prioWorld) verify(k domain.ProcessKey, uid uint32) error {
	p, ok := w.procs[k.PID]
	if !ok || !p.Start.Equal(k.Start) || p.UID != uid {
		return fmt.Errorf("pid %d: %w", k.PID, domain.ErrProcessGone)
	}
	return nil
}

func (w *prioWorld) Background(_ context.Context, k domain.ProcessKey, uid uint32) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.verify(k, uid); err != nil {
		return 0, err
	}
	if w.fail[k.PID] {
		return 0, errors.New("setpriority: operation not permitted")
	}
	w.calls = append(w.calls, fmt.Sprintf("bg:%d", k.PID))
	w.low[k] = true
	return uint64(k.PID), nil
}

func (w *prioWorld) Foreground(_ context.Context, k domain.ProcessKey, uid uint32, token uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.verify(k, uid); err != nil {
		return err
	}
	if token != uint64(k.PID) {
		return fmt.Errorf("pid %d: wrong token %d", k.PID, token)
	}
	w.calls = append(w.calls, fmt.Sprintf("fg:%d", k.PID))
	delete(w.low, k)
	return nil
}

func (w *prioWorld) log() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.calls, " ")
}

func (w *prioWorld) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = nil
}

// prioDaemon returns a daemon in enforce mode with a high heavy limit, so
// nothing queues, over w.
func prioDaemon(t *testing.T, w *prioWorld, mode string) *Daemon {
	return prioDaemonLimit(t, w, mode, 8)
}

func prioDaemonLimit(t *testing.T, w *prioWorld, mode string, limit int) *Daemon {
	t.Helper()
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(shortTempDir(t), "throtd.sock")
	cfg.SampleInterval = config.Duration(100 * time.Millisecond)
	cfg.Scheduler.Mode, cfg.Scheduler.HeavyLimit, cfg.Scheduler.Adaptive = mode, limit, false
	cfg.Scheduler.ResumeCooldown = 0
	d := NewDaemon(cfg, w, w, w, slog.New(slog.DiscardHandler))
	if d.pri == nil {
		t.Fatal("prioWorld is not a Prioritizer")
	}
	return d
}

func pkey(pid int) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: time.Unix(int64(pid), 0)}
}

func TestBackgroundLowersAttributedMembersOnce(t *testing.T) {
	w := newPrioWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
		domain.ProcessSample{PID: 12, PPID: 11, Name: "compile"},
		domain.ProcessSample{PID: 50, PPID: 1, Name: "make"}, // unattributed
		domain.ProcessSample{PID: 51, PPID: 50, Name: "cc1"},
	)
	d := prioDaemon(t, w, "enforce")
	ctx := context.Background()
	d.tick(ctx)
	if got := w.log(); got != "bg:11 bg:12" {
		t.Fatalf("calls = %q, want bg:11 bg:12", got)
	}
	if ev := eventsOf(d, domain.EventPriorityLowered); len(ev) != 1 || ev[0].Attrs["pid"] != "11" ||
		ev[0].Attrs["owner"] != "claude-1" || ev[0].Attrs["name"] != "go" || ev[0].Attrs["processes"] != "2" {
		t.Fatalf("priority_lowered events = %v", ev)
	}
	st := d.status
	for _, h := range st.Heavy {
		if want := h.Key.PID == 11; h.Background != want {
			t.Errorf("heavy %d background = %v, want %v", h.Key.PID, h.Background, want)
		}
	}

	// Lowered members are not lowered again; a member that appears later
	// is lowered on the next tick, without another event.
	w.reset()
	d.tick(ctx)
	if got := w.log(); got != "" {
		t.Fatalf("second tick calls = %q, want none", got)
	}
	w.add(domain.ProcessSample{PID: 13, PPID: 11, Name: "compile"})
	d.tick(ctx)
	if got := w.log(); got != "bg:13" {
		t.Fatalf("calls after a new member = %q, want bg:13", got)
	}
	if n := len(eventsOf(d, domain.EventPriorityLowered)); n != 1 {
		t.Fatalf("%d priority_lowered events, want 1", n)
	}

	// Exited members are forgotten, so a reused PID is a new process.
	w.exit(13)
	d.tick(ctx)
	if _, ok := d.prio.owner[pkey(13)]; ok {
		t.Fatal("an exited member is still tracked")
	}
}

func TestBackgroundSkipsHeldAndForeign(t *testing.T) {
	w := newPrioWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
		domain.ProcessSample{PID: 20, PPID: 10, Name: "cargo"},
		domain.ProcessSample{PID: 30, PPID: 10, Name: "make", State: domain.StateStopped}, // stopped by someone else
	)
	// A process of another user under the agent.
	w.add(domain.ProcessSample{PID: 12, PPID: 11, Name: "compile"})
	w.mu.Lock()
	p := w.procs[12]
	p.UID++
	w.procs[12] = p
	w.mu.Unlock()

	d := prioDaemon(t, w, "enforce")
	ctx := context.Background()
	d.tick(ctx)
	// The user pauses cargo by hand.
	if err := d.ctl.Pause(ctx, mustWorkload(t, d, pkey(20))); err != nil {
		t.Fatal(err)
	}
	w.reset()
	// Forget what the first tick lowered to see what a fresh round does.
	d.restorePriorities(ctx, RestoreDisabled)
	w.reset()
	d.tick(ctx)
	got := w.log()
	if got != "bg:11" {
		t.Fatalf("calls = %q, want only bg:11 (not the agent, the other user's process, the held or the stopped workload)", got)
	}
}

func mustWorkload(t *testing.T, d *Daemon, k domain.ProcessKey) process.WorkloadTarget {
	t.Helper()
	target, ok := d.workload(k)
	if !ok {
		t.Fatalf("no workload %v", k)
	}
	return target
}

func TestBackgroundRefusesReusedPID(t *testing.T) {
	w := newPrioWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
	)
	d := prioDaemon(t, w, "enforce")
	ctx := context.Background()
	d.tick(ctx)
	// Between the sample and the restore, pid 11 exits and a new process
	// takes its PID.
	w.mu.Lock()
	p := w.procs[11]
	p.Start = p.Start.Add(time.Second)
	w.procs[11] = p
	w.mu.Unlock()
	w.reset()
	d.restorePriorities(ctx, RestoreDisabled)
	if got := w.log(); got != "" {
		t.Fatalf("restore acted on a reused PID: %q", got)
	}
	if ev := eventsOf(d, domain.EventControlFailed); len(ev) != 0 {
		t.Fatalf("a gone process is not a failure: %v", ev)
	}
}

func TestBackgroundRestoresWhenLeavingEnforce(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(c *config.Config)
	}{
		{"mode", RestoreEnforceDisabled, func(c *config.Config) { c.Scheduler.Mode = "observe" }},
		{"disabled", RestoreDisabled, func(c *config.Config) { c.Scheduler.Background = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPrioWorld(
				domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
				domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
				domain.ProcessSample{PID: 12, PPID: 11, Name: "compile"},
			)
			d := prioDaemon(t, w, "enforce")
			path := filepath.Join(t.TempDir(), "config.json")
			d.SetConfigPath(path)
			writeConfig(t, path, d.live, func(*config.Config) {})
			ctx := context.Background()
			d.tick(ctx)
			w.reset()
			writeConfig(t, path, d.live, tc.change)
			if _, err := d.Reload(); err != nil {
				t.Fatal(err)
			}
			d.tick(ctx)
			if got := w.log(); got != "fg:11 fg:12" && got != "fg:12 fg:11" {
				t.Fatalf("calls = %q, want fg:11 and fg:12", got)
			}
			ev := eventsOf(d, domain.EventPriorityRestored)
			if len(ev) != 1 || ev[0].Attrs["reason"] != tc.reason || ev[0].Attrs["processes"] != "2" || ev[0].Attrs["pid"] != "11" {
				t.Fatalf("priority_restored events = %v", ev)
			}
			if len(w.low) != 0 || len(d.prio.workloads) != 0 {
				t.Fatalf("still lowered: %v, tracked %v", w.low, d.prio.workloads)
			}
			for _, h := range d.status.Heavy {
				if h.Background {
					t.Fatalf("status still shows %d in the background", h.Key.PID)
				}
			}
			// Nothing is lowered again while inactive.
			w.reset()
			d.tick(ctx)
			if got := w.log(); got != "" {
				t.Fatalf("inactive tick calls = %q", got)
			}
		})
	}
}

// writeConfig writes base, changed by change, to path.
func writeConfig(t *testing.T, path string, base config.Config, change func(*config.Config)) {
	t.Helper()
	c := base
	change(&c)
	b := fmt.Sprintf(`{"socket_path": %q, "sample_interval": %q, "scheduler": {"mode": %q, "heavy_limit": %d, "adaptive": false, "background": %v}}`,
		c.SocketPath, time.Duration(c.SampleInterval), c.Scheduler.Mode, c.Scheduler.HeavyLimit, c.Scheduler.Background)
	if err := os.WriteFile(path, []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundObserveNeverLowers(t *testing.T) {
	w := newPrioWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
	)
	for _, mode := range []string{"off", "observe"} {
		d := prioDaemon(t, w, mode)
		d.tick(context.Background())
		if got := w.log(); got != "" {
			t.Fatalf("%s mode calls = %q", mode, got)
		}
	}
}

func TestBackgroundErrorsDoNotBreakScheduling(t *testing.T) {
	w := newPrioWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
		domain.ProcessSample{PID: 12, PPID: 11, Name: "compile"},
	)
	w.fail[11] = true
	d := prioDaemonLimit(t, w, "enforce", 1)
	ctx := context.Background()
	waitFor(t, "normal pressure", func() bool {
		d.tick(ctx)
		return d.engine.Level() == domain.PressureNormal
	})
	if got := w.log(); got != "bg:12" {
		t.Fatalf("calls = %q, want bg:12 (11 failed, and is not retried)", got)
	}
	if ev := eventsOf(d, domain.EventControlFailed); len(ev) != 1 || ev[0].Attrs["action"] != "background" {
		t.Fatalf("control_failed events = %v, want one background failure", ev)
	}
	// The scheduler still queues a second build.
	w.add(domain.ProcessSample{PID: 20, PPID: 10, Name: "go"})
	d.tick(ctx)
	if q := d.schedulerQueued(); !slices.Contains(q, pkey(20)) {
		t.Fatalf("queued = %v, want pid 20", q)
	}
	if got := w.signals(); got != "stop:20" {
		t.Fatalf("signals = %q, want stop:20", got)
	}
	// A queued workload is lowered too, and stays lowered when resumed.
	if !w.low[pkey(20)] {
		t.Fatal("queued workload not lowered")
	}
	if err := d.ctl.Resume(ctx, pkey(20)); err != nil && !errors.As(err, new(*control.RefusedError)) {
		t.Fatal(err)
	}
	if !w.low[pkey(20)] {
		t.Fatal("resume restored priority")
	}
}

func TestBackgroundRestoredOnShutdown(t *testing.T) {
	w := newPrioWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
	)
	d := prioDaemon(t, w, "enforce")
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, "lowered", func() bool { return strings.Contains(w.log(), "bg:11") })
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := w.log(); got != "bg:11 fg:11" {
		t.Fatalf("calls = %q, want bg:11 fg:11", got)
	}
	ev := eventsOf(d, domain.EventPriorityRestored)
	if len(ev) != 1 || ev[0].Attrs["reason"] != RestoreShutdown {
		t.Fatalf("priority_restored events = %v", ev)
	}
}
