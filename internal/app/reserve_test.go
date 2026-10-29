package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// errNoSample fails a process sample.
var errNoSample = errors.New("no process sample")

// agentKey is the key of the agent oneSlot lists.
var agentKey = domain.ProcessKey{PID: 10}

func reserveAs(t *testing.T, d *Daemon, p ReserveParams) ReserveResult {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.reserve(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return v.(ReserveResult)
}

func TestReserveGrantLeaseConsumed(t *testing.T) {
	running := process.WorkloadView{Key: domain.ProcessKey{PID: 11}, Name: "go", Owner: "claude-1", OwnerPID: 10}
	d, ident := leaseDaemon(t, oneSlot(1, running))
	// Two hook calls and a throt wait, each a new process under the agent.
	ident.procs[100], ident.procs[101], ident.procs[102] = child(10), child(10), child(10)

	// No slot: the hook's call holds the agent's place, first in line.
	r := reserveAs(t, d, ReserveParams{PID: 100})
	if r.Granted || r.Position != 1 || r.LeaseID != "" || !strings.Contains(r.Reason, "limit 1") {
		t.Fatalf("reserve without a slot = %+v", r)
	}
	if len(d.reserved.r) != 1 || d.reserved.r[0].agent != agentKey || d.reserved.r[0].owner != "claude-1" {
		t.Fatalf("reservations = %+v", d.reserved.r)
	}
	since := d.reserved.r[0].since
	// throt wait renews the same reservation.
	if r := reserveAs(t, d, ReserveParams{PID: 102, Wait: true}); r.Granted || r.Position != 1 {
		t.Fatalf("wait poll = %+v", r)
	}
	if len(d.reserved.r) != 1 || !d.reserved.r[0].since.Equal(since) {
		t.Fatalf("reservations after a renewal = %+v", d.reserved.r)
	}

	// The slot frees; the tick grants the reservation an agent lease.
	d.status.Heavy = nil
	granted := d.grantReservations(nil, time.Now())
	if len(granted) != 1 || granted[0].Process != agentKey || granted[0].Agent != "claude-1" || len(d.reserved.r) != 0 {
		t.Fatalf("granted %+v, reservations %+v", granted, d.reserved.r)
	}
	id := granted[0].ID
	// The lease is not the agent's process lease: a throt run under the
	// agent is neither nested nor its holder, and waits for the slot.
	if r, err := acquireAs(t, d, context.Background(), AcquireParams{PID: 101}); err != nil || r.Granted {
		t.Fatalf("acquire beside an agent lease = %+v, %v", r, err)
	}
	d.waiting.w = nil

	// throt wait reports the slot without consuming it.
	if r := reserveAs(t, d, ReserveParams{PID: 102, Wait: true}); !r.Granted || r.LeaseID != id {
		t.Fatalf("wait after the grant = %+v", r)
	}
	if _, a, _ := d.leases.agentLease(agentKey); a.consumed {
		t.Fatal("a wait poll consumed the lease")
	}
	// The rerun's hook call is admitted at once and consumes it.
	if r := reserveAs(t, d, ReserveParams{PID: 100}); !r.Granted || r.LeaseID != id {
		t.Fatalf("rerun = %+v", r)
	}
	l, a, ok := d.leases.agentLease(agentKey)
	if !ok || !a.consumed {
		t.Fatalf("after the rerun: lease %+v, %+v", l, a)
	}

	// The lease still counts as a reserved slot, for the agent too.
	st, _ := d.currentStatus(context.Background(), nil)
	if v := Assess(st.(StatusResult), "codex-1"); v.OKToStart || v.Reserved != 1 {
		t.Fatalf("verdict with the agent lease = %+v", v)
	}

	// The command shows up as the agent's new heavy workload and takes the
	// slot over.
	procs := []domain.ProcessSample{{PID: 10, PPID: 1}, {PID: 300, PPID: 10, Start: time.Unix(300, 0)}}
	heavy := []process.WorkloadView{{Key: domain.ProcessKey{PID: 300, Start: time.Unix(300, 0)}, Owner: "claude-1", OwnerPID: 10}}
	d.mu.Lock()
	ended := d.leases.observe(time.Now(), time.Now(), procs, heavy)
	d.mu.Unlock()
	if len(ended) != 1 || ended[0].reason != LeaseBecameWorkload || ended[0].workload.PID != 300 || len(d.leases.held) != 0 {
		t.Fatalf("ended = %+v, held = %+v", ended, d.leases.held)
	}
}

func TestAgentLeaseRelease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	agent := domain.ProcessSample{PID: 10, PPID: 1, Start: time.Unix(5, 0)}
	key := agent.Key()
	old := process.WorkloadView{Key: domain.ProcessKey{PID: 20, Start: time.Unix(6, 0)}, OwnerPID: 10}
	fresh := process.WorkloadView{Key: domain.ProcessKey{PID: 30, Start: time.Unix(7, 0)}, OwnerPID: 10}
	other := process.WorkloadView{Key: domain.ProcessKey{PID: 40, Start: time.Unix(7, 0)}, OwnerPID: 99}
	tests := []struct {
		name     string
		consumed bool
		now      time.Time
		procs    []domain.ProcessSample
		heavy    []process.WorkloadView
		want     string
	}{
		{name: "ready, agent busy with older work", now: t0.Add(time.Second), procs: []domain.ProcessSample{agent}, heavy: []process.WorkloadView{old, fresh}},
		{name: "consumed, only older work", consumed: true, now: t0.Add(time.Second), procs: []domain.ProcessSample{agent}, heavy: []process.WorkloadView{old}},
		{name: "consumed, another agent's work", consumed: true, now: t0.Add(time.Second), procs: []domain.ProcessSample{agent}, heavy: []process.WorkloadView{other}},
		{name: "consumed, new work", consumed: true, now: t0.Add(time.Second), procs: []domain.ProcessSample{agent}, heavy: []process.WorkloadView{old, fresh}, want: LeaseBecameWorkload},
		{name: "agent exited", now: t0.Add(time.Second), want: LeaseExited},
		{name: "unused grant expires", now: t0.Add(leaseTTL), procs: []domain.ProcessSample{agent}, want: LeaseExpired},
		{name: "consumed, never heavy", consumed: true, now: t0.Add(leaseTTL), procs: []domain.ProcessSample{agent}, want: LeaseExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lt leaseTable
			lt.grantAgent(key, "claude-1", t0, tt.consumed, []domain.ProcessKey{old.Key})
			ended := lt.observe(t0.Add(time.Millisecond), tt.now, tt.procs, tt.heavy)
			if tt.want == "" {
				if len(ended) != 0 || len(lt.leases) != 1 {
					t.Fatalf("ended = %+v; want the lease kept", ended)
				}
				return
			}
			if len(ended) != 1 || ended[0].reason != tt.want || len(lt.leases) != 0 || len(lt.held) != 0 {
				t.Fatalf("ended = %+v, held = %+v; want released: %s", ended, lt.held, tt.want)
			}
			if tt.want == LeaseBecameWorkload && ended[0].workload != fresh.Key {
				t.Fatalf("taken over by %v, want %v", ended[0].workload, fresh.Key)
			}
		})
	}

	t.Run("consuming restarts the TTL once", func(t *testing.T) {
		var lt leaseTable
		l := lt.grantAgent(key, "", t0, false, nil)
		lt.consume(l.ID, t0.Add(50*time.Second), nil)
		lt.consume(l.ID, t0.Add(100*time.Second), nil)
		if ended := lt.expire(t0.Add(50*time.Second + leaseTTL - time.Second)); len(ended) != 0 {
			t.Fatalf("expired early: %+v", ended)
		}
		if ended := lt.expire(t0.Add(50*time.Second + leaseTTL)); len(ended) != 1 || len(lt.held) != 0 {
			t.Fatalf("ended = %+v, held = %+v", ended, lt.held)
		}
	})
}

