package control

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

const me = 501

// epoch is in time.Local, like the Start values samplers report.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Local()

func clock() time.Time { return epoch.Add(time.Hour) }

func key(pid int) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: epoch.Add(time.Duration(pid) * time.Second)}
}

type ident struct {
	start time.Time
	uid   uint32
}

// fakeSig is a scripted Signaler. Processes absent from procs are gone. Like
// the platform, Stop and Continue verify identity and report a mismatch as
// gone.
type fakeSig struct {
	mu      sync.Mutex
	procs   map[int]ident
	stopErr map[int]error
	contErr map[int]error
	goneErr map[int]error
	// onCall runs before each Stop or Continue, to script races.
	onCall func(op string, pid int)
	// calls records every Stop and Continue, e.g. "stop 10" or "cont 11",
	// plus anything tests append through note.
	calls []string
}

// newSig returns a fake where each pid is alive, owned by me, and started
// at key(pid).Start.
func newSig(pids ...int) *fakeSig {
	f := &fakeSig{procs: map[int]ident{}, stopErr: map[int]error{}, contErr: map[int]error{}, goneErr: map[int]error{}}
	for _, p := range pids {
		f.procs[p] = ident{key(p).Start, me}
	}
	return f
}

func (f *fakeSig) signal(op string, errs map[int]error, k domain.ProcessKey, uid uint32) error {
	if f.onCall != nil {
		f.onCall(op, k.PID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %d", op, k.PID))
	if err := errs[k.PID]; err != nil {
		return err
	}
	if p, ok := f.procs[k.PID]; !ok || !p.start.Equal(k.Start) || p.uid != uid {
		return fmt.Errorf("pid %d: %w", k.PID, domain.ErrProcessGone)
	}
	return nil
}

func (f *fakeSig) Stop(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return f.signal("stop", f.stopErr, k, uid)
}

func (f *fakeSig) Continue(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return f.signal("cont", f.contErr, k, uid)
}

func (f *fakeSig) Gone(_ context.Context, k domain.ProcessKey) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("gone? %d", k.PID))
	if err := f.goneErr[k.PID]; err != nil {
		return false, err
	}
	p, ok := f.procs[k.PID]
	return !ok || !p.start.Equal(k.Start), nil
}

func (f *fakeSig) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSig) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeSig) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

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
	c := New(sig, me, clock, nil)
	if err := c.Pause(context.Background(), target(10, 11, 12)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop 10", "stop 11", "stop 12"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("pause calls = %v, want %v", sig.log(), want)
	}
	want := []PausedWorkload{{Key: key(10), Name: "make", Owner: "claude-1", Members: keys(10, 11, 12), Since: clock(), State: StatePaused}}
	if got := c.Paused(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paused = %+v, want %+v", got, want)
	}

	sig.reset()
	if err := c.Resume(context.Background(), key(10)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cont 12", "cont 11", "cont 10"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("resume calls = %v, want %v", sig.log(), want)
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
			c := New(sig, me, clock, nil)
			tg := target(10, 11)
			tt.mutate(&tg)
			err := c.Pause(context.Background(), tg)
			if r := reason(err); r != tt.want {
				t.Fatalf("reason = %q (err %v), want %q", r, err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q lacks reason", err)
			}
			if len(sig.log()) != 0 || len(c.Paused()) != 0 {
				t.Errorf("refusal signaled %v / recorded %v", sig.log(), c.Paused())
			}
		})
	}
}

func TestPauseAlreadyPaused(t *testing.T) {
	sig := newSig(10, 11, 20)
	c := New(sig, me, clock, nil)
	if err := c.Pause(context.Background(), target(10, 11)); err != nil {
		t.Fatal(err)
	}
	sig.reset()
	for _, tg := range []process.WorkloadTarget{target(10, 11), target(11), target(20, 11)} {
		if r := reason(c.Pause(context.Background(), tg)); r != ReasonAlreadyPaused {
			t.Errorf("pause %v reason = %q, want %q", tg.Key, r, ReasonAlreadyPaused)
		}
	}
	if len(sig.log()) != 0 {
		t.Errorf("refusals signaled %v", sig.log())
	}
}

