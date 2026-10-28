package control

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

const me = 501

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func clock() time.Time { return epoch.Add(time.Hour) }

func key(pid int) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: epoch.Add(time.Duration(pid) * time.Second)}
}

type ident struct {
	start time.Time
	uid   uint32
}

// fakeSig is a scripted Signaler. Processes absent from procs are gone.
type fakeSig struct {
	procs   map[int]ident
	idErr   map[int]error
	stopErr map[int]error
	contErr map[int]error
	// onIdentity runs before each Identity lookup, to script races.
	onIdentity func(pid int)
	// calls records signals only, e.g. "stop 10" or "cont 11".
	calls []string
}

// newSig returns a fake where each pid is alive, owned by me, and started
// at key(pid).Start.
func newSig(pids ...int) *fakeSig {
	f := &fakeSig{procs: map[int]ident{}, idErr: map[int]error{}, stopErr: map[int]error{}, contErr: map[int]error{}}
	for _, p := range pids {
		f.procs[p] = ident{key(p).Start, me}
	}
	return f
}

func (f *fakeSig) Identity(ctx context.Context, pid int) (time.Time, uint32, error) {
	if f.onIdentity != nil {
		f.onIdentity(pid)
	}
	if err := f.idErr[pid]; err != nil {
		return time.Time{}, 0, err
	}
	p, ok := f.procs[pid]
	if !ok {
		return time.Time{}, 0, fmt.Errorf("pid %d: %w", pid, domain.ErrProcessGone)
	}
	return p.start, p.uid, nil
}

func (f *fakeSig) Stop(pid int) error {
	f.calls = append(f.calls, fmt.Sprintf("stop %d", pid))
	return f.stopErr[pid]
}

func (f *fakeSig) Continue(pid int) error {
	f.calls = append(f.calls, fmt.Sprintf("cont %d", pid))
	return f.contErr[pid]
}

func (f *fakeSig) reset() { f.calls = nil }

// target builds an attributed build workload of pids, top first.
func target(pids ...int) process.WorkloadTarget {
	t := process.WorkloadTarget{Name: "make", Class: domain.ClassBuild, Owner: "claude-1"}
	for _, p := range pids {
		t.Members = append(t.Members, process.Member{Key: key(p), UID: me})
	}
	if len(pids) > 0 {
		t.Key = key(pids[0])
	}
	return t
}

func keys(pids ...int) []domain.ProcessKey {
	out := make([]domain.ProcessKey, len(pids))
	for i, p := range pids {
		out[i] = key(p)
	}
	return out
}

