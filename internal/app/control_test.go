package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

var errFS = errors.New("injected filesystem failure")

// controlDaemon is a daemon that is never run, with its socket (and so
// paused.json) in dir. Tests call its control paths directly.
func controlDaemon(t *testing.T, dir string, sig control.Signaler) *Daemon {
	t.Helper()
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(dir, "throtd.sock")
	return NewDaemon(cfg, fakeSampler{}, fakeSampler{}, sig, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// target is a heavy workload of this user with top process pid and the
// given members (top first), all started at start.
func target(start time.Time, pids ...int) process.WorkloadTarget {
	t := process.WorkloadTarget{
		Key: domain.ProcessKey{PID: pids[0], Start: start}, Name: "go", Class: domain.ClassBuild, Owner: "claude-1",
	}
	for _, pid := range pids {
		t.Members = append(t.Members, process.Member{Key: domain.ProcessKey{PID: pid, Start: start}, UID: uint32(os.Getuid())})
	}
	return t
}

// goTarget is fakeSampler's heavy workload, pid 11.
func goTarget() process.WorkloadTarget { return target(time.Unix(1, 0), 11) }

func countEvents(d *Daemon, kind domain.EventKind, substr string) int {
	n := 0
	for _, e := range eventsOf(d, kind) {
		if strings.Contains(e.Message, substr) {
			n++
		}
	}
	return n
}

func TestResumeOrphanRefusals(t *testing.T) {
	uid := uint32(os.Getuid())
	stopped := func(pid int, uid uint32) process.StoppedView {
		return process.StoppedView{
			Key: domain.ProcessKey{PID: pid, Start: time.Unix(int64(pid), 0)}, UID: uid,
			Name: "make", Class: domain.ClassBuild, Owner: "claude-1",
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, d *Daemon)
		pid   int
		force bool
		want  string
	}{
		{
			name: "owned by another user",
			setup: func(_ *testing.T, d *Daemon) {
				d.stopped = []process.StoppedView{stopped(30, uid+1)}
			},
			pid: 30, force: true, // even --force never touches another user's process
			want: "owned by another user",
		},
		{
			name: "member of another paused workload",
			setup: func(t *testing.T, d *Daemon) {
				// pid 31 is a member of the paused workload 30, not a record
				// of its own, so resume looks for it among the orphans.
				tg := target(time.Unix(30, 0), 30)
				tg.Members = append(tg.Members, process.Member{Key: domain.ProcessKey{PID: 31, Start: time.Unix(31, 0)}, UID: uid})
				if err := d.ctl.Pause(context.Background(), tg); err != nil {
					t.Fatal(err)
				}
				d.stopped = []process.StoppedView{stopped(30, uid), stopped(31, uid)}
			},
			pid: 31, force: true,
			want: "paused by throtd as part of another workload",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(
				domain.ProcessSample{PID: 30, PPID: 1, Name: "make", State: domain.StateStopped},
				domain.ProcessSample{PID: 31, PPID: 30, Name: "cc1", State: domain.StateStopped},
			)
			d := controlDaemon(t, t.TempDir(), w)
			tc.setup(t, d)
			before := w.signals()
			raw, _ := json.Marshal(ControlParams{PID: tc.pid, Force: tc.force})
			_, err := d.resume(context.Background(), raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got := w.signals(); got != before {
				t.Errorf("signals = %q, want %q (none sent by the refused resume)", got, before)
			}
			if n := countEvents(d, domain.EventControlRefused, tc.want); n != 1 {
				t.Errorf("%d control_refused events naming %q, want 1", n, tc.want)
			}
		})
	}
}

// TestRetryResumesEvents checks that retries of a failed resume emit one
// event when they start failing and one when nothing is left, not one per
// tick.
func TestRetryResumesEvents(t *testing.T) {
	sig := &fakeSignaler{}
	d := controlDaemon(t, t.TempDir(), sig)
	ctx := context.Background()
	if err := d.ctl.Pause(ctx, goTarget()); err != nil {
		t.Fatal(err)
	}
	sig.setContFail(true)
	if err := d.ctl.Resume(ctx, goTarget().Key); err == nil {
		t.Fatal("resume succeeded with Continue failing")
	}
	for i, step := range []struct {
		contFail        bool
		failed, resumed int // cumulative events
		left            int // records left
	}{
		{true, 1, 0, 1},  // retries start failing: one event
		{true, 1, 0, 1},  // still failing: no repeat
		{false, 1, 1, 0}, // resumed: one recovery event
		{false, 1, 1, 0}, // nothing left: quiet
	} {
		sig.setContFail(step.contFail)
		d.retryResumes(ctx)
		failed := countEvents(d, domain.EventControlFailed, "resume retry failed")
		resumed := countEvents(d, domain.EventControlResumed, "left stopped by an earlier failure")
		if failed != step.failed || resumed != step.resumed || len(d.ctl.Paused()) != step.left {
			t.Fatalf("step %d: %d failed, %d resumed events, %d records; want %d, %d, %d",
				i, failed, resumed, len(d.ctl.Paused()), step.failed, step.resumed, step.left)
		}
	}
	if got := sig.signals(); got != "stop:11 cont:11" {
		t.Errorf("signals = %q", got)
	}
}

func TestSaveState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exists  string // "", "file", or "dir" (a non-empty directory) at paused.json
		state   []byte
		write   error
		wantErr bool
		want    string // paused.json contents afterwards; "" means absent
	}{
		{name: "write", state: []byte(`{"v":1}`), want: `{"v":1}`},
		{name: "replace", exists: "file", state: []byte(`{"v":2}`), want: `{"v":2}`},
		{name: "write fails", exists: "file", state: []byte(`{"v":2}`), write: errFS, wantErr: true, want: "old"},
		{name: "remove", exists: "file"},
		{name: "remove absent"},
		{name: "remove fails", exists: "dir", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			d := controlDaemon(t, dir, &fakeSignaler{})
			path := d.statePath()
			switch tc.exists {
			case "file":
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "dir":
				if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.write != nil {
				d.writeFile = func(string, []byte) error { return tc.write }
			}
			err := d.saveState(tc.state)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.exists == "dir" {
				return
			}
			b, rerr := os.ReadFile(path)
			switch {
			case tc.want == "" && !os.IsNotExist(rerr):
				t.Errorf("paused.json = %q, %v; want absent", b, rerr)
			case tc.want != "" && string(b) != tc.want:
				t.Errorf("paused.json = %q, %v; want %q", b, rerr, tc.want)
			}
		})
	}
}

