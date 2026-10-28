package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

// fakeIdent answers Identify from a table. A PID it does not list is a
// live process of this user, started at time.Unix(pid, 0), whose parent is
// init.
type fakeIdent struct {
	mu    sync.Mutex
	procs map[int]domain.ProcessSample
	calls int
}

func (f *fakeIdent) Identify(_ context.Context, pid int) (domain.ProcessSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if p, ok := f.procs[pid]; ok {
		p.PID = pid
		return p, nil
	}
	return domain.ProcessSample{PID: pid, PPID: 1, UID: uint32(os.Getuid()), Start: time.Unix(int64(pid), 0)}, nil
}

// child is a live process of this user under ppid.
func child(ppid int) domain.ProcessSample {
	return domain.ProcessSample{PPID: ppid, UID: uint32(os.Getuid()), Start: time.Unix(1, 0)}
}

// leaseDaemon is a daemon that is never run: tests set its status and call
// handlers directly. Its callers are identified through ident.
func leaseDaemon(t *testing.T, st StatusResult) (*Daemon, *fakeIdent) {
	t.Helper()
	d := NewDaemon(config.Default(), fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.status = st
	ident := &fakeIdent{procs: map[int]domain.ProcessSample{}}
	d.ident = ident
	return d, ident
}

func oneSlot(limit int, heavy ...process.WorkloadView) StatusResult {
	return StatusResult{
		Pressure:  domain.PressureNormal,
		Agents:    []process.AgentView{{Key: domain.ProcessKey{PID: 10}, Agent: "claude-1"}},
		Heavy:     heavy,
		Scheduler: SchedulerStatus{Mode: scheduler.ModeEnforce, HeavyLimit: limit, Queued: []domain.ProcessKey{}},
	}
}

func acquireAs(t *testing.T, d *Daemon, ctx context.Context, p AcquireParams) (AcquireResult, error) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.acquire(ctx, raw)
	if err != nil {
		return AcquireResult{}, err
	}
	return v.(AcquireResult), nil
}

func eventsOf(d *Daemon, kind domain.EventKind) []domain.Event {
	return slices.DeleteFunc(d.events.Recent(0), func(e domain.Event) bool { return e.Kind != kind })
}

func TestAcquireConcurrentOneSlot(t *testing.T) {
	// One slot of two is taken; eight callers race for the other.
	running := process.WorkloadView{Key: domain.ProcessKey{PID: 11}, Name: "go", Owner: "claude-1"}
	d, _ := leaseDaemon(t, oneSlot(2, running))
	const callers = 8
	results := make([]AcquireResult, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			r, err := acquireAs(t, d, context.Background(), AcquireParams{PID: 100 + i})
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		})
	}
	wg.Wait()
	granted := 0
	for _, r := range results {
		if r.Granted {
			granted++
			if r.LeaseID == "" {
				t.Errorf("granted without a lease id: %+v", r)
			}
		} else if !strings.Contains(r.Reason, "1 running, 1 reserved, limit 2") {
			t.Errorf("refusal reason = %q", r.Reason)
		}
	}
	if granted != 1 {
		t.Fatalf("%d of %d concurrent acquires granted, want exactly 1", granted, callers)
	}
	if n := len(eventsOf(d, domain.EventLeaseGranted)); n != 1 {
		t.Errorf("%d lease_granted events, want 1", n)
	}
	st, _ := d.currentStatus(context.Background(), nil)
	if l := st.(StatusResult).Scheduler.Leases; len(l) != 1 {
		t.Fatalf("status leases = %+v, want 1", l)
	}
	// A repeated acquire by the holder returns its lease.
	var holder AcquireResult
	for i, r := range results {
		if r.Granted {
			if holder, _ = acquireAs(t, d, context.Background(), AcquireParams{PID: 100 + i}); !holder.Granted || holder.LeaseID != r.LeaseID {
				t.Errorf("holder re-acquire = %+v, want lease %s", holder, r.LeaseID)
			}
		}
	}
}

