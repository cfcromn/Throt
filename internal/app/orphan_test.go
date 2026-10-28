package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/transport"
)

// TestDaemonOrphans covers stopped processes throtd did not pause: they are
// reported in status, an attributed heavy one gets one orphan_stopped
// event, the scheduler leaves them alone, and resume continues one only
// when it is an attributed heavy workload or --force is given.
func TestDaemonOrphans(t *testing.T) {
	stopped := domain.StateStopped
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "bash"},
		domain.ProcessSample{PID: 12, PPID: 11, Name: "make", State: stopped}, // left by an earlier throtd
		domain.ProcessSample{PID: 13, PPID: 12, Name: "cc1", State: stopped},
		domain.ProcessSample{PID: 20, PPID: 1, Name: "zsh"},
		domain.ProcessSample{PID: 21, PPID: 20, Name: "make", State: stopped}, // Ctrl-Z in a terminal
	)
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(shortTempDir(t), "throtd.sock")
	cfg.SampleInterval = config.Duration(20 * time.Millisecond)
	// Enforce with one slot: the scheduler must not queue the orphans (and
	// so later resume them) although two heavy workloads exceed it.
	cfg.Scheduler.Mode, cfg.Scheduler.HeavyLimit, cfg.Scheduler.ResumeCooldown = "enforce", 1, 0
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewDaemon(cfg, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()
	defer func() { stop(); <-done }()
	call := func(method string, params, result any) error {
		return transport.Call(context.Background(), cfg.SocketPath, method, params, result)
	}
	status := func() StatusResult {
		var st StatusResult
		call("status", nil, &st)
		return st
	}
	events := func(kind domain.EventKind) []domain.Event {
		var all, out []domain.Event
		call("events", nil, &all)
		for _, e := range all {
			if e.Kind == kind {
				out = append(out, e)
			}
		}
		return out
	}

	waitFor(t, "three orphans and an event", func() bool {
		return status().OrphanCount == 3 && len(events(domain.EventOrphanStopped)) == 1
	})
	st := status()
	kinds := map[int]process.OrphanKind{}
	for _, o := range st.Orphans {
		kinds[o.Key.PID] = o.Kind
	}
	if kinds[12] != process.OrphanLeftPaused || kinds[13] != process.OrphanLeftPaused || kinds[21] != process.OrphanJobControl {
		t.Fatalf("orphans = %+v", st.Orphans)
	}
	if ev := events(domain.EventOrphanStopped)[0]; ev.Attrs["pids"] != "12,13" {
		t.Errorf("orphan_stopped attrs = %v, want pids 12,13 (not the shell job)", ev.Attrs)
	}
	// Later ticks do not repeat the event.
	time.Sleep(100 * time.Millisecond)
	if n := len(events(domain.EventOrphanStopped)); n != 1 {
		t.Errorf("%d orphan_stopped events, want 1", n)
	}

	// A shell job is refused without --force; nothing is signaled.
	if err := call("resume", ControlParams{PID: 21}, nil); err == nil || !strings.Contains(err.Error(), "Ctrl-Z") {
		t.Fatalf("resume shell job: err = %v, want a Ctrl-Z refusal", err)
	}
	// A PID that is neither paused nor stopped is refused.
	if err := call("resume", ControlParams{PID: 10}, nil); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Fatalf("resume running agent: err = %v", err)
	}
	if got := w.signals(); got != "" {
		t.Fatalf("signals = %q, want none (no auto-resume, no queueing)", got)
	}

	// The attributed workload resumes, children first.
	if err := call("resume", ControlParams{PID: 12}, nil); err != nil {
		t.Fatal(err)
	}
	if got := w.signals(); got != "cont:13 cont:12" {
		t.Fatalf("signals = %q, want cont:13 cont:12", got)
	}
	if err := call("resume", ControlParams{PID: 21, Force: true}, nil); err != nil {
		t.Fatal(err)
	}
	if got := w.signals(); got != "cont:13 cont:12 cont:21" {
		t.Fatalf("signals = %q", got)
	}
	var orphanResumes int
	for _, e := range events(domain.EventControlResumed) {
		if e.Attrs["orphan"] == "true" {
			orphanResumes++
		}
	}
	if orphanResumes != 2 {
		t.Errorf("%d orphan resume events, want 2", orphanResumes)
	}

	// Once the processes run again, they are no longer orphans.
	for _, p := range []domain.ProcessSample{
		{PID: 12, PPID: 11, Name: "make"}, {PID: 13, PPID: 12, Name: "cc1"}, {PID: 21, PPID: 20, Name: "make"},
	} {
		w.add(p)
	}
	waitFor(t, "no orphans", func() bool { return status().OrphanCount == 0 })
}