func reason(err error) string {
	var r *RefusedError
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

func TestPauseAndResume(t *testing.T) {
	sig := newSig(10, 11, 12)
	c := New(sig, me, clock)
	if err := c.Pause(context.Background(), target(10, 11, 12)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop 10", "stop 11", "stop 12"}; !reflect.DeepEqual(sig.calls, want) {
		t.Errorf("pause calls = %v, want %v", sig.calls, want)
	}
	want := []PausedWorkload{{Key: key(10), Name: "make", Owner: "claude-1", Members: keys(10, 11, 12), Since: clock()}}
	if got := c.Paused(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paused = %+v, want %+v", got, want)
	}

	sig.reset()
	if err := c.Resume(context.Background(), key(10)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cont 12", "cont 11", "cont 10"}; !reflect.DeepEqual(sig.calls, want) {
		t.Errorf("resume calls = %v, want %v", sig.calls, want)
	}
	if got := c.Paused(); len(got) != 0 {
		t.Errorf("Paused after resume = %+v", got)
	}
	if r := reason(c.Resume(context.Background(), key(10))); r != ReasonNotPaused {
		t.Errorf("second resume reason = %q, want %q", r, ReasonNotPaused)
	}
}

func TestPauseRefusals(t *testing.T) {
	tooMany := make([]int, MaxMembers+1)
	for i := range tooMany {
		tooMany[i] = i + 2
	}
	tests := []struct {
		name   string
		mutate func(*process.WorkloadTarget)
		want   string
	}{
		{"unattributed", func(t *process.WorkloadTarget) { t.Owner = "" }, ReasonUnattributed},
		{"agent class", func(t *process.WorkloadTarget) { t.Class = domain.ClassAgent }, ReasonNotHeavy},
		{"unknown class", func(t *process.WorkloadTarget) { t.Class = domain.ClassUnknown }, ReasonNotHeavy},
		{"no members", func(t *process.WorkloadTarget) { t.Members = nil }, ReasonNoMembers},
		{"too many members", func(t *process.WorkloadTarget) { *t = target(tooMany...) }, ReasonTooManyMembers},
		{"top not first", func(t *process.WorkloadTarget) { t.Key = key(11) }, ReasonInvalidTarget},
		{"pid 1", func(t *process.WorkloadTarget) { t.Members[1].Key.PID = 1 }, ReasonInvalidTarget},
		{"pid 0 (process group)", func(t *process.WorkloadTarget) { t.Members[1].Key.PID = 0 }, ReasonInvalidTarget},
		{"negative pid", func(t *process.WorkloadTarget) { t.Members[1].Key.PID = -1 }, ReasonInvalidTarget},
		{"duplicate member", func(t *process.WorkloadTarget) { t.Members[1] = t.Members[0] }, ReasonInvalidTarget},
		{"foreign top uid", func(t *process.WorkloadTarget) { t.Members[0].UID = 0 }, ReasonForeignUID},
		{"foreign child uid", func(t *process.WorkloadTarget) { t.Members[1].UID = 502 }, ReasonForeignUID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := newSig(10, 11)
			c := New(sig, me, clock)
			tg := target(10, 11)
			tt.mutate(&tg)
			err := c.Pause(context.Background(), tg)
			if r := reason(err); r != tt.want {
				t.Fatalf("reason = %q (err %v), want %q", r, err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q lacks reason", err)
			}
			if len(sig.calls) != 0 || len(c.Paused()) != 0 {
				t.Errorf("refusal signaled %v / recorded %v", sig.calls, c.Paused())
			}
		})
	}
}

func TestPauseAlreadyPaused(t *testing.T) {
	sig := newSig(10, 11, 20)
	c := New(sig, me, clock)
	if err := c.Pause(context.Background(), target(10, 11)); err != nil {
		t.Fatal(err)
	}
	sig.reset()
	for _, tg := range []process.WorkloadTarget{target(10, 11), target(11), target(20, 11)} {
		if r := reason(c.Pause(context.Background(), tg)); r != ReasonAlreadyPaused {
			t.Errorf("pause %v reason = %q, want %q", tg.Key, r, ReasonAlreadyPaused)
		}
	}
	if len(sig.calls) != 0 {
		t.Errorf("refusals signaled %v", sig.calls)
	}
}

func TestPauseLimit(t *testing.T) {
	sig := newSig()
	c := New(sig, me, clock)
	for i := range MaxPaused + 1 {
		pid := 100 + i
		sig.procs[pid] = ident{key(pid).Start, me}
		err := c.Pause(context.Background(), target(pid))
		if i < MaxPaused && err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
		if i == MaxPaused {
			if r := reason(err); r != ReasonLimitReached {
				t.Fatalf("reason = %q, want %q", r, ReasonLimitReached)
			}
		}
	}
	if n := len(sig.calls); n != MaxPaused {
		t.Errorf("signals = %d, want %d", n, MaxPaused)
	}
}

func TestPauseMaxMembersAccepted(t *testing.T) {
	pids := make([]int, MaxMembers)
	for i := range pids {
		pids[i] = i + 2
	}
	c := New(newSig(pids...), me, clock)
	if err := c.Pause(context.Background(), target(pids...)); err != nil {
		t.Fatalf("pause of %d members: %v", MaxMembers, err)
	}
}

func TestPauseRaces(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		setup     func(*fakeSig)
		wantErr   error // nil means success
		wantCalls []string
		// wantMembers is the recorded members on success.
		wantMembers []domain.ProcessKey
	}{
		{
			name:      "top exited before pause aborts with no signals",
			setup:     func(f *fakeSig) { delete(f.procs, 10) },
			wantErr:   domain.ErrProcessGone,
			wantCalls: nil,
		},
		{
			name:      "top pid reused aborts with no signals",
			setup:     func(f *fakeSig) { f.procs[10] = ident{epoch.Add(time.Minute), me} },
			wantErr:   domain.ErrProcessGone,
			wantCalls: nil,
		},
		{
			name:      "top pid now another user aborts",
			setup:     func(f *fakeSig) { f.procs[10] = ident{key(10).Start, 0} },
			wantErr:   domain.ErrProcessGone,
			wantCalls: nil,
		},
		{
			name: "top exits between identity and stop aborts",
			setup: func(f *fakeSig) {
				f.stopErr[10] = fmt.Errorf("esrch: %w", domain.ErrProcessGone)
			},
			wantErr:   domain.ErrProcessGone,
			wantCalls: []string{"stop 10"},
		},
		{
			name: "child exits mid-pause is skipped",
			setup: func(f *fakeSig) {
				f.onIdentity = func(pid int) {
					if pid == 11 {
						delete(f.procs, 11)
					}
				}
			},
			wantCalls:   []string{"stop 10", "stop 12"},
			wantMembers: keys(10, 12),
		},
		{
			name:        "child exits between identity and stop is skipped",
			setup:       func(f *fakeSig) { f.stopErr[11] = domain.ErrProcessGone },
			wantCalls:   []string{"stop 10", "stop 11", "stop 12"},
			wantMembers: keys(10, 12),
		},
		{
			name:        "child pid reuse is never signaled",
			setup:       func(f *fakeSig) { f.procs[11] = ident{epoch.Add(time.Minute), me} },
			wantCalls:   []string{"stop 10", "stop 12"},
			wantMembers: keys(10, 12),
		},
		{
			name:        "child pid now foreign uid is never signaled",
			setup:       func(f *fakeSig) { f.procs[12] = ident{key(12).Start, 0} },
			wantCalls:   []string{"stop 10", "stop 11"},
			wantMembers: keys(10, 11),
		},
		{
			name:      "stop failure rolls back in reverse order",
			setup:     func(f *fakeSig) { f.stopErr[12] = boom },
			wantErr:   boom,
			wantCalls: []string{"stop 10", "stop 11", "stop 12", "cont 11", "cont 10"},
		},
		{
			name:      "identity failure rolls back without signaling the member",
			setup:     func(f *fakeSig) { f.idErr[11] = boom },
			wantErr:   boom,
			wantCalls: []string{"stop 10", "cont 10"},
		},
		{
			name: "rollback skips a member that exited",
			setup: func(f *fakeSig) {
				f.stopErr[12] = boom
				f.onIdentity = func(pid int) {
					if pid == 12 {
						delete(f.procs, 11)
					}
				}
			},
			wantErr:   boom,
			wantCalls: []string{"stop 10", "stop 11", "stop 12", "cont 10"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := newSig(10, 11, 12)
			tt.setup(sig)
			c := New(sig, me, clock)
			err := c.Pause(context.Background(), target(10, 11, 12))
			if !reflect.DeepEqual(sig.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", sig.calls, tt.wantCalls)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				if got := c.Paused(); len(got) != 1 || !reflect.DeepEqual(got[0].Members, tt.wantMembers) {
					t.Errorf("Paused = %+v, want members %v", got, tt.wantMembers)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if reason(err) != "" {
				t.Errorf("race reported as refusal: %v", err)
			}
			if got := c.Paused(); len(got) != 0 {
				t.Errorf("failed pause recorded %+v", got)
			}
		})
	}
}

func TestPauseRollbackFailureIsReported(t *testing.T) {
	boom, stuck := errors.New("boom"), errors.New("stuck")
	sig := newSig(10, 11, 12)
	sig.stopErr[12] = boom
	sig.contErr[11] = stuck
	c := New(sig, me, clock)
	err := c.Pause(context.Background(), target(10, 11, 12))
	if !errors.Is(err, boom) || !errors.Is(err, stuck) || !strings.Contains(err.Error(), "rollback was incomplete") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"stop 10", "stop 11", "stop 12", "cont 11", "cont 10"}; !reflect.DeepEqual(sig.calls, want) {
		t.Errorf("calls = %v, want %v", sig.calls, want)
	}
}

