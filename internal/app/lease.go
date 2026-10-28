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
}

// find returns the active lease for exactly key.
func (t *leaseTable) find(key domain.ProcessKey) (Lease, bool) {
	for _, l := range t.leases {
		if l.Process.PID == key.PID && l.Process.Start.Equal(key.Start) {
			return l, true
		}
	}
	return Lease{}, false
}

// holds reports whether pid holds an active lease.
func (t *leaseTable) holds(pid int) bool {
	return slices.ContainsFunc(t.leases, func(l Lease) bool { return l.Process.PID == pid })
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
// than leaseTTL expires.
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
		}
		return reason != ""
	})
	return ended
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