func TestReservationExpiry(t *testing.T) {
	t0 := time.Unix(1000, 0)
	k := func(pid int) domain.ProcessKey { return domain.ProcessKey{PID: pid, Start: time.Unix(int64(pid), 0)} }
	var l reservationList
	for i := range maxReservations {
		if !l.touch(k(100+i), "", t0) {
			t.Fatalf("reservation %d refused", i)
		}
	}
	if l.touch(k(9999), "", t0) || len(l.r) != maxReservations {
		t.Fatalf("over the bound: %d reservations", len(l.r))
	}
	// A renewal keeps the first time; nobody else renews.
	l.touch(k(100), "a", t0.Add(time.Minute))
	l.prune(t0.Add(reservationTTL-time.Second), t0, nil)
	if len(l.r) != maxReservations {
		t.Fatalf("pruned early: %d left", len(l.r))
	}
	l.prune(t0.Add(reservationTTL), t0, nil)
	if len(l.r) != 1 || l.r[0].agent != k(100) || !l.r[0].since.Equal(t0) || l.r[0].owner != "a" {
		t.Fatalf("after the TTL: %+v", l.r)
	}
	// An agent missing from a sample taken after it reserved is gone.
	l.prune(t0.Add(2*time.Minute), t0.Add(90*time.Second), []domain.ProcessSample{{PID: 100, Start: time.Unix(100, 0)}})
	if len(l.r) != 1 {
		t.Fatalf("live agent dropped: %+v", l.r)
	}
	l.prune(t0.Add(2*time.Minute), t0.Add(90*time.Second), []domain.ProcessSample{{PID: 1}})
	if len(l.r) != 0 {
		t.Fatalf("gone agent kept: %+v", l.r)
	}
}