// TestDaemonPausedIsNotOrphan checks that a workload throtd paused itself is
// never reported, members included.
func TestDaemonPausedIsNotOrphan(t *testing.T) {
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
		domain.ProcessSample{PID: 12, PPID: 11, Name: "compile"},
	)
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(shortTempDir(t), "throtd.sock")
	cfg.SampleInterval = config.Duration(20 * time.Millisecond)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewDaemon(cfg, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()
	defer func() { stop(); <-done }()
	status := func() StatusResult {
		var st StatusResult
		transport.Call(context.Background(), cfg.SocketPath, "status", nil, &st)
		return st
	}
	waitFor(t, "heavy workload", func() bool { return len(status().Heavy) == 1 })
	if err := transport.Call(context.Background(), cfg.SocketPath, "pause", ControlParams{PID: 11}, nil); err != nil {
		t.Fatal(err)
	}
	w.add(domain.ProcessSample{PID: 11, PPID: 10, Name: "go", State: domain.StateStopped})
	w.add(domain.ProcessSample{PID: 12, PPID: 11, Name: "compile", State: domain.StateStopped})
	time.Sleep(100 * time.Millisecond)
	if st := status(); st.OrphanCount != 0 || len(st.Paused) != 1 {
		t.Fatalf("orphans = %+v, paused = %+v; want none and one", st.Orphans, st.Paused)
	}
}

// TestDaemonCtrlZJob checks a Ctrl-Z'd agent: every stopped member of the
// job is job control, so there is no orphan_stopped event, and resuming
// its heavy child is refused without --force.
func TestDaemonCtrlZJob(t *testing.T) {
	stopped := domain.StateStopped
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "zsh"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "claude", State: stopped},
		domain.ProcessSample{PID: 12, PPID: 11, Name: "bash", State: stopped},
		domain.ProcessSample{PID: 13, PPID: 12, Name: "make", State: stopped},
		domain.ProcessSample{PID: 14, PPID: 13, Name: "cc1", State: stopped},
	)
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(shortTempDir(t), "throtd.sock")
	cfg.SampleInterval = config.Duration(20 * time.Millisecond)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewDaemon(cfg, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()
	defer func() { stop(); <-done }()
	call := func(method string, params, result any) error {
		return transport.Call(context.Background(), cfg.SocketPath, method, params, result)
	}
	var st StatusResult
	waitFor(t, "four orphans", func() bool {
		st = StatusResult{}
		call("status", nil, &st)
		return st.OrphanCount == 4
	})
	for _, o := range st.Orphans {
		if o.Kind != process.OrphanJobControl {
			t.Errorf("pid %d (%s): kind %s, want job_control", o.Key.PID, o.Name, o.Kind)
		}
	}
	time.Sleep(100 * time.Millisecond) // several ticks
	var events []domain.Event
	call("events", nil, &events)
	for _, e := range events {
		if e.Kind == domain.EventOrphanStopped {
			t.Errorf("orphan_stopped event for a shell job: %+v", e)
		}
	}
	if err := call("resume", ControlParams{PID: 13}, nil); err == nil || !strings.Contains(err.Error(), "Ctrl-Z") {
		t.Fatalf("resume a Ctrl-Z'd job's make: err = %v, want a Ctrl-Z refusal", err)
	}
	if got := w.signals(); got != "" {
		t.Fatalf("signals = %q, want none", got)
	}
	if err := call("resume", ControlParams{PID: 13, Force: true}, nil); err != nil {
		t.Fatal(err)
	}
	if got := w.signals(); got != "cont:13" {
		t.Fatalf("signals = %q, want cont:13 (forced: that process only)", got)
	}
}

// TestResumeOrphanTopHandResumed checks a left-paused workload whose top
// process was resumed by hand after the last sample: the stopped children
// are still continued, and only a workload with no member stopped is
// refused.
func TestResumeOrphanTopHandResumed(t *testing.T) {
	stopped := domain.StateStopped
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "bash"},
		domain.ProcessSample{PID: 12, PPID: 11, Name: "make", State: stopped},
		domain.ProcessSample{PID: 13, PPID: 12, Name: "cc1", State: stopped},
	)
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(t.TempDir(), "throtd.sock")
	d := NewDaemon(cfg, w, w, w, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	d.tick(ctx)
	w.add(domain.ProcessSample{PID: 12, PPID: 11, Name: "make", State: domain.StateRunning}) // kill -CONT 12
	raw, _ := json.Marshal(ControlParams{PID: 12})
	if _, err := d.resume(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if got := w.signals(); got != "cont:13" {
		t.Fatalf("signals = %q, want cont:13", got)
	}
	// Nothing is stopped any more, but the snapshot still says so.
	if _, err := d.resume(ctx, raw); err == nil || !strings.Contains(err.Error(), "not stopped") {
		t.Fatalf("second resume: err = %v, want a not-stopped refusal", err)
	}
	if got := w.signals(); got != "cont:13" {
		t.Errorf("signals = %q, want only cont:13", got)
	}
}