func TestAcquireCountsLeasesPerAgent(t *testing.T) {
	st := oneSlot(4)
	st.Scheduler.PerAgentLimit = 1
	d, ident := leaseDaemon(t, st)
	// The OS says both callers' parent is the agent (pid 10); its budget is
	// one. 104 runs under a shell (500) that the latest sample predates.
	ident.procs[100], ident.procs[101], ident.procs[104], ident.procs[500] = child(10), child(10), child(500), child(10)
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 100}); !r.Granted {
		t.Fatalf("first acquire refused: %s", r.Reason)
	}
	r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 101})
	if r.Granted || !strings.Contains(r.Reason, "claude-1 already runs 1 heavy jobs, 1 of them reserved") {
		t.Fatalf("second acquire for the same agent = %+v", r)
	}
	// An unattributed caller is not held to the agent's budget; one whose
	// shell is not sampled yet is attributed through the OS.
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 102}); !r.Granted {
		t.Fatalf("unattributed acquire refused: %s", r.Reason)
	}
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 104}); r.Granted {
		t.Fatalf("acquire below an unsampled shell granted: %+v", r)
	}
	if got := d.leases.owners(); !slices.Equal(got, []string{"claude-1", ""}) {
		t.Fatalf("lease owners = %q", got)
	}
}

func TestAcquireBounded(t *testing.T) {
	st := oneSlot(1)
	st.Scheduler.Mode = scheduler.ModeOff // no slot limit, so only the bound refuses
	d, _ := leaseDaemon(t, st)
	for i := range maxLeases {
		if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 1000 + i}); !r.Granted {
			t.Fatalf("acquire %d refused: %s", i, r.Reason)
		}
	}
	r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 5000})
	if r.Granted || r.Reason != ReasonTooManyLeases {
		t.Fatalf("acquire beyond the bound = %+v, want %s", r, ReasonTooManyLeases)
	}
	if len(d.leases.leases) != maxLeases {
		t.Fatalf("%d leases, want %d", len(d.leases.leases), maxLeases)
	}
}

func TestAcquireNested(t *testing.T) {
	// The only slot is taken by make (pid 50), whose recipe shell (51, not
	// sampled yet) runs throt run (52). Another lease holder (60) runs a
	// script whose throt run is 61.
	makeJob := process.WorkloadView{Key: domain.ProcessKey{PID: 50}, Name: "make", Owner: "claude-1"}
	d, ident := leaseDaemon(t, oneSlot(2, makeJob))
	d.leases.parents = map[int]int{50: 10, 10: 1, 60: 10}
	ident.procs[52], ident.procs[51], ident.procs[61], ident.procs[60] = child(51), child(50), child(60), child(10)
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 60}); !r.Granted || r.LeaseID == "" {
		t.Fatalf("lease for 60 = %+v", r)
	}
	// Every slot is now running or reserved; nested callers still start,
	// and reserve nothing.
	for _, pid := range []int{52, 61} {
		r, err := acquireAs(t, d, context.Background(), AcquireParams{PID: pid})
		if err != nil || !r.Granted || r.Reason != ReasonNested || r.LeaseID != "" {
			t.Fatalf("acquire %d = %+v, %v; want granted %s without a lease", pid, r, err, ReasonNested)
		}
	}
	if n := len(d.leases.leases); n != 1 {
		t.Fatalf("%d leases, want 1", n)
	}
	// A sibling of make is not nested and has to wait.
	ident.procs[70] = child(10)
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 70}); r.Granted {
		t.Fatalf("sibling acquire = %+v, want refused", r)
	}
}