func TestReservationExpiresInTick(t *testing.T) {
	// An abandoned reservation is dropped by the tick's publish, so it no
	// longer holds a place in line or reaches the scheduler.
	d, _ := leaseDaemon(t, oneSlot(1))
	t0 := time.Now()
	d.reserved.touch(agentKey, "claude-1", t0.Add(-reservationTTL))
	_, _, _, waiters := d.publish(sample{now: t0, sampled: t0, perr: errNoSample}, processRound{}, domain.HostSample{}, domain.PressureNormal, "")
	if len(d.reserved.r) != 0 || len(waiters) != 0 {
		t.Fatalf("reservations %+v, waiters %+v", d.reserved.r, waiters)
	}
	d.reserved.touch(agentKey, "claude-1", t0)
	_, _, _, waiters = d.publish(sample{now: t0, sampled: t0, perr: errNoSample}, processRound{}, domain.HostSample{}, domain.PressureNormal, "")
	if len(waiters) != 1 || waiters[0].Key != agentKey || !waiters[0].Lease {
		t.Fatalf("waiters = %+v, want the live reservation", waiters)
	}
}

func TestReserveFIFOWithQueuedWork(t *testing.T) {
	// One of two slots is free, and workload 21 has been queued since
	// t0-10s.
	t0 := time.Unix(1000, 0)
	running := process.WorkloadView{Key: domain.ProcessKey{PID: 11}, Name: "go", Owner: "claude-1", OwnerPID: 10}
	queued := process.WorkloadView{Key: domain.ProcessKey{PID: 21}, Name: "go", Owner: "codex-1", OwnerPID: 20}
	st := oneSlot(2, running, queued)
	st.Agents = append(st.Agents, process.AgentView{Key: domain.ProcessKey{PID: 20}, Agent: "codex-1"})
	st.Scheduler.Queued = []domain.ProcessKey{queued.Key}
	d, _ := leaseDaemon(t, st)
	d.queuedLine = []scheduler.Waiter{{Key: queued.Key, Owner: "codex-1", Since: t0.Add(-10 * time.Second)}}

	// The reservation is behind the queued workload.
	r, _ := d.reserveFor([]int{10}, nil, t0, false)
	if r.Granted || r.Position != 2 || !strings.Contains(r.Reason, "1 ahead") {
		t.Fatalf("reserve behind queued work = %+v", r)
	}
	if g := d.grantReservations(nil, t0.Add(time.Second)); len(g) != 0 {
		t.Fatalf("granted ahead of queued work: %+v", g)
	}
	// A throt run caller arriving later waits behind the reservation.
	d.queuedLine = nil
	d.status.Heavy, d.status.Scheduler.Queued = []process.WorkloadView{running}, []domain.ProcessKey{}
	d.waiting.touch(domain.ProcessKey{PID: 500}, "codex-1", t0.Add(2*time.Second))
	d.mu.Lock()
	ahead := d.aheadInLine(scheduler.Waiter{Key: domain.ProcessKey{PID: 500}, Owner: "codex-1", Since: t0.Add(2 * time.Second), Lease: true}, d.statusWithLeases(), t0.Add(3*time.Second))
	d.mu.Unlock()
	if ahead != 1 {
		t.Fatalf("%d ahead of the later throt run caller, want the reservation", ahead)
	}
	// The queued workload resumed: the reservation is first and gets the
	// free slot; the later reservation of another agent does not.
	d.reserveFor([]int{20}, nil, t0.Add(4*time.Second), false)
	g := d.grantReservations(nil, t0.Add(5*time.Second))
	if len(g) != 1 || g[0].Process != agentKey {
		t.Fatalf("granted %+v, want only the oldest reservation", g)
	}
	if len(d.reserved.r) != 1 || d.reserved.r[0].owner != "codex-1" {
		t.Fatalf("reservations = %+v", d.reserved.r)
	}
}

func TestReserveImmediateAndNoAgent(t *testing.T) {
	d, _ := leaseDaemon(t, oneSlot(2))
	now := time.Unix(1000, 0)
	// A free slot is granted at once and consumed by the call.
	r, l := d.reserveFor([]int{10}, nil, now, false)
	if !r.Granted || l.ID == "" || len(d.reserved.r) != 0 {
		t.Fatalf("immediate reserve = %+v, lease %+v", r, l)
	}
	if _, a, ok := d.leases.agentLease(agentKey); !ok || !a.consumed {
		t.Fatal("the immediate grant is not consumed")
	}
	// The agent's next heavy command reuses its slot instead of waiting
	// for itself.
	if r, l := d.reserveFor([]int{10}, nil, now.Add(time.Second), false); !r.Granted || l.ID != "" {
		t.Fatalf("second call = %+v, %+v", r, l)
	}
	// Without an agent there is no place to hold.
	if r, l := d.reserveFor([]int{77}, nil, now, false); r.Granted || r.Position != 0 || r.Reason != ReasonNoAgent || l.ID != "" {
		t.Fatalf("no agent = %+v", r)
	}
	if len(d.reserved.r) != 0 {
		t.Fatalf("reservations = %+v", d.reserved.r)
	}
}

func TestReserveIsPrivileged(t *testing.T) {
	if p := privilegedMethods(); !strings.Contains(strings.Join(p, " "), "reserve") {
		t.Fatalf("privileged methods = %v", p)
	}
}
