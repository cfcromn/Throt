package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// Admission leases make `throt run` binding instead of advisory. A caller
// that acquires a lease holds one heavy slot until its command shows up as
// a heavy workload (which then counts by itself), its process exits, or
// leaseTTL passes. Leases live in memory only: a restarted daemon forgets
// them, which fails open.
const (
	// leaseTTL ends a lease whose command never becomes heavy.
	leaseTTL = 60 * time.Second
	// maxLeases bounds the lease table.
	maxLeases = 256
	// maxLeaseDepth bounds parent walks through the process table.
	maxLeaseDepth = 64
)

// Lease release reasons, in lease_released events.
const (
	LeaseExited         = "exited"
	LeaseBecameWorkload = "became_workload"
	LeaseExpired        = "expired"
)

// ReasonTooManyLeases is the acquire refusal when the lease table is full.
const ReasonTooManyLeases = "too_many_leases"

// Lease is one active admission lease, as shown in status.
type Lease struct {
	ID      string            `json:"id"`
	Process domain.ProcessKey `json:"process"`
	// Agent is the caller's agent instance label, or "" when unattributed.
	Agent   string    `json:"agent,omitempty"`
	Granted time.Time `json:"granted"`
}

// leaseEnd is a released lease and why it ended.
type leaseEnd struct {
	Lease
	reason string
	// workload is the heavy workload that took the lease over, for
	// LeaseBecameWorkload.
	workload domain.ProcessKey
}

// admitted returns the workloads that took over a lease, for
// scheduler.Input.Admitted.
func admitted(ended []leaseEnd) []domain.ProcessKey {
	var out []domain.ProcessKey
	for _, e := range ended {
		if e.reason == LeaseBecameWorkload {
			out = append(out, e.workload)
		}
	}
	return out
}

// leaseTable holds the active leases. It is not safe for concurrent use;
// the daemon guards it with d.mu together with the status it is checked
// against, so a grant and the snapshot it counts are one atomic step.
type leaseTable struct {
	leases []Lease
	lastID uint64
	// parents maps PID to parent PID in the latest process sample.
	parents map[int]int
	// held holds the agent leases (see grantAgent), by lease ID.
	held map[string]*agentLease
}

// agentLease is what an agent lease adds to a Lease. An agent lease is
// granted to an agent's reservation (the "reserve" method), not to one
// process: its Process is the agent's key, and the command it is for has
// not been started yet. It is ready until a hook call of that agent
// consumes it, and then ends when a heavy workload of the agent appears
// that was not running at that moment.
type agentLease struct {
	consumed bool
	// before holds the agent's heavy workloads when the lease was consumed.
	before []domain.ProcessKey
}

// find returns the active process lease for exactly key.
func (t *leaseTable) find(key domain.ProcessKey) (Lease, bool) {
	for _, l := range t.leases {
		if t.held[l.ID] == nil && l.Process.PID == key.PID && l.Process.Start.Equal(key.Start) {
			return l, true
		}
	}
	return Lease{}, false
}

// holds reports whether pid holds an active process lease. An agent lease
// does not count: it must not make every command of the agent nested.
func (t *leaseTable) holds(pid int) bool {
	return slices.ContainsFunc(t.leases, func(l Lease) bool { return t.held[l.ID] == nil && l.Process.PID == pid })
}

// grantAgent adds an agent lease for the agent with key, consumed at once
// when consumed is set, with before as the agent's heavy workloads.
func (t *leaseTable) grantAgent(key domain.ProcessKey, agent string, now time.Time, consumed bool, before []domain.ProcessKey) Lease {
	l := t.grant(key, agent, now)
	if t.held == nil {
		t.held = map[string]*agentLease{}
	}
	t.held[l.ID] = &agentLease{consumed: consumed, before: before}
	return l
}

// agentLease returns the agent lease of the agent with exactly key.
func (t *leaseTable) agentLease(key domain.ProcessKey) (Lease, *agentLease, bool) {
	for _, l := range t.leases {
		if a := t.held[l.ID]; a != nil && l.Process.PID == key.PID && l.Process.Start.Equal(key.Start) {
			return l, a, true
		}
	}
	return Lease{}, nil, false
}

// consume marks the agent lease id consumed at now by a command about to
// start, with before as the agent's heavy workloads, and restarts its TTL
// so the command has leaseTTL to appear.
func (t *leaseTable) consume(id string, now time.Time, before []domain.ProcessKey) {
	a := t.held[id]
	if a == nil || a.consumed {
		return
	}
	a.consumed, a.before = true, before
	for i := range t.leases {
		if t.leases[i].ID == id {
			t.leases[i].Granted = now
		}
	}
}

// grant adds a lease for key with the next ID.
func (t *leaseTable) grant(key domain.ProcessKey, agent string, now time.Time) Lease {
	t.lastID++
	l := Lease{ID: strconv.FormatUint(t.lastID, 10), Process: key, Agent: agent, Granted: now}
	t.leases = append(t.leases, l)
	return l
}