// TestPauseRefusedWhenStateUnsaved checks the safety rule behind saveState:
// without a durable record nothing is stopped, since a crash could leave
// the work stopped forever.
func TestPauseRefusedWhenStateUnsaved(t *testing.T) {
	sig := &fakeSignaler{}
	d := controlDaemon(t, t.TempDir(), sig)
	d.writeFile = func(string, []byte) error { return errFS }
	d.status.Heavy = []process.WorkloadView{{Key: goTarget().Key, Name: "go", Owner: "claude-1"}}
	d.graph.Update(time.Now(), mustSample(t))
	raw, _ := json.Marshal(ControlParams{PID: 11})
	_, err := d.pause(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "cannot persist pause state") {
		t.Fatalf("err = %v, want a persist refusal", err)
	}
	if got := sig.signals(); got != "" {
		t.Errorf("signals = %q, want none", got)
	}
	if p := d.ctl.Paused(); len(p) != 0 {
		t.Errorf("paused = %+v, want none", p)
	}
	if n := countEvents(d, domain.EventControlFailed, "cannot persist"); n != 1 {
		t.Errorf("%d control_failed events, want 1", n)
	}
}

func mustSample(t *testing.T) []domain.ProcessSample {
	t.Helper()
	procs, err := fakeSampler{}.SampleProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return procs
}

