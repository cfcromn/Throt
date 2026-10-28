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

	"github.com/cfcromn/Throt/internal/control"
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

// AcquireParams name the caller of the "acquire" method. throt run execs
// its command in place, so the caller's process key stays valid for it.
//
// The daemon trusts nothing else the client says about itself: it resolves
// the caller's start time and parent from the OS (ProcessIdentifier), so a
// wall-clock step cannot make the client's start time disagree with the
// daemon's samples, and a client cannot borrow another agent's budget.
// Older clients also sent "start" and "ppid"; both are ignored.
type AcquireParams struct {
	// PID is the caller's own PID. Where the kernel reports the peer PID it
	// must match; elsewhere (a peer in another PID namespace) it is the
	// process whose identity the daemon resolves.
	PID int `json:"pid"`
}

// ProcessIdentifier looks up one live process with exactly the start time,
// uid and parent the daemon's process sampler reports. The platform process
// controllers implement it.
type ProcessIdentifier interface {
	Identify(ctx context.Context, pid int) (domain.ProcessSample, error)
}

// ReasonNested grants a caller below a running heavy workload or a lease
// holder without reserving a slot: its work is part of one already counted.
const ReasonNested = "nested"

// maxIdentifyLookups bounds the OS lookups one acquire makes for ancestors
// missing from the latest process sample (e.g. the shell a make recipe
// just started).
const maxIdentifyLookups = 8

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

// ancestry returns the caller's ancestors, nearest first, starting at its
// real parent ppid as the OS reported it. Further parents come from the
// latest process sample; ancestors it does not show yet (such as the shell
// a make recipe just started) are asked of the OS, at most
// maxIdentifyLookups times.
func (d *Daemon) ancestry(ctx context.Context, ppid int) []int {
	var chain []int
	lookups := 0
	for next := ppid; next > 1 && len(chain) < maxLeaseDepth && !slices.Contains(chain, next); {
		chain = append(chain, next)
		d.mu.Lock()
		pp, ok := d.leases.parents[next]
		d.mu.Unlock()
		if !ok {
			if lookups == maxIdentifyLookups {
				break
			}
			lookups++
			s, err := d.identify(ctx, next)
			if err != nil {
				break
			}
			pp = s.PPID
		}
		next = pp
	}
	return chain
}

// identify looks up one process with its own deadline. The request context
// has none, so without it a hung ps would hold the acquire, and one of the
// server's few connection slots, forever.
func (d *Daemon) identify(ctx context.Context, pid int) (domain.ProcessSample, error) {
	ctx, cancel := context.WithTimeout(ctx, control.CallTimeout)
	defer cancel()
	return d.ident.Identify(ctx, pid)
}

// nearestAgent returns the label of the first agent in chain.
func nearestAgent(chain []int, agents []process.AgentView) string {
	for _, pid := range chain {
		for _, a := range agents {
			if a.Key.PID == pid {
				return a.Agent
			}
		}
	}
	return ""
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

// acquire grants an admission lease to the calling process when Assess,
// counting active leases as running heavy work, says a heavy job may start.
// The check and the grant happen under one lock, so of two concurrent
// callers competing for one slot exactly one is granted. A caller nested
// below a heavy workload or a lease holder is granted without a lease.
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
	if d.ident == nil {
		return nil, d.refused("acquire", p.PID, "cannot identify processes on this platform")
	}
	self, err := d.identify(ctx, p.PID)
	switch {
	case err != nil:
		return nil, d.refused("acquire", p.PID, "cannot identify the caller: "+err.Error())
	case self.Start.IsZero():
		return nil, d.refused("acquire", p.PID, "the caller has no start time")
	case self.UID != d.uid:
		return nil, d.refused("acquire", p.PID, "the process belongs to another user")
	}
	key := self.Key()
	chain := d.ancestry(ctx, self.PPID)
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
	agent := nearestAgent(chain, st.Agents)
	v := Assess(st, agent)
	res := AcquireResult{Reason: v.Reason, Verdict: v}
	var l Lease
	switch {
	case d.leases.nested(chain, st.Heavy):
		res.Granted, res.Reason = true, ReasonNested
	case len(d.leases.leases) >= maxLeases:
		res.Reason = ReasonTooManyLeases
	case v.OKToStart:
		l = d.leases.grant(key, agent, now)
		res.Granted, res.LeaseID = true, l.ID
	}
	d.mu.Unlock()

	if res.LeaseID != "" {
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
