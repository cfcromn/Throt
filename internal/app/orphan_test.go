package app

import (
	"context"
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