func TestPauseLimit(t *testing.T) {
	sig := newSig()
	c := New(sig, me, clock, nil)
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
	if n := len(sig.log()); n != MaxPaused {
		t.Errorf("signals = %d, want %d", n, MaxPaused)
	}
}

func TestPauseMaxMembersAccepted(t *testing.T) {
	pids := make([]int, MaxMembers)
	for i := range pids {
		pids[i] = i + 2
	}
	c := New(newSig(pids...), me, clock, nil)
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
			name:      "top exited before pause aborts",
			setup:     func(f *fakeSig) { delete(f.procs, 10) },
			wantErr:   domain.ErrProcessGone,
			wantCalls: []string{"stop 10"},
		},
		{
			name:      "top pid reused aborts",
			setup:     func(f *fakeSig) { f.procs[10] = ident{epoch.Add(time.Minute), me} },
			wantErr:   domain.ErrProcessGone,
			wantCalls: []string{"stop 10"},
		},
		{
			name:      "top pid now another user aborts",
			setup:     func(f *fakeSig) { f.procs[10] = ident{key(10).Start, 0} },
			wantErr:   domain.ErrProcessGone,
			wantCalls: []string{"stop 10"},
		},
		{
			name: "child exits mid-pause is skipped",
			setup: func(f *fakeSig) {
				f.onCall = func(op string, pid int) {
					if op == "stop" && pid == 11 {
						f.mu.Lock()
						delete(f.procs, 11)
						f.mu.Unlock()
					}
				}
			},
			wantCalls:   []string{"stop 10", "stop 11", "stop 12"},
			wantMembers: keys(10, 12),
		},
		{
			name:        "child pid reuse is skipped",
			setup:       func(f *fakeSig) { f.procs[11] = ident{epoch.Add(time.Minute), me} },
			wantCalls:   []string{"stop 10", "stop 11", "stop 12"},
			wantMembers: keys(10, 12),
		},
		{
			name:        "child pid now foreign uid is skipped",
			setup:       func(f *fakeSig) { f.procs[12] = ident{key(12).Start, 0} },
			wantCalls:   []string{"stop 10", "stop 11", "stop 12"},
			wantMembers: keys(10, 11),
		},
		{
			name:      "stop failure rolls back in reverse order",
			setup:     func(f *fakeSig) { f.stopErr[12] = boom },
			wantErr:   boom,
			wantCalls: []string{"stop 10", "stop 11", "stop 12", "cont 11", "cont 10"},
		},
		{
			name:      "first child failure rolls back the top",
			setup:     func(f *fakeSig) { f.stopErr[11] = boom },
			wantErr:   boom,
			wantCalls: []string{"stop 10", "stop 11", "cont 10"},
		},
		{
			name: "rollback of a member that exited counts as done",
			setup: func(f *fakeSig) {
				f.stopErr[12] = boom
				f.onCall = func(op string, pid int) {
					if op == "stop" && pid == 12 {
						f.mu.Lock()
						delete(f.procs, 11)
						f.mu.Unlock()
					}
				}
			},
			wantErr:   boom,
			wantCalls: []string{"stop 10", "stop 11", "stop 12", "cont 11", "cont 10"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := newSig(10, 11, 12)
			tt.setup(sig)
			c := New(sig, me, clock, nil)
			err := c.Pause(context.Background(), target(10, 11, 12))
			if got := sig.log(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", got, tt.wantCalls)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				got := c.Paused()
				if len(got) != 1 || !reflect.DeepEqual(got[0].Members, tt.wantMembers) || got[0].State != StatePaused {
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

// recorder captures every onChange state, decoded.
type recorder struct {
	sig    *fakeSig // if set, each change is also noted in its call log
	states [][]PausedWorkload
	fail   error // if set, every save fails with it
}

func (r *recorder) onChange(b []byte) error {
	if r.fail != nil {
		return r.fail
	}
	if b == nil {
		r.states = append(r.states, nil)
		if r.sig != nil {
			r.sig.note("state empty")
		}
		return nil
	}
	s, err := parseState(b)
	if err != nil {
		panic(err)
	}
	r.states = append(r.states, s.Paused)
	if r.sig != nil {
		var parts []string
		for _, p := range s.Paused {
			parts = append(parts, fmt.Sprintf("%d:%s:%v", p.Key.PID, p.State, pidsOf(p.Members)))
		}
		r.sig.note("state " + strings.Join(parts, ","))
	}
	return nil
}

func (r *recorder) last() []PausedWorkload { return r.states[len(r.states)-1] }

func pidsOf(ks []domain.ProcessKey) []int {
	out := make([]int, len(ks))
	for i, k := range ks {
		out[i] = k.PID
	}
	return out
}

func TestPausePersistsBeforeSignaling(t *testing.T) {
	sig := newSig(10, 12) // 11 exits before it is stopped
	rec := &recorder{sig: sig}
	c := New(sig, me, clock, rec.onChange)
	if err := c.Pause(context.Background(), target(10, 11, 12)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"state 10:pausing:[10 11 12]",
		"stop 10", "stop 11", "stop 12",
		"state 10:paused:[10 12]",
	}
	if got := sig.log(); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	sig.reset()
	if err := c.Resume(context.Background(), key(10)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cont 12", "cont 10", "state empty"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("resume order = %v, want %v", sig.log(), want)
	}
}

func TestPauseFailedRollbackStaysRecorded(t *testing.T) {
	boom, stuck := errors.New("boom"), errors.New("stuck")
	sig := newSig(10, 11, 12)
	sig.stopErr[12] = boom
	sig.contErr[11] = stuck
	rec := &recorder{}
	c := New(sig, me, clock, rec.onChange)
	err := c.Pause(context.Background(), target(10, 11, 12))
	if !errors.Is(err, boom) || !errors.Is(err, stuck) || !strings.Contains(err.Error(), "rollback was incomplete") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"stop 10", "stop 11", "stop 12", "cont 11", "cont 10"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
	want := []PausedWorkload{{Key: key(10), Name: "make", Owner: "claude-1", Members: keys(11), Since: clock(), State: StateResuming}}
	if got := c.Paused(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paused = %+v, want %+v", got, want)
	}
	if got := rec.last(); !reflect.DeepEqual(got, want) {
		t.Errorf("persisted = %+v, want %+v", got, want)
	}

	// The tick retries until the member resumes.
	sig.reset()
	delete(sig.contErr, 11)
	if errs := c.RetryResumes(context.Background()); errs != nil {
		t.Fatal(errs)
	}
	if want := []string{"cont 11"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("retry calls = %v, want %v", sig.log(), want)
	}
	if len(c.Paused()) != 0 || rec.last() != nil {
		t.Errorf("record kept after retry: %v / %v", c.Paused(), rec.last())
	}
}

func TestPauseCancelled(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		sig := newSig(10, 11)
		rec := &recorder{}
		c := New(sig, me, clock, rec.onChange)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Pause(ctx, target(10, 11)); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if len(sig.log()) != 0 || len(c.Paused()) != 0 || len(rec.states) != 0 {
			t.Errorf("calls %v, paused %v, states %v", sig.log(), c.Paused(), rec.states)
		}
	})
	t.Run("mid-pause rolls back", func(t *testing.T) {
		sig := newSig(10, 11, 12)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cancel while the second member is being stopped; rollback must
		// still continue despite the cancelled context.
		sig.onCall = func(op string, pid int) {
			if op == "stop" && pid == 11 {
				cancel()
			}
		}
		c := New(sig, me, clock, nil)
		err := c.Pause(ctx, target(10, 11, 12))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if want := []string{"stop 10", "stop 11", "cont 11", "cont 10"}; !reflect.DeepEqual(sig.log(), want) {
			t.Errorf("calls = %v, want %v", sig.log(), want)
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
		wantLeft  []domain.ProcessKey
		wantCalls []string
	}{
		{
			name:      "gone member counts as done",
			setup:     func(f *fakeSig) { delete(f.procs, 11) },
			wantCalls: []string{"cont 12", "cont 11", "cont 10"},
		},
		{
			name:      "reused pid counts as done",
			setup:     func(f *fakeSig) { f.procs[12] = ident{epoch.Add(time.Minute), me} },
			wantCalls: []string{"cont 12", "cont 11", "cont 10"},
		},
		{
			name:      "continue failure keeps that member and the rest still resume",
			setup:     func(f *fakeSig) { f.contErr[11] = boom },
			wantErr:   boom,
			wantLeft:  keys(11),
			wantCalls: []string{"cont 12", "cont 11", "cont 10"},
		},
		{
			name:      "several failures keep parent-first order",
			setup:     func(f *fakeSig) { f.contErr[10], f.contErr[12] = boom, boom },
			wantErr:   boom,
			wantLeft:  keys(10, 12),
			wantCalls: []string{"cont 12", "cont 11", "cont 10"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := newSig(10, 11, 12)
			rec := &recorder{}
			c := New(sig, me, clock, rec.onChange)
			if err := c.Pause(context.Background(), target(10, 11, 12)); err != nil {
				t.Fatal(err)
			}
			sig.reset()
			tt.setup(sig)
			// A cancelled context must not prevent resuming.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := c.Resume(ctx, key(10))
			if !reflect.DeepEqual(sig.log(), tt.wantCalls) {
				t.Errorf("calls = %v, want %v", sig.log(), tt.wantCalls)
			}
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			got := c.Paused()
			if tt.wantLeft == nil {
				if len(got) != 0 || rec.last() != nil {
					t.Errorf("record kept after resume: %v", got)
				}
				return
			}
			want := []PausedWorkload{{Key: key(10), Name: "make", Owner: "claude-1", Members: tt.wantLeft, Since: clock(), State: StateResuming}}
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(rec.last(), want) {
				t.Errorf("Paused = %+v, persisted %+v, want %+v", got, rec.last(), want)
			}
		})
	}
}

func TestResumeTransientFailureIsRetried(t *testing.T) {
	transient := errors.New("ps: fork: resource temporarily unavailable")
	sig := newSig(10, 11, 12)
	rec := &recorder{}
	c := New(sig, me, clock, rec.onChange)
	if err := c.Pause(context.Background(), target(10, 11, 12)); err != nil {
		t.Fatal(err)
	}
	sig.contErr[11] = transient
	if err := c.Resume(context.Background(), key(10)); !errors.Is(err, transient) {
		t.Fatalf("err = %v", err)
	}
	if got := c.Paused(); len(got) != 1 || got[0].State != StateResuming || !reflect.DeepEqual(got[0].Members, keys(11)) {
		t.Fatalf("Paused = %+v", got)
	}

	// Still failing: the record stays.
	if errs := c.RetryResumes(context.Background()); len(errs) != 1 || !errors.Is(errs[0], transient) {
		t.Fatalf("errs = %v", errs)
	}
	if len(c.Paused()) != 1 {
		t.Fatalf("Paused = %+v", c.Paused())
	}

	// Paused (not resuming) records are left alone by retries.
	sig.procs[20] = ident{key(20).Start, me}
	if err := c.Pause(context.Background(), target(20)); err != nil {
		t.Fatal(err)
	}
	delete(sig.contErr, 11)
	sig.reset()
	if errs := c.RetryResumes(context.Background()); errs != nil {
		t.Fatal(errs)
	}
	if want := []string{"cont 11"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
	if got := c.Paused(); len(got) != 1 || got[0].Key != key(20) || got[0].State != StatePaused {
		t.Errorf("Paused = %+v", got)
	}
	if got := rec.last(); len(got) != 1 || got[0].Key != key(20) {
		t.Errorf("persisted = %+v", got)
	}
}

func TestResumeAll(t *testing.T) {
	boom := errors.New("boom")
	sig := newSig(10, 11, 20, 21, 30)
	rec := &recorder{}
	c := New(sig, me, clock, rec.onChange)
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
	if want := []string{"cont 11", "cont 10", "cont 21", "cont 20", "cont 30"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
	// The failed workload is kept, and so is the state that will be left
	// on disk for the next daemon.
	want := []PausedWorkload{{Key: key(20), Name: "make", Owner: "claude-1", Members: keys(21), Since: clock(), State: StateResuming}}
	if got := c.Paused(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paused = %+v, want %+v", got, want)
	}
	if got := rec.last(); !reflect.DeepEqual(got, want) {
		t.Errorf("persisted = %+v, want %+v", got, want)
	}
	data, err := c.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	if s, err := parseState(data); err != nil || !reflect.DeepEqual(s.Paused, want) {
		t.Errorf("MarshalState = %s, %v", data, err)
	}

	delete(sig.contErr, 21)
	if errs := c.ResumeAll(context.Background()); errs != nil {
		t.Errorf("second ResumeAll = %v", errs)
	}
	if len(c.Paused()) != 0 || rec.last() != nil {
		t.Errorf("Paused = %v", c.Paused())
	}
	if errs := c.ResumeAll(context.Background()); errs != nil {
		t.Errorf("empty ResumeAll = %v", errs)
	}
}

func TestPausedIsSortedCopy(t *testing.T) {
	sig := newSig(30, 10, 11, 20)
	c := New(sig, me, clock, nil)
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

// blockingSig blocks Stop until release is closed, reporting entry on
// entered.
type blockingSig struct {
	*fakeSig
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSig) Stop(ctx context.Context, k domain.ProcessKey, uid uint32) error {
	close(b.entered)
	<-b.release
	return b.fakeSig.Stop(ctx, k, uid)
}

func TestPausedDoesNotWaitForSignaling(t *testing.T) {
	sig := &blockingSig{fakeSig: newSig(10), entered: make(chan struct{}), release: make(chan struct{})}
	c := New(sig, me, clock, nil)
	done := make(chan error, 1)
	go func() { done <- c.Pause(context.Background(), target(10)) }()
	<-sig.entered

	got := make(chan []PausedWorkload, 1)
	go func() { got <- c.Paused() }()
	select {
	case p := <-got:
		if len(p) != 1 || p[0].State != StatePausing {
			t.Errorf("Paused mid-pause = %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Paused blocked behind a slow Stop")
	}
	if _, err := c.MarshalState(); err != nil {
		t.Error(err)
	}
	// Tick work skips the round instead of waiting.
	if errs := c.RetryResumes(context.Background()); errs != nil {
		t.Errorf("RetryResumes = %v", errs)
	}
	c.Forget(context.Background(), func(domain.ProcessKey) bool { return false })

	close(sig.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p := c.Paused(); len(p) != 1 || p[0].State != StatePaused {
		t.Errorf("Paused = %+v", p)
	}
}

// hangingSig never answers until ctx is done.
type hangingSig struct{}

func (hangingSig) Stop(ctx context.Context, _ domain.ProcessKey, _ uint32) error {
	<-ctx.Done()
	return ctx.Err()
}

func (hangingSig) Continue(ctx context.Context, _ domain.ProcessKey, _ uint32) error {
	<-ctx.Done()
	return ctx.Err()
}

func (hangingSig) Gone(ctx context.Context, _ domain.ProcessKey) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func TestCallDeadline(t *testing.T) {
	data := []byte(`{"version":2,"paused":[{"process":{"pid":10,"start":"2026-01-01T00:00:10Z"},"name":"make","owner":"claude-1",` +
		`"members":[{"pid":10,"start":"2026-01-01T00:00:10Z"},{"pid":11,"start":"2026-01-01T00:00:11Z"}],"since":"2026-01-01T01:00:00Z","state":"paused"}]}`)
	c := New(hangingSig{}, me, clock, nil, WithCallTimeout(20*time.Millisecond))
	// Even a cancelled context leaves each call its own deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan []error, 1)
	go func() { done <- c.RestoreAndResume(ctx, data) }()
	select {
	case errs := <-done:
		if len(errs) != 1 || !errors.Is(errs[0], context.DeadlineExceeded) {
			t.Errorf("errs = %v", errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hung call did not time out")
	}
	if got := c.Paused(); len(got) != 1 || len(got[0].Members) != 2 {
		t.Errorf("Paused = %+v", got)
	}

	if err := c.Pause(context.Background(), target(20)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Pause err = %v", err)
	}
	if errs := c.RetryResumes(context.Background()); len(errs) != 1 {
		t.Errorf("RetryResumes = %v", errs)
	}
	c.Forget(context.Background(), func(domain.ProcessKey) bool { return false })
	if got := c.Paused(); len(got) != 1 || len(got[0].Members) != 2 {
		t.Errorf("Paused after Forget = %+v", got)
	}
}

func TestForget(t *testing.T) {
	boom := errors.New("ps failed")
	sig := newSig(10, 11, 20, 21, 30, 31)
	rec := &recorder{}
	c := New(sig, me, clock, rec.onChange)
	for _, tg := range []process.WorkloadTarget{target(10, 11), target(20, 21), target(30, 31)} {
		if err := c.Pause(context.Background(), tg); err != nil {
			t.Fatal(err)
		}
	}
	sig.reset()
	// Workload 10 is entirely gone; workload 20 lost its top but 21 is
	// still alive and stopped, so it must stay resumable. Workload 30 is
	// missing from the sample but the OS says it is still there.
	delete(sig.procs, 10)
	delete(sig.procs, 11)
	delete(sig.procs, 20)
	alive := map[domain.ProcessKey]bool{key(21): true}
	c.Forget(context.Background(), func(k domain.ProcessKey) bool { return alive[k] })
	want := []PausedWorkload{
		{Key: key(20), Name: "make", Owner: "claude-1", Members: keys(21), Since: clock(), State: StatePaused},
		{Key: key(30), Name: "make", Owner: "claude-1", Members: keys(30, 31), Since: clock(), State: StatePaused},
	}
	if got := c.Paused(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paused = %+v, want %+v", got, want)
	}
	if got := rec.last(); !reflect.DeepEqual(got, want) {
		t.Errorf("persisted = %+v, want %+v", got, want)
	}
	for _, call := range sig.log() {
		if !strings.HasPrefix(call, "gone? ") {
			t.Errorf("Forget signaled: %v", sig.log())
		}
	}

	// A member whose state cannot be confirmed is kept, and so is every
	// member after it in that round.
	delete(sig.procs, 30)
	delete(sig.procs, 31)
	sig.goneErr[30] = boom
	sig.reset()
	c.Forget(context.Background(), func(k domain.ProcessKey) bool { return alive[k] })
	if want := []string{"gone? 30"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
	if got := c.Paused(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paused = %+v, want %+v", got, want)
	}

	delete(sig.goneErr, 30)
	c.Forget(context.Background(), func(k domain.ProcessKey) bool { return alive[k] })
	if got := c.Paused(); len(got) != 1 || got[0].Key != key(20) {
		t.Errorf("Paused = %+v", got)
	}

	sig.reset()
	if err := c.Resume(context.Background(), key(20)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cont 21"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
}

func TestStateRoundTrip(t *testing.T) {
	boom := errors.New("boom")
	sig := newSig(10, 11, 12, 20, 21)
	c := New(sig, me, clock, nil)
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

	rec := &recorder{}
	fresh := New(sig, me, clock, rec.onChange)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errs := fresh.RestoreAndResume(ctx, data)
	if want := []string{"cont 12", "cont 11", "cont 10", "cont 21", "cont 20"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
	if len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Errorf("errs = %v", errs)
	}
	// The failed member stays recorded: restore re-records on purpose.
	want := []PausedWorkload{{Key: key(20), Name: "make", Owner: "claude-1", Members: keys(21), Since: clock(), State: StateResuming}}
	if got := fresh.Paused(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paused = %+v, want %+v", got, want)
	}
	if len(rec.states) == 0 || !reflect.DeepEqual(rec.last(), want) {
		t.Fatalf("persisted = %+v", rec.states)
	}
	// Before any signal, every record was persisted as resuming.
	if first := rec.states[0]; len(first) != 2 || first[0].State != StateResuming || first[1].State != StateResuming {
		t.Errorf("first persisted state = %+v", first)
	}
	for _, call := range sig.log() {
		if strings.HasPrefix(call, "stop") {
			t.Errorf("restore paused something: %v", sig.log())
		}
	}

	delete(sig.contErr, 21)
	if errs := fresh.RetryResumes(context.Background()); errs != nil {
		t.Fatal(errs)
	}
	if len(fresh.Paused()) != 0 || rec.last() != nil {
		t.Errorf("record kept after retry: %v", fresh.Paused())
	}
}

func TestRestorePausingRecord(t *testing.T) {
	// A crash mid-pause leaves every intended member; continuing members
	// that were never stopped is harmless.
	data := []byte(`{"version":2,"paused":[{"process":{"pid":10,"start":"2026-01-01T00:00:10Z"},"name":"make","owner":"claude-1",` +
		`"members":[{"pid":10,"start":"2026-01-01T00:00:10Z"},{"pid":11,"start":"2026-01-01T00:00:11Z"}],"since":"2026-01-01T01:00:00Z","state":"pausing"}]}`)
	sig := newSig(10, 11)
	c := New(sig, me, clock, nil)
	if errs := c.RestoreAndResume(context.Background(), data); errs != nil {
		t.Fatal(errs)
	}
	if want := []string{"cont 11", "cont 10"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
	if len(c.Paused()) != 0 {
		t.Errorf("Paused = %+v", c.Paused())
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
		"wrong version":    `{"version":3,"paused":[]}`,
		"unknown state":    `{"version":2,"paused":[{"members":[],"state":"frozen"}]}`,
		"v1 with state":    `{"version":1,"paused":[{"members":[],"state":"paused"}]}`,
		"duplicate record": `{"version":2,"paused":[{"members":[],"state":"paused"},{"members":[],"state":"paused"}]}`,
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
			c := New(sig, me, clock, nil)
			errs := c.RestoreAndResume(context.Background(), []byte(data))
			if len(errs) != 1 || !errors.Is(errs[0], ErrMalformedState) || !strings.Contains(errs[0].Error(), "malformed state") {
				t.Errorf("errs = %v", errs)
			}
			if len(sig.log()) != 0 || len(c.Paused()) != 0 {
				t.Errorf("malformed state signaled %v / recorded %v", sig.log(), c.Paused())
			}
		})
	}
	// Sanity: the well-formed variant of the fixture (version 1, still
	// accepted) does resume.
	sig := newSig(10)
	if errs := New(sig, me, clock, nil).RestoreAndResume(context.Background(), []byte(member(10, "2026-01-01T00:00:10Z"))); errs != nil {
		t.Fatal(errs)
	}
	if want := []string{"cont 10"}; !reflect.DeepEqual(sig.log(), want) {
		t.Errorf("calls = %v, want %v", sig.log(), want)
	}
}

func TestRefusedErrorMessage(t *testing.T) {
	if got := (&RefusedError{Reason: ReasonNotHeavy}).Error(); got != "control: refused: not_heavy" {
		t.Errorf("Error() = %q", got)
	}
}

func TestPauseRefusedWhenStateCannotBeSaved(t *testing.T) {
	sig := newSig(10, 11)
	rec := &recorder{fail: errors.New("disk full")}
	c := New(sig, me, clock, rec.onChange)
	if err := c.Pause(context.Background(), target(10, 11)); err == nil || !strings.Contains(err.Error(), "cannot persist") {
		t.Fatalf("err = %v", err)
	}
	if calls := sig.log(); len(calls) != 0 {
		t.Errorf("signals sent without a durable record: %v", calls)
	}
	if len(c.Paused()) != 0 {
		t.Errorf("paused = %+v", c.Paused())
	}
}

func TestRetryStopsAtFirstHardError(t *testing.T) {
	sig := newSig(10, 11, 12)
	c := New(sig, me, clock, nil)
	if err := c.Pause(context.Background(), target(10, 11, 12)); err != nil {
		t.Fatal(err)
	}
	sig.mu.Lock()
	sig.contErr[11] = errors.New("ps: fork failed")
	sig.contErr[12] = errors.New("ps: fork failed")
	sig.calls = nil
	sig.mu.Unlock()
	if err := c.Resume(context.Background(), key(10)); err == nil {
		t.Fatal("resume with a failing member succeeded")
	}
	sig.mu.Lock()
	sig.calls = nil
	sig.mu.Unlock()
	if errs := c.RetryResumes(context.Background()); len(errs) != 1 {
		t.Fatalf("retry errs = %v", errs)
	}
	// 11 and 12 are left. Child-first, 12 is tried first and fails, so the
	// round ends and 11 waits for the next tick.
	if got := sig.log(); !reflect.DeepEqual(got, []string{"cont 12"}) {
		t.Errorf("retry calls = %v, want only cont 12", got)
	}
	if p := c.Paused(); len(p) != 1 || !reflect.DeepEqual(p[0].Members, keys(11, 12)) {
		t.Errorf("paused = %+v", p)
	}
}
