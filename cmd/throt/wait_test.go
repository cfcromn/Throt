package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
)

// slotFull is a refusal with every slot of two taken, one by a lease.
var slotFull = app.Verdict{Reason: "all heavy slots are in use (1 running, 1 reserved, limit 2)", SchedulerMode: "enforce", RunningHeavy: 1, Reserved: 1, HeavyLimit: 2}

func TestHookDefer(t *testing.T) {
	const wantDeny = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Throt: 2 heavy builds running (limit 2); you are #3 in line. Your place is held. Do other non-build work now. Run ` + "`throt wait`" + ` with run_in_background so you are notified when a slot frees, then rerun this exact command."}}` + "\n"
	tests := []struct {
		name    string
		command string
		res     app.ReserveResult
		err     error
		oks     []bool // the fallback wait's verdicts
		want    string // exact output
		reserve int    // reserve calls
		checks  int    // fallback capacity checks
	}{
		{name: "slot free: allowed at once", command: "make", res: app.ReserveResult{Granted: true, LeaseID: "7"}, reserve: 1},
		{name: "no slot: deferred", command: "cd web && npm run build", res: app.ReserveResult{Position: 3, Reason: slotFull.Reason, Verdict: slotFull}, want: wantDeny, reserve: 1},
		{name: "light command: no reserve", command: "ls -la"},
		{name: "older throtd: waits as before", command: "make", err: errors.New("unknown method: reserve"), oks: []bool{true}, reserve: 1, checks: 1},
		{name: "throtd down: runs", command: "make", err: errors.New("connection refused"), oks: []bool{true}, reserve: 1, checks: 1},
		{name: "no place held: waits as before", command: "make", res: app.ReserveResult{Reason: app.ReasonNoAgent}, oks: []bool{true}, reserve: 1, checks: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeHook{t: t, clock: time.Unix(1000, 0), oks: tt.oks}
			if tt.oks == nil {
				f.oks = []bool{false}
			}
			calls := 0
			h := f.waiter()
			h.reserve = func(context.Context) (app.ReserveResult, error) {
				calls++
				return tt.res, tt.err
			}
			var out bytes.Buffer
			h.claudeCode(context.Background(), strings.NewReader(bashEvent(tt.command)), &out, 50*time.Second)
			if out.String() != tt.want {
				t.Errorf("output %q\nwant   %q", out.String(), tt.want)
			}
			if calls != tt.reserve || f.calls != tt.checks {
				t.Errorf("%d reserve calls, %d capacity checks; want %d, %d", calls, f.calls, tt.reserve, tt.checks)
			}
		})
	}
}

func TestDeferMessage(t *testing.T) {
	// Not a full set of slots: the reason says what holds the call back.
	r := app.ReserveResult{Position: 1, Reason: "pressure is high", Verdict: app.Verdict{SchedulerMode: "enforce", HeavyLimit: 4, RunningHeavy: 1}}
	if msg := deferMessage(r); !strings.HasPrefix(msg, "Throt: no heavy-build slot is free (pressure is high); you are #1 in line.") {
		t.Errorf("message = %q", msg)
	}
}

// fakeSlots drives slotWaiter with a fake clock; each reserve returns the
// next of results, then the last one again.
type fakeSlots struct {
	clock   time.Time
	results []app.ReserveResult
	errs    []error
	calls   int
}

func (f *fakeSlots) waiter(stdout, stderr *bytes.Buffer) slotWaiter {
	return slotWaiter{
		reserve: func(context.Context) (app.ReserveResult, error) {
			i := min(f.calls, max(len(f.results), len(f.errs))-1)
			f.calls++
			var r app.ReserveResult
			var err error
			if i < len(f.results) {
				r = f.results[i]
			}
			if i < len(f.errs) {
				err = f.errs[i]
			}
			return r, err
		},
		now:    func() time.Time { return f.clock },
		sleep:  func(_ context.Context, d time.Duration) error { f.clock = f.clock.Add(d); return nil },
		stdout: stdout,
		stderr: stderr,
	}
}

func TestThrotWait(t *testing.T) {
	waiting := app.ReserveResult{Position: 2, Reason: slotFull.Reason}
	tests := []struct {
		name    string
		timeout time.Duration
		results []app.ReserveResult
		errs    []error
		code    int    // 0 for success
		msg     string // in the error
		stdout  string
		calls   int
		waited  time.Duration
	}{
		{name: "granted after waiting", timeout: time.Minute, results: []app.ReserveResult{waiting, waiting, {Granted: true}},
			stdout: waitReady + "\n", calls: 3, waited: 2 * time.Second},
		{name: "granted at once", timeout: time.Minute, results: []app.ReserveResult{{Granted: true}}, stdout: waitReady + "\n", calls: 1},
		{name: "timeout", timeout: 2500 * time.Millisecond, results: []app.ReserveResult{waiting},
			code: 1, msg: "no heavy slot after 2.5s", calls: 4, waited: 2500 * time.Millisecond},
		{name: "throtd unreachable", timeout: time.Minute, errs: []error{errors.New("connection refused")}, code: 1, msg: "cannot reach throtd", calls: 1},
		{name: "throtd lost while waiting", timeout: time.Minute, results: []app.ReserveResult{waiting, {}}, errs: []error{nil, errors.New("EOF")},
			code: 1, msg: "cannot reach throtd", calls: 2, waited: time.Second},
		{name: "older throtd", timeout: time.Minute, errs: []error{errors.New("unknown method: reserve")}, code: 1, msg: "too old", calls: 1},
		{name: "no agent", timeout: time.Minute, results: []app.ReserveResult{{Reason: app.ReasonNoAgent}}, code: 1, msg: "no agent", calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Unix(1000, 0)
			f := &fakeSlots{clock: start, results: tt.results, errs: tt.errs}
			var stdout, stderr bytes.Buffer
			err := f.waiter(&stdout, &stderr).wait(context.Background(), tt.timeout)
			var ee exitError
			switch {
			case tt.code == 0 && err != nil:
				t.Fatalf("err = %v", err)
			case tt.code != 0 && (!errors.As(err, &ee) || ee.code != tt.code || !strings.Contains(ee.msg, tt.msg)):
				t.Fatalf("err = %#v, want exit %d with %q", err, tt.code, tt.msg)
			}
			if stdout.String() != tt.stdout || f.calls != tt.calls || f.clock.Sub(start) != tt.waited {
				t.Errorf("stdout %q, %d calls, waited %v; want %q, %d, %v", stdout.String(), f.calls, f.clock.Sub(start), tt.stdout, tt.calls, tt.waited)
			}
		})
	}
}
