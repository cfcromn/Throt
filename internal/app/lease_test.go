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

// leaseDaemon is a daemon that is never run: tests set its status and call
// handlers directly.
func leaseDaemon(t *testing.T, st StatusResult) *Daemon {
	t.Helper()
	d := NewDaemon(config.Default(), fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.status = st
	return d
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
	d := leaseDaemon(t, oneSlot(2, running))
	const callers = 8
	results := make([]AcquireResult, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			r, err := acquireAs(t, d, context.Background(), AcquireParams{PID: 100 + i, Start: time.Unix(1, 0)})
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
			if holder, _ = acquireAs(t, d, context.Background(), AcquireParams{PID: 100 + i, Start: time.Unix(1, 0)}); !holder.Granted || holder.LeaseID != r.LeaseID {
				t.Errorf("holder re-acquire = %+v, want lease %s", holder, r.LeaseID)
			}
		}
	}
}

func TestAcquireCountsLeasesPerAgent(t *testing.T) {
	st := oneSlot(4)
	st.Scheduler.PerAgentLimit = 1
	d := leaseDaemon(t, st)
	// Both callers' parent is the agent (pid 10); the agent's budget is one.
	d.leases.parents = map[int]int{100: 10, 101: 10, 102: 1}
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 100}); !r.Granted {
		t.Fatalf("first acquire refused: %s", r.Reason)
	}
	r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 101})
	if r.Granted || !strings.Contains(r.Reason, "claude-1 already runs 1 heavy jobs, 1 of them reserved") {
		t.Fatalf("second acquire for the same agent = %+v", r)
	}
	// An unattributed caller is not held to the agent's budget; one not in
	// the sample yet is attributed through its ppid.
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 102}); !r.Granted {
		t.Fatalf("unattributed acquire refused: %s", r.Reason)
	}
	if r, _ := acquireAs(t, d, context.Background(), AcquireParams{PID: 103, PPID: 10}); r.Granted {
		t.Fatalf("acquire attributed by ppid granted: %+v", r)
	}
	if got := d.leases.owners(); !slices.Equal(got, []string{"claude-1", ""}) {
		t.Fatalf("lease owners = %q", got)
	}
}

func TestAcquireBounded(t *testing.T) {
	st := oneSlot(1)
	st.Scheduler.Mode = scheduler.ModeOff // no slot limit, so only the bound refuses
	d := leaseDaemon(t, st)
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

func TestAcquireChecksCaller(t *testing.T) {
	d := leaseDaemon(t, oneSlot(2))
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

	t.Run("zero start is fixed by the first later sample", func(t *testing.T) {
		var lt leaseTable
		lt.grant(domain.ProcessKey{PID: 100}, "", t0)
		lt.observe(t0.Add(time.Second), t0.Add(time.Second), []domain.ProcessSample{caller}, nil)
		if len(lt.leases) != 1 || !lt.leases[0].Process.Start.Equal(start) {
			t.Fatalf("leases = %+v, want start pinned to %v", lt.leases, start)
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
	if err := call("acquire", AcquireParams{PID: self, Start: time.Unix(int64(self), 0)}, &r); err != nil || !r.Granted {
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