func TestPauseCancelled(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		sig := newSig(10, 11)
		c := New(sig, me, clock)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Pause(ctx, target(10, 11)); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if len(sig.calls) != 0 || len(c.Paused()) != 0 {
			t.Errorf("calls %v, paused %v", sig.calls, c.Paused())
		}
	})
	t.Run("mid-pause rolls back", func(t *testing.T) {
		sig := newSig(10, 11, 12)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cancel once the second member is being verified; rollback must
		// still verify and continue despite the cancelled context.
		sig.onIdentity = func(pid int) {
			if pid == 11 {
				cancel()
			}
		}
		c := New(sig, me, clock)
		// Identity of 11 succeeds (the fake ignores ctx), so 11 is stopped
		// and the check before 12 observes the cancellation.
		err := c.Pause(ctx, target(10, 11, 12))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if want := []string{"stop 10", "stop 11", "cont 11", "cont 10"}; !reflect.DeepEqual(sig.calls, want) {
			t.Errorf("calls = %v, want %v", sig.calls, want)
		}
		if len(c.Paused()) != 0 {
			t.Errorf("cancelled pause recorded %v", c.Paused())
		}
	})
}

func TestResume(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		setup     func(*fakeSig)
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "gone member skipped silently",
			setup:     func(f *fakeSig) { delete(f.procs, 11) },
			wantCalls: []string{"cont 12", "cont 10"},
		},
		{
			name:      "reused pid never continued",
			setup:     func(f *fakeSig) { f.procs[12] = ident{epoch.Add(time.Minute), me} },
			wantCalls: []string{"cont 11", "cont 10"},
		},
		{
			name:      "continue reporting gone is silent",
			setup:     func(f *fakeSig) { f.contErr[11] = domain.ErrProcessGone },
			wantCalls: []string{"cont 12", "cont 11", "cont 10"},
		},
		{
			name:      "continue failure is collected and the rest still resume",
			setup:     func(f *fakeSig) { f.contErr[11] = boom },
			wantErr:   boom,
			wantCalls: []string{"cont 12", "cont 11", "cont 10"},
		},
		{
			name:      "identity failure is collected and that member skipped",
			setup:     func(f *fakeSig) { f.idErr[12] = boom },
			wantErr:   boom,
			wantCalls: []string{"cont 11", "cont 10"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := newSig(10, 11, 12)
			c := New(sig, me, clock)
			if err := c.Pause(context.Background(), target(10, 11, 12)); err != nil {
				t.Fatal(err)
			}
			sig.reset()
			tt.setup(sig)
			// A cancelled context must not prevent resuming.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := c.Resume(ctx, key(10))
			if !reflect.DeepEqual(sig.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", sig.calls, tt.wantCalls)
			}
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if len(c.Paused()) != 0 {
				t.Errorf("record kept after resume: %v", c.Paused())
			}
		})
	}
}