func TestRecoverPaused(t *testing.T) {
	// validState is what a daemon that paused goTarget left behind.
	validState := func(t *testing.T) []byte {
		t.Helper()
		dir := t.TempDir()
		d := controlDaemon(t, dir, &fakeSignaler{})
		if err := d.ctl.Pause(context.Background(), goTarget()); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(d.statePath())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name     string
		file     func(t *testing.T, path string) // creates paused.json; nil leaves it absent
		rename   error
		contFail bool
		event    domain.EventKind // "" means no control event
		msg      string
		signals  string
		left     string // what is at paused.json afterwards: "", "file", or "dir"
		invalid  bool   // paused.json.invalid exists afterwards
		retrying bool
	}{
		{name: "absent"},
		{
			name: "unreadable",
			file: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			event: domain.EventControlFailed, msg: "recover paused workloads: read ", left: "dir",
		},
		{
			name:  "malformed",
			file:  writeState([]byte("garbage")),
			event: domain.EventControlFailed, msg: "; moved to ", invalid: true,
		},
		{
			name:   "malformed, move aside fails",
			file:   writeState([]byte("garbage")),
			rename: errFS,
			event:  domain.EventControlFailed, msg: "; left in place: " + errFS.Error(), left: "file",
		},
		{
			name:     "partial failure",
			file:     func(t *testing.T, path string) { writeState(validState(t))(t, path) },
			contFail: true,
			event:    domain.EventControlFailed, msg: "will keep retrying", left: "file", retrying: true,
		},
		{
			name:  "resumed",
			file:  func(t *testing.T, path string) { writeState(validState(t))(t, path) },
			event: domain.EventControlResumed, msg: "left paused by a previous throtd", signals: "cont:11",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := &fakeSignaler{}
			sig.setContFail(tc.contFail)
			d := controlDaemon(t, t.TempDir(), sig)
			path := d.statePath()
			if tc.file != nil {
				tc.file(t, path)
			}
			if tc.rename != nil {
				d.rename = func(string, string) error { return tc.rename }
			}
			d.recoverPaused(context.Background())

			var got []domain.Event
			for _, kind := range []domain.EventKind{domain.EventControlFailed, domain.EventControlResumed} {
				got = append(got, eventsOf(d, kind)...)
			}
			switch {
			case tc.event == "" && len(got) != 0:
				t.Errorf("events = %+v, want none", got)
			case tc.event != "" && (len(got) != 1 || got[0].Kind != tc.event || !strings.Contains(got[0].Message, tc.msg)):
				t.Errorf("events = %+v, want one %s containing %q", got, tc.event, tc.msg)
			}
			if s := sig.signals(); s != tc.signals {
				t.Errorf("signals = %q, want %q", s, tc.signals)
			}
			left := ""
			if fi, err := os.Stat(path); err == nil {
				left = map[bool]string{true: "dir", false: "file"}[fi.IsDir()]
			}
			if left != tc.left {
				t.Errorf("paused.json is %q, want %q", left, tc.left)
			}
			if _, err := os.Stat(path + ".invalid"); (err == nil) != tc.invalid {
				t.Errorf("paused.json.invalid: %v, want present %v", err, tc.invalid)
			}
			if d.retryFailing != tc.retrying {
				t.Errorf("retryFailing = %v, want %v", d.retryFailing, tc.retrying)
			}
			if !tc.retrying {
				return
			}
			// The next ticks finish the job and report it once.
			sig.setContFail(false)
			d.retryResumes(context.Background())
			if n := countEvents(d, domain.EventControlResumed, "left stopped by an earlier failure"); n != 1 || len(d.ctl.Paused()) != 0 {
				t.Errorf("after retry: %d recovery events, paused = %+v", n, d.ctl.Paused())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("paused.json left after the retry resumed everything: %v", err)
			}
		})
	}
}

func writeState(b []byte) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTrack checks that a sampler's failures and recoveries are each one
// event, while the degraded summary always shows the latest error.
func TestTrack(t *testing.T) {
	d := controlDaemon(t, t.TempDir(), &fakeSignaler{})
	errHost, errHost2, errProcs := errors.New("a"), errors.New("b"), errors.New("p")
	for i, step := range []struct {
		host, procs         error
		degraded, recovered int // cumulative events
		summary             string
	}{
		{nil, nil, 0, 0, ""},
		{errHost, nil, 1, 0, "host: a"},
		{errHost2, nil, 1, 0, "host: b"}, // still failing: no new event, latest message
		{errHost, errProcs, 2, 0, "host: a; processes: p"},
		{nil, errProcs, 2, 1, "processes: p"},
		{nil, nil, 2, 2, ""},
		{nil, nil, 2, 2, ""},
	} {
		d.track("host", step.host)
		d.track("processes", step.procs)
		degraded, recovered := eventsOf(d, domain.EventDegraded), eventsOf(d, domain.EventRecovered)
		if len(degraded) != step.degraded || len(recovered) != step.recovered {
			t.Fatalf("step %d: %d degraded, %d recovered events; want %d, %d", i, len(degraded), len(recovered), step.degraded, step.recovered)
		}
		if got := d.degradedSummary(); got != step.summary {
			t.Errorf("step %d: summary = %q, want %q", i, got, step.summary)
		}
	}
	for _, e := range append(eventsOf(d, domain.EventDegraded), eventsOf(d, domain.EventRecovered)...) {
		if s := e.Attrs["sampler"]; !strings.HasPrefix(e.Message, s+" sampling ") {
			t.Errorf("event %q has sampler %q", e.Message, s)
		}
	}
}