func TestAcquireChecksCaller(t *testing.T) {
	d, ident := leaseDaemon(t, oneSlot(2))
	if _, err := acquireAs(t, d, context.Background(), AcquireParams{PID: 1}); err == nil || !strings.Contains(err.Error(), "pid must be > 1") {
		t.Fatalf("pid 1: err = %v", err)
	}
	// The kernel says the caller is pid 300, but it asks for pid 301.
	ctx := transport.WithPeerPID(context.Background(), 300)
	if _, err := acquireAs(t, d, ctx, AcquireParams{PID: 301}); err == nil || !strings.Contains(err.Error(), "caller pid 300 is not pid 301") {
		t.Fatalf("foreign pid: err = %v", err)
	}
	if r, err := acquireAs(t, d, ctx, AcquireParams{PID: 300}); err != nil || !r.Granted {
		t.Fatalf("own pid: %+v, %v", r, err)
	}
	// The key comes from the OS, never from the client.
	if l := d.leases.leases[0]; l.Process != (domain.ProcessKey{PID: 300, Start: time.Unix(300, 0)}) {
		t.Fatalf("lease key = %+v", l.Process)
	}
	ident.procs[400] = domain.ProcessSample{PPID: 1, UID: uint32(os.Getuid())} // no start time
	ident.procs[401] = domain.ProcessSample{PPID: 1, UID: uint32(os.Getuid()) + 1, Start: time.Unix(1, 0)}
	for pid, want := range map[int]string{400: "no start time", 401: "another user"} {
		if _, err := acquireAs(t, d, context.Background(), AcquireParams{PID: pid}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("acquire %d: err = %v, want %q", pid, err, want)
		}
	}
	d.ident = nil
	if _, err := acquireAs(t, d, context.Background(), AcquireParams{PID: 300}); err == nil || !strings.Contains(err.Error(), "cannot identify") {
		t.Errorf("without an identifier: err = %v", err)
	}
}

func TestAcquireIsPrivileged(t *testing.T) {
	// The transport tests cover refusing a foreign uid for every method
	// marked with WithPrivileged, and passing the peer PID to them; this
	// pins acquire to that list.
	if !slices.Contains(privilegedMethods(), "acquire") {
		t.Fatalf("privileged methods = %v, want acquire included", privilegedMethods())
	}
}

func TestLeaseRelease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	start := time.Unix(5, 0)
	key := domain.ProcessKey{PID: 100, Start: start}
	agent := domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"}
	caller := domain.ProcessSample{PID: 100, PPID: 10, Start: start, Name: "throt"}
	heavy := func(pid int) []process.WorkloadView {
		return []process.WorkloadView{{Key: domain.ProcessKey{PID: pid, Start: start}, Name: "go"}}
	}
	tests := []struct {
		name     string
		sampled  time.Time // when the sample started
		now      time.Time
		procs    []domain.ProcessSample
		heavy    []process.WorkloadView
		want     string // release reason, "" if kept
		workload int
	}{
		{name: "alive and not heavy", sampled: t0.Add(time.Second), now: t0.Add(time.Second), procs: []domain.ProcessSample{agent, caller}},
		{name: "exited", sampled: t0.Add(time.Second), now: t0.Add(time.Second), procs: []domain.ProcessSample{agent}, want: LeaseExited},
		{name: "pid reused", sampled: t0.Add(time.Second), now: t0.Add(time.Second),
			procs: []domain.ProcessSample{agent, {PID: 100, PPID: 10, Start: start.Add(time.Hour), Name: "sh"}}, want: LeaseExited},
		{name: "sample predates the grant", sampled: t0.Add(-time.Second), now: t0.Add(time.Second), procs: []domain.ProcessSample{agent}},
		{name: "became a workload itself", sampled: t0.Add(time.Second), now: t0.Add(time.Second),
			procs: []domain.ProcessSample{agent, {PID: 100, PPID: 10, Start: start, Name: "go"}}, heavy: heavy(100), want: LeaseBecameWorkload, workload: 100},
		{name: "descendant became a workload", sampled: t0.Add(time.Second), now: t0.Add(time.Second),
			procs: []domain.ProcessSample{agent, caller, {PID: 101, PPID: 100, Name: "sh"}, {PID: 102, PPID: 101, Start: start, Name: "go"}},
			heavy: heavy(102), want: LeaseBecameWorkload, workload: 102},
		{name: "unrelated workload", sampled: t0.Add(time.Second), now: t0.Add(time.Second),
			procs: []domain.ProcessSample{agent, caller, {PID: 200, PPID: 10, Start: start, Name: "go"}}, heavy: heavy(200)},
		{name: "expired", sampled: t0.Add(leaseTTL), now: t0.Add(leaseTTL), procs: []domain.ProcessSample{agent, caller}, want: LeaseExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lt leaseTable
			lt.grant(key, "claude-1", t0)
			ended := lt.observe(tt.sampled, tt.now, tt.procs, tt.heavy)
			if tt.want == "" {
				if len(ended) != 0 || len(lt.leases) != 1 {
					t.Fatalf("ended = %+v, leases = %+v; want the lease kept", ended, lt.leases)
				}
				return
			}
			if len(ended) != 1 || ended[0].reason != tt.want || len(lt.leases) != 0 {
				t.Fatalf("ended = %+v, leases = %+v; want released: %s", ended, lt.leases, tt.want)
			}
			if got := admitted(ended); tt.workload != 0 && (len(got) != 1 || got[0].PID != tt.workload) {
				t.Fatalf("admitted = %v, want pid %d", got, tt.workload)
			}
		})
	}

	t.Run("expires without a sample", func(t *testing.T) {
		var lt leaseTable
		lt.grant(key, "", t0)
		if ended := lt.expire(t0.Add(leaseTTL - time.Second)); len(ended) != 0 {
			t.Fatalf("expired early: %+v", ended)
		}
		if ended := lt.expire(t0.Add(leaseTTL)); len(ended) != 1 || ended[0].reason != LeaseExpired {
			t.Fatalf("ended = %+v", ended)
		}
	})

	t.Run("a zero start is no wildcard", func(t *testing.T) {
		var lt leaseTable
		lt.grant(key, "", t0)
		if _, ok := lt.find(domain.ProcessKey{PID: 100}); ok {
			t.Fatal("find matched a zero start")
		}
		if _, ok := lt.find(key); !ok {
			t.Fatal("find missed the exact key")
		}
	})
}