func TestResumeAll(t *testing.T) {
	boom := errors.New("boom")
	sig := newSig(10, 11, 20, 21, 30)
	c := New(sig, me, clock)
	for _, tg := range []process.WorkloadTarget{target(30), target(10, 11), target(20, 21)} {
		if err := c.Pause(context.Background(), tg); err != nil {
			t.Fatal(err)
		}
	}
	sig.reset()
	sig.contErr[21] = boom
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown: the context is typically already done
	errs := c.ResumeAll(ctx)
	if len(errs) != 1 || !errors.Is(errs[0], boom) || !strings.Contains(errs[0].Error(), "pid:20") {
		t.Errorf("errs = %v", errs)
	}
	if want := []string{"cont 11", "cont 10", "cont 21", "cont 20", "cont 30"}; !reflect.DeepEqual(sig.calls, want) {
		t.Errorf("calls = %v, want %v", sig.calls, want)
	}
	if len(c.Paused()) != 0 {
		t.Errorf("Paused = %v", c.Paused())
	}
	if errs := c.ResumeAll(context.Background()); errs != nil {
		t.Errorf("empty ResumeAll = %v", errs)
	}
}

func TestPausedIsSortedCopy(t *testing.T) {
	sig := newSig(30, 10, 11, 20)
	c := New(sig, me, clock)
	for _, tg := range []process.WorkloadTarget{target(30), target(10, 11), target(20)} {
		if err := c.Pause(context.Background(), tg); err != nil {
			t.Fatal(err)
		}
	}
	got := c.Paused()
	if len(got) != 3 || got[0].Key != key(10) || got[1].Key != key(20) || got[2].Key != key(30) {
		t.Fatalf("Paused = %+v", got)
	}
	got[0].Members[0] = key(99)
	if again := c.Paused(); again[0].Members[0] != key(10) {
		t.Error("mutating Paused changed the controller")
	}
}

func TestForget(t *testing.T) {
	sig := newSig(10, 11, 20, 21)
	c := New(sig, me, clock)
	for _, tg := range []process.WorkloadTarget{target(10, 11), target(20, 21)} {
		if err := c.Pause(context.Background(), tg); err != nil {
			t.Fatal(err)
		}
	}
	sig.reset()
	// Workload 10 is entirely gone; workload 20 lost its top but 21 is
	// still alive and stopped, so it must stay resumable.
	alive := map[domain.ProcessKey]bool{key(21): true}
	c.Forget(func(k domain.ProcessKey) bool { return alive[k] })
	want := []PausedWorkload{{Key: key(20), Name: "make", Owner: "claude-1", Members: keys(21), Since: clock()}}
	if got := c.Paused(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paused = %+v, want %+v", got, want)
	}
	if len(sig.calls) != 0 {
		t.Errorf("Forget signaled %v", sig.calls)
	}
	if err := c.Resume(context.Background(), key(20)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cont 21"}; !reflect.DeepEqual(sig.calls, want) {
		t.Errorf("calls = %v, want %v", sig.calls, want)
	}
}

