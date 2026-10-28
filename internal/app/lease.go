package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/transport"
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

// AcquireParams identify the caller of the "acquire" method by its own
// process key. throt run execs its command in place, so the key stays valid.
type AcquireParams struct {
	PID int `json:"pid"`
	// Start is the caller's start time as the platform's process sampler
	// reports it. When zero, the first process sample after the grant
	// fixes it.
	Start time.Time `json:"start,omitzero"`
	// PPID, when set, attributes the lease to the caller's agent while the
	// caller is too new to be in the daemon's process sample.
	PPID int `json:"ppid,omitempty"`
}

// AcquireResult is returned by the "acquire" method.
type AcquireResult struct {
	Granted bool   `json:"granted"`
	Reason  string `json:"reason"`
	LeaseID string `json:"lease_id,omitempty"`
	// Verdict is the capacity verdict the decision was based on.
	Verdict Verdict `json:"verdict"`
}

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

// sameProcess reports whether a lease key names process p. A zero start
// matches any start.
func sameProcess(k domain.ProcessKey, p domain.ProcessSample) bool {
	return k.PID == p.PID && (k.Start.IsZero() || k.Start.Equal(p.Start))
}

// find returns the active lease for key, matching a zero start to any.
func (t *leaseTable) find(key domain.ProcessKey) (Lease, bool) {
	for _, l := range t.leases {
		if l.Process.PID == key.PID && (l.Process.Start.IsZero() || key.Start.IsZero() || l.Process.Start.Equal(key.Start)) {
			return l, true
		}
	}
	return Lease{}, false
}

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
		alive := seen && sameProcess(l.Process, p)
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
	for i, l := range t.leases {
		if p, ok := byPID[l.Process.PID]; ok && l.Process.Start.IsZero() && l.Granted.Before(sampled) {
			t.leases[i].Process.Start = p.Start
		}
	}
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

// agentFor returns the label of the nearest agent above pid, starting at
// ppid when pid is not in the latest process sample yet.
func (t *leaseTable) agentFor(pid, ppid int, agents []process.AgentView) string {
	labels := make(map[int]string, len(agents))
	for _, a := range agents {
		labels[a.Key.PID] = a.Agent
	}
	next, ok := t.parents[pid]
	if !ok {
		next = ppid
	}
	for depth := 0; next > 1 && depth < maxLeaseDepth; depth++ {
		if label, ok := labels[next]; ok {
			return label
		}
		next = t.parents[next]
	}
	return ""
}

// acquire grants an admission lease to the calling process when Assess,
// counting active leases as running heavy work, says a heavy job may start.
// The check and the grant happen under one lock, so of two concurrent
// callers competing for one slot exactly one is granted.
func (d *Daemon) acquire(ctx context.Context, raw json.RawMessage) (any, error) {
	var p AcquireParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if p.PID <= 1 {
		return nil, errors.New("invalid params: pid must be > 1")
	}
	// A lease is the caller's own: where the kernel reports the peer PID,
	// it must be the process the lease is for.
	if peer, ok := transport.PeerPIDFrom(ctx); ok && peer != p.PID {
		return nil, d.refused("acquire", p.PID, fmt.Sprintf("caller pid %d is not pid %d", peer, p.PID))
	}
	key := domain.ProcessKey{PID: p.PID, Start: p.Start}
	paused := d.ctl.Paused()
	now := time.Now()

	d.mu.Lock()
	if l, ok := d.leases.find(key); ok {
		d.mu.Unlock()
		return AcquireResult{Granted: true, Reason: "already holds a lease", LeaseID: l.ID}, nil
	}
	st := d.status
	st.Paused = paused
	st.Scheduler.Leases = d.leases.views()
	agent := d.leases.agentFor(p.PID, p.PPID, st.Agents)
	v := Assess(st, agent)
	res := AcquireResult{Reason: v.Reason, Verdict: v}
	var l Lease
	switch {
	case len(d.leases.leases) >= maxLeases:
		res.Reason = ReasonTooManyLeases
	case v.OKToStart:
		l = d.leases.grant(key, agent, now)
		res.Granted, res.LeaseID = true, l.ID
	}
	d.mu.Unlock()

	if res.Granted {
		d.emit(domain.EventLeaseGranted, fmt.Sprintf("lease %s granted to pid %d (%s): %s", l.ID, p.PID, ownerLabel(agent), v.Reason),
			map[string]string{"lease_id": l.ID, "pid": strconv.Itoa(p.PID), "agent": agent})
	}
	return res, nil
}

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