func TestDaemonLeaseHoldsSlot(t *testing.T) {
	// This test process plays `throt run`: the kernel reports its PID as
	// the caller, so the world gets a process with that PID under the agent.
	self := os.Getpid()
	w := newWorld(domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"})
	w.add(domain.ProcessSample{PID: self, PPID: 10, Name: "throt"})
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
	call := func(method string, params, out any) error {
		return transport.Call(context.Background(), cfg.SocketPath, method, params, out)
	}
	status := func() StatusResult {
		var st StatusResult
		call("status", nil, &st)
		return st
	}
	events := func(kind domain.EventKind) []domain.Event {
		var evs []domain.Event
		call("events", nil, &evs)
		return slices.DeleteFunc(evs, func(e domain.Event) bool { return e.Kind != kind })
	}
	waitFor(t, "normal pressure", func() bool { return status().Pressure == domain.PressureNormal })

	var r AcquireResult
	if err := call("acquire", AcquireParams{PID: self}, &r); err != nil || !r.Granted {
		t.Fatalf("acquire = %+v, %v", r, err)
	}
	if l := status().Scheduler.Leases; len(l) != 1 || l[0].Agent != "claude-1" || l[0].Process.PID != self {
		t.Fatalf("status leases = %+v", l)
	}
	// The kernel-verified caller cannot take a lease for another process.
	if err := call("acquire", AcquireParams{PID: 4242}, nil); err == nil || !strings.Contains(err.Error(), "is not pid 4242") {
		t.Fatalf("acquire for another pid: %v", err)
	}
	// The lease holds the only slot, so a build started meanwhile queues.
	w.add(domain.ProcessSample{PID: 11, PPID: 10, Name: "go"})
	waitFor(t, "pid 11 queued behind the lease", func() bool {
		q := status().Scheduler.Queued
		return len(q) == 1 && q[0].PID == 11
	})
	// The leased command starts its build: the build takes the slot the
	// lease held and runs, the lease ends, and 11 keeps waiting.
	w.add(domain.ProcessSample{PID: 20, PPID: self, Name: "go"})
	waitFor(t, "lease released", func() bool { return len(events(domain.EventLeaseReleased)) == 1 })
	if e := events(domain.EventLeaseReleased)[0]; e.Attrs["reason"] != LeaseBecameWorkload || e.Attrs["lease_id"] != r.LeaseID {
		t.Fatalf("lease_released = %+v", e)
	}
	st := status()
	if len(st.Scheduler.Leases) != 0 || len(st.Scheduler.Queued) != 1 || st.Scheduler.Queued[0].PID != 11 {
		t.Fatalf("scheduler status = %+v", st.Scheduler)
	}
	if strings.Contains(w.signals(), "stop:20") {
		t.Fatalf("the leased build was paused: %s", w.signals())
	}
	if n := len(events(domain.EventLeaseGranted)); n != 1 {
		t.Fatalf("%d lease_granted events, want 1", n)
	}
}