// views returns a copy of the active leases, never nil.
func (t *leaseTable) views() []Lease {
	return append([]Lease{}, t.leases...)
}

// owners returns the agent of each active lease, for scheduler.Input.
func (t *leaseTable) owners() []string {
	out := make([]string, len(t.leases))
	for i, l := range t.leases {
		out[i] = l.Agent
	}
	return out
}

// observe ends leases against a fresh process sample that started at
// sampled: a lease whose process, or a descendant of it, is a heavy
// workload ends (the workload counts from now on); one whose process is
// missing from a sample taken after the grant has exited; and one older
// than leaseTTL expires. A consumed agent lease ends instead when the
// agent owns a heavy workload it did not own when the lease was consumed.
func (t *leaseTable) observe(sampled, now time.Time, procs []domain.ProcessSample, heavy []process.WorkloadView) []leaseEnd {
	byPID := make(map[int]domain.ProcessSample, len(procs))
	t.parents = make(map[int]int, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
		t.parents[p.PID] = p.PPID
	}
	if len(t.leases) == 0 {
		return nil
	}
	// Heavy workload tops and their ancestors, each mapped to the heavy
	// workload below it with the lowest key: a lease whose process is among
	// them has a heavy descendant (or is heavy itself).
	heavy = slices.SortedFunc(slices.Values(heavy), func(a, b process.WorkloadView) int {
		return cmp.Or(cmp.Compare(a.Key.PID, b.Key.PID), a.Key.Start.Compare(b.Key.Start))
	})
	aboveHeavy := map[int]domain.ProcessKey{}
	for _, h := range heavy {
		for pid, depth := h.Key.PID, 0; pid > 0 && depth < maxLeaseDepth; depth++ {
			if _, done := aboveHeavy[pid]; done {
				break
			}
			aboveHeavy[pid] = h.Key
			pid = t.parents[pid]
		}
	}
	var ended []leaseEnd
	t.leases = slices.DeleteFunc(t.leases, func(l Lease) bool {
		p, seen := byPID[l.Process.PID]
		alive := seen && p.Start.Equal(l.Process.Start)
		workload, below := aboveHeavy[p.PID]
		if a := t.held[l.ID]; a != nil {
			workload, below = newWorkload(a, l.Process.PID, heavy)
		}
		reason := ""
		switch {
		case alive && below:
			reason = LeaseBecameWorkload
		case !alive && l.Granted.Before(sampled):
			// A sample taken before the grant may predate the process.
			reason = LeaseExited
		case now.Sub(l.Granted) >= leaseTTL:
			reason = LeaseExpired
		}
		if reason != "" {
			ended = append(ended, leaseEnd{l, reason, workload})
			delete(t.held, l.ID)
		}
		return reason != ""
	})
	return ended
}

// newWorkload returns the heavy workload, lowest key first, of the agent
// with PID agent that took over the consumed agent lease a.
func newWorkload(a *agentLease, agent int, heavy []process.WorkloadView) (domain.ProcessKey, bool) {
	if !a.consumed {
		return domain.ProcessKey{}, false
	}
	for _, h := range heavy {
		if h.OwnerPID == agent && !slices.Contains(a.before, h.Key) {
			return h.Key, true
		}
	}
	return domain.ProcessKey{}, false
}

// expire ends leases older than leaseTTL. It is used when no process sample
// is available.
func (t *leaseTable) expire(now time.Time) []leaseEnd {
	var ended []leaseEnd
	t.leases = slices.DeleteFunc(t.leases, func(l Lease) bool {
		if now.Sub(l.Granted) < leaseTTL {
			return false
		}
		ended = append(ended, leaseEnd{Lease: l, reason: LeaseExpired})
		delete(t.held, l.ID)
		return true
	})
	return ended
}

// nested reports whether an ancestor in chain is the top of a heavy
// workload or holds a lease: the caller's work is then part of work that
// already counts (a throt run line in a make recipe or npm script).
func (t *leaseTable) nested(chain []int, heavy []process.WorkloadView) bool {
	for _, pid := range chain {
		if t.holds(pid) || slices.ContainsFunc(heavy, func(h process.WorkloadView) bool { return h.Key.PID == pid }) {
			return true
		}
	}
	return false
}

// emitLeaseEnds emits a lease_released event per ended lease.
func (d *Daemon) emitLeaseEnds(ended []leaseEnd) {
	for _, e := range ended {
		d.emit(domain.EventLeaseReleased, fmt.Sprintf("lease %s of pid %d (%s) released: %s", e.ID, e.Process.PID, ownerLabel(e.Agent), e.reason),
			map[string]string{"lease_id": e.ID, "pid": strconv.Itoa(e.Process.PID), "agent": e.Agent, "reason": e.reason})
	}
}

func ownerLabel(agent string) string {
	if agent == "" {
		return "unattributed"
	}
	return agent
}
