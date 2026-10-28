package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// fakeRun drives a runner with scripted status replies and a fake clock.
type fakeRun struct {
	replies  []statusReply // one per status call; the last one repeats
	calls    int
	now      time.Time
	slept    time.Duration
	execPath string
	execArgv []string
	stderr   bytes.Buffer
	missing  bool // lookPath reports not found
}

type statusReply struct {
	st  app.StatusResult
	err error
}

func (f *fakeRun) runner() runner {
	return runner{
		status: func(context.Context) (app.StatusResult, error) {
			r := f.replies[min(f.calls, len(f.replies)-1)]
			f.calls++
			return r.st, r.err
		},
		ppid: 4242,
		lookPath: func(file string) (string, error) {
			if f.missing {
				return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
			}
			return "/usr/bin/" + file, nil
		},
		exec: func(path string, argv, _ []string) error {
			f.execPath, f.execArgv = path, argv
			return nil
		},
		now: func() time.Time { return f.now },
		sleep: func(_ context.Context, d time.Duration) error {
			f.now = f.now.Add(d)
			f.slept += d
			return nil
		},
		stderr: &f.stderr,
	}
}

func busy() statusReply {
	return statusReply{st: app.StatusResult{
		Pressure: domain.PressureElevated,
		Heavy: []process.WorkloadView{
			{Key: domain.ProcessKey{PID: 11}, Name: "go", Class: domain.ClassBuild},
			{Key: domain.ProcessKey{PID: 12}, Name: "java", Class: domain.ClassBuild},
		},
		Scheduler: app.SchedulerStatus{Mode: scheduler.ModeEnforce, HeavyLimit: 2},
	}}
}

func free() statusReply {
	return statusReply{st: app.StatusResult{
		Pressure:  domain.PressureNormal,
		Scheduler: app.SchedulerStatus{Mode: scheduler.ModeEnforce, HeavyLimit: 2},
	}}
}

func down() statusReply { return statusReply{err: errors.New("dial unix: no such file")} }

func TestRunAdmission(t *testing.T) {
	argv := []string{"npm", "run", "build"}
	tests := []struct {
		name      string
		replies   []statusReply
		opts      runOptions
		wantCalls int
		wantSlept time.Duration
		stderr    []string
		quiet     bool // stderr must be empty
	}{
		{name: "immediate ok", replies: []statusReply{free()}, wantCalls: 1, quiet: true},
		{
			name: "waits until ok", replies: []statusReply{busy(), busy(), busy(), free()},
			wantCalls: 4, wantSlept: 3 * time.Second,
			stderr: []string{"waiting for a heavy slot (2/2 running, pressure elevated)", "capacity available after 3s"},
		},
		{
			name: "timeout runs anyway", replies: []statusReply{busy()}, opts: runOptions{timeout: 2500 * time.Millisecond},
			wantCalls: 4, wantSlept: 2500 * time.Millisecond,
			stderr: []string{"waiting for a heavy slot", "still no capacity after 2.5s", "running anyway"},
		},
		{
			name: "daemon down runs immediately", replies: []statusReply{down()}, wantCalls: 1,
			stderr: []string{"throtd is not reachable", "running without waiting"},
		},
		{
			name: "daemon lost while waiting runs", replies: []statusReply{busy(), down()}, wantCalls: 2, wantSlept: time.Second,
			stderr: []string{"lost throtd", "running without waiting"},
		},
		{
			name: "no-wait when busy", replies: []statusReply{busy()}, opts: runOptions{noWait: true}, wantCalls: 1,
			stderr: []string{"ok to start a heavy job: no", "all heavy slots are in use", "--no-wait"},
		},
		{
			name: "no-wait when free", replies: []statusReply{free()}, opts: runOptions{noWait: true}, wantCalls: 1,
			stderr: []string{"ok to start a heavy job: yes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRun{replies: tt.replies, now: time.Unix(1000, 0)}
			tt.opts.argv = argv
			if err := f.runner().run(context.Background(), tt.opts); err != nil {
				t.Fatalf("run: %v", err)
			}
			if f.execPath != "/usr/bin/npm" || !reflect.DeepEqual(f.execArgv, argv) {
				t.Errorf("exec %q %q", f.execPath, f.execArgv)
			}
			if f.calls != tt.wantCalls || f.slept != tt.wantSlept {
				t.Errorf("status calls %d, slept %s; want %d, %s", f.calls, f.slept, tt.wantCalls, tt.wantSlept)
			}
			errText := f.stderr.String()
			if tt.quiet && errText != "" {
				t.Errorf("unexpected stderr: %q", errText)
			}
			for _, want := range tt.stderr {
				if !strings.Contains(errText, want) {
					t.Errorf("stderr missing %q:\n%s", want, errText)
				}
			}
		})
	}
}

func TestRunNoticesAreThrottled(t *testing.T) {
	replies := make([]statusReply, 25)
	for i := range replies {
		replies[i] = busy()
	}
	f := &fakeRun{replies: append(replies, free()), now: time.Unix(1000, 0)}
	if err := f.runner().run(context.Background(), runOptions{argv: []string{"make"}}); err != nil {
		t.Fatal(err)
	}
	// Waiting 25s prints at 0s, 10s and 20s.
	if n := strings.Count(f.stderr.String(), "waiting for a heavy slot"); n != 3 {
		t.Errorf("%d waiting notices, want 3:\n%s", n, f.stderr.String())
	}
}