// TestControlEventsAndRefusalWords checks that control_paused and
// control_resumed carry an action attr, and that a controller refusal
// reads as a sentence while the event keeps the stable reason code.
func TestControlEventsAndRefusalWords(t *testing.T) {
	sig := &fakeSignaler{}
	d := controlDaemon(t, t.TempDir(), sig)
	d.status.Heavy = []process.WorkloadView{{Key: goTarget().Key, Name: "go", Owner: "claude-1"}}
	d.graph.Update(time.Now(), mustSample(t))
	raw, _ := json.Marshal(ControlParams{PID: 11})
	ctx := context.Background()
	if _, err := d.pause(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if ev := eventsOf(d, domain.EventControlPaused); len(ev) != 1 || ev[0].Attrs["action"] != "pause" {
		t.Errorf("control_paused events = %+v, want one with action pause", ev)
	}
	_, err := d.pause(ctx, raw)
	want := "pause refused: it is already paused, alone or as part of another workload"
	if err == nil || err.Error() != want {
		t.Fatalf("second pause: err = %v, want %q", err, want)
	}
	ev := eventsOf(d, domain.EventControlRefused)
	if len(ev) != 1 || ev[0].Attrs["reason"] != control.ReasonAlreadyPaused || !strings.Contains(ev[0].Message, "already paused, alone") {
		t.Errorf("control_refused events = %+v, want reason code %s and the sentence", ev, control.ReasonAlreadyPaused)
	}
	if _, err := d.resume(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if ev := eventsOf(d, domain.EventControlResumed); len(ev) != 1 || ev[0].Attrs["action"] != "resume" {
		t.Errorf("control_resumed events = %+v, want one with action resume", ev)
	}
}

// TestResumeOrphanTwice checks that resuming an orphan again before the
// next sample, while the snapshot still shows it stopped, is refused from
// the process's live state instead of reported as another resume.
func TestResumeOrphanTwice(t *testing.T) {
	uid := uint32(os.Getuid())
	w := newWorld(domain.ProcessSample{PID: 30, PPID: 1, Name: "make", State: domain.StateStopped})
	d := controlDaemon(t, t.TempDir(), w)
	d.stopped = []process.StoppedView{{
		Key: domain.ProcessKey{PID: 30, Start: time.Unix(30, 0)}, UID: uid, Name: "make", Class: domain.ClassBuild,
	}}
	raw, _ := json.Marshal(ControlParams{PID: 30, Force: true})
	ctx := context.Background()
	if _, err := d.resume(ctx, raw); err != nil {
		t.Fatal(err)
	}
	_, err := d.resume(ctx, raw)
	if err == nil || !strings.Contains(err.Error(), "resume refused: not stopped") {
		t.Fatalf("second resume: err = %v, want a not-stopped refusal", err)
	}
	if got := w.signals(); got != "cont:30" {
		t.Errorf("signals = %q, want one cont:30", got)
	}
	if ev := eventsOf(d, domain.EventControlResumed); len(ev) != 1 || ev[0].Attrs["action"] != "resume" {
		t.Errorf("control_resumed events = %+v, want one with action resume", ev)
	}
	// A process gone since the sample is refused too, and not signaled.
	w.exit(30)
	if _, err := d.resume(ctx, raw); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("resume of an exited orphan: err = %v", err)
	}
	if got := w.signals(); got != "cont:30" {
		t.Errorf("signals = %q, want one cont:30", got)
	}
}