func TestStateRoundTrip(t *testing.T) {
	boom := errors.New("boom")
	sig := newSig(10, 11, 12, 20, 21)
	c := New(sig, me, clock)
	for _, tg := range []process.WorkloadTarget{target(10, 11, 12), target(20, 21)} {
		if err := c.Pause(context.Background(), tg); err != nil {
			t.Fatal(err)
		}
	}
	data, err := c.MarshalState()
	if err != nil {
		t.Fatal(err)
	}

	// After a crash: 11 exited, 12's PID was reused, 20 is now another
	// user's, and continuing 21 fails.
	delete(sig.procs, 11)
	sig.procs[12] = ident{epoch.Add(time.Minute), me}
	sig.procs[20] = ident{key(20).Start, 0}
	sig.contErr[21] = boom
	sig.reset()

	fresh := New(sig, me, clock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errs := fresh.RestoreAndResume(ctx, data)
	if want := []string{"cont 10", "cont 21"}; !reflect.DeepEqual(sig.calls, want) {
		t.Errorf("calls = %v, want %v", sig.calls, want)
	}
	if len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Errorf("errs = %v", errs)
	}
	if len(fresh.Paused()) != 0 {
		t.Errorf("restore recorded %v", fresh.Paused())
	}
	for _, call := range sig.calls {
		if strings.HasPrefix(call, "stop") {
			t.Errorf("restore paused something: %v", sig.calls)
		}
	}
}

func TestRestoreMalformed(t *testing.T) {
	member := func(pid int, start string) string {
		return fmt.Sprintf(`{"version":1,"paused":[{"process":{"pid":10,"start":"2026-01-01T00:00:10Z"},"members":[{"pid":%d,"start":%q}]}]}`, pid, start)
	}
	many := func(field string, n int) string {
		items := make([]string, n)
		for i := range items {
			if field == "paused" {
				items[i] = `{"members":[]}`
			} else {
				items[i] = `{"pid":10,"start":"2026-01-01T00:00:10Z"}`
			}
		}
		if field == "paused" {
			return `{"version":1,"paused":[` + strings.Join(items, ",") + `]}`
		}
		return `{"version":1,"paused":[{"members":[` + strings.Join(items, ",") + `]}]}`
	}
	tests := map[string]string{
		"empty":            ``,
		"not json":         `garbage`,
		"null":             `null`,
		"wrong version":    `{"version":2,"paused":[]}`,
		"unknown field":    `{"version":1,"paused":[],"extra":true}`,
		"trailing data":    member(10, "2026-01-01T00:00:10Z") + `{}`,
		"wrong type":       `{"version":1,"paused":{}}`,
		"pid 0":            member(0, "2026-01-01T00:00:10Z"),
		"pid -1":           member(-1, "2026-01-01T00:00:10Z"),
		"pid 1":            member(1, "2026-01-01T00:00:10Z"),
		"zero start":       member(10, "0001-01-01T00:00:00Z"),
		"bad start":        member(10, "yesterday"),
		"too many records": many("paused", MaxPaused+1),
		"too many members": many("members", MaxMembers+1),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			sig := newSig(10)
			c := New(sig, me, clock)
			errs := c.RestoreAndResume(context.Background(), []byte(data))
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), "malformed state") {
				t.Errorf("errs = %v", errs)
			}
			if len(sig.calls) != 0 {
				t.Errorf("malformed state signaled %v", sig.calls)
			}
		})
	}
	// Sanity: the well-formed variant of the fixture does resume.
	sig := newSig(10)
	if errs := New(sig, me, clock).RestoreAndResume(context.Background(), []byte(member(10, "2026-01-01T00:00:10Z"))); errs != nil {
		t.Fatal(errs)
	}
	if want := []string{"cont 10"}; !reflect.DeepEqual(sig.calls, want) {
		t.Errorf("calls = %v, want %v", sig.calls, want)
	}
}

func TestRefusedErrorMessage(t *testing.T) {
	if got := (&RefusedError{Reason: ReasonNotHeavy}).Error(); got != "control: refused: not_heavy" {
		t.Errorf("Error() = %q", got)
	}
}