func TestRunCancelledWhileWaiting(t *testing.T) {
	f := &fakeRun{replies: []statusReply{busy()}, now: time.Unix(1000, 0)}
	r := f.runner()
	r.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := r.run(context.Background(), runOptions{argv: []string{"make"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if f.execArgv != nil {
		t.Errorf("exec ran after cancellation: %q", f.execArgv)
	}
}

func TestRunCommandNotFound(t *testing.T) {
	f := &fakeRun{replies: []statusReply{free()}, missing: true}
	err := f.runner().run(context.Background(), runOptions{argv: []string{"no-such-tool"}})
	var ee exitError
	if !errors.As(err, &ee) || ee.code != 127 || !strings.Contains(ee.msg, "no-such-tool: command not found") {
		t.Fatalf("err = %#v", err)
	}
	if f.execArgv != nil {
		t.Errorf("exec ran: %q", f.execArgv)
	}
}

func TestRunExecFailure(t *testing.T) {
	f := &fakeRun{replies: []statusReply{free()}}
	r := f.runner()
	r.exec = func(string, []string, []string) error { return errors.New("permission denied") }
	var ee exitError
	if err := r.run(context.Background(), runOptions{argv: []string{"x"}}); !errors.As(err, &ee) || ee.code != 126 {
		t.Fatalf("err = %#v", err)
	}
}

func TestRunAgentDetection(t *testing.T) {
	// The agent "claude-1" (pid 4242, our parent) is at its per-agent
	// budget, so only a run attributed to it has to wait.
	st := app.StatusResult{
		Pressure: domain.PressureNormal,
		Agents: []process.AgentView{
			{Key: domain.ProcessKey{PID: 4242}, Agent: "claude-1"},
			{Key: domain.ProcessKey{PID: 7000}, Agent: "codex-1"},
		},
		Heavy:     []process.WorkloadView{{Key: domain.ProcessKey{PID: 11}, Name: "go", Owner: "claude-1"}},
		Scheduler: app.SchedulerStatus{Mode: scheduler.ModeEnforce, HeavyLimit: 4, PerAgentLimit: 1},
	}
	tests := []struct {
		name      string
		ppid      int
		ancestors []int
		wantWait  bool
	}{
		{name: "parent is the agent", ppid: 4242, wantWait: true},
		{name: "agent is a grandparent", ppid: 500, ancestors: []int{500, 4242, 1}, wantWait: true},
		{name: "nearest agent wins", ppid: 500, ancestors: []int{500, 7000, 4242}, wantWait: false},
		{name: "ambiguous ancestry uses the parent only", ppid: 500, ancestors: nil, wantWait: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := free()
			done.st.Agents = st.Agents
			f := &fakeRun{replies: []statusReply{{st: st}, done}, now: time.Unix(1000, 0)}
			r := f.runner()
			r.ppid = tt.ppid
			asked := 0
			r.ancestors = func(context.Context) []int { asked++; return tt.ancestors }
			if err := r.run(context.Background(), runOptions{argv: []string{"go", "test"}}); err != nil {
				t.Fatal(err)
			}
			if waited := f.calls > 1; waited != tt.wantWait {
				t.Errorf("waited = %v, want %v; stderr:\n%s", waited, tt.wantWait, f.stderr.String())
			}
			if asked > 1 || (tt.ppid == 4242 && asked != 0) {
				t.Errorf("ancestors read %d times", asked)
			}
		})
	}
}

func TestAncestorChain(t *testing.T) {
	procs := []domain.ProcessSample{{PID: 500, PPID: 400}, {PID: 400, PPID: 1}, {PID: 1, PPID: 0}, {PID: 9, PPID: 9}}
	if got := ancestorChain(500, procs); !reflect.DeepEqual(got, []int{500, 400, 1}) {
		t.Errorf("chain = %v", got)
	}
	if got := ancestorChain(77, procs); !reflect.DeepEqual(got, []int{77}) {
		t.Errorf("missing parent: chain = %v", got)
	}
	if got := ancestorChain(9, procs); !reflect.DeepEqual(got, []int{9}) {
		t.Errorf("cycle: chain = %v", got)
	}
}

func TestParseRunArgs(t *testing.T) {
	tests := []struct {
		args    []string
		want    runOptions
		wantErr string
	}{
		{args: []string{"--", "npm", "run", "build"}, want: runOptions{argv: []string{"npm", "run", "build"}}},
		{args: []string{"make", "-j8"}, want: runOptions{argv: []string{"make", "-j8"}}},
		{args: []string{"--timeout", "30s", "--", "go", "test", "--", "-run", "X"},
			want: runOptions{timeout: 30 * time.Second, argv: []string{"go", "test", "--", "-run", "X"}}},
		{args: []string{"--no-wait", "--", "--weird-name"}, want: runOptions{noWait: true, argv: []string{"--weird-name"}}},
		{args: []string{"-timeout=1m", "cargo", "--timeout", "5"}, want: runOptions{timeout: time.Minute, argv: []string{"cargo", "--timeout", "5"}}},
		{args: []string{}, wantErr: "missing command"},
		{args: []string{"--"}, wantErr: "missing command"},
		{args: []string{"--timeout", "-1s", "--", "x"}, wantErr: "negative"},
		{args: []string{"--bogus", "--", "x"}, wantErr: "not defined"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			got, err := parseRunArgs(tt.args, io.Discard)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestRunDispatchPassesArgs(t *testing.T) {
	// A missing command fails before any daemon call, even with global flags.
	err := run([]string{"-config", "/nonexistent/throt.json", "run"})
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("err = %v", err)
	}
}
