package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
	"github.com/cfcromn/Throt/internal/transport"
)

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
	// Overdue asks for a lease even past the limit: the caller waited its
	// whole timeout and runs anyway, and must not be queued after it starts.
	Overdue bool `json:"overdue,omitempty"`
}

// AcquireResult is returned by the "acquire" method.
type AcquireResult struct {
	Granted bool   `json:"granted"`
	Reason  string `json:"reason"`
	LeaseID string `json:"lease_id,omitempty"`
	// Verdict is the capacity verdict the decision was based on.
	Verdict Verdict `json:"verdict"`
}

// ReasonNested grants a caller below a running heavy workload or a lease
// holder without reserving a slot: its work is part of one already counted.
const ReasonNested = "nested"

// ReasonOverdue grants a lease past the limit to a caller that waited its
// whole timeout and runs anyway.
const ReasonOverdue = "overdue"

// ProcessIdentifier looks up one live process with exactly the start time,
// uid and parent the daemon's process sampler reports. The platform process
// controllers implement it.
type ProcessIdentifier interface {
	Identify(ctx context.Context, pid int) (domain.ProcessSample, error)
}

// maxIdentifyLookups bounds the OS lookups one acquire makes for ancestors
// missing from the latest process sample (e.g. the shell a make recipe
// just started).
const maxIdentifyLookups = 8

// acquire grants an admission lease to the calling process when Assess,
// counting active leases as running heavy work, says a heavy job may start.
// The check and the grant happen under one lock, so of two concurrent
// callers competing for one slot exactly one is granted. A caller nested
// below a heavy workload or a lease holder is granted without a lease.
func (d *Daemon) acquire(ctx context.Context, raw json.RawMessage) (any, error) {
	var p AcquireParams
	if err := decodeParams(raw, &p); err != nil {
		return nil, err
	}
	if err := checkPID(p.PID); err != nil {
		return nil, err
	}
	self, err := d.acquireCaller(ctx, p.PID)
	if err != nil {
		return nil, err
	}
	chain := d.ancestry(ctx, self.PPID)
	paused := d.ctl.Paused()
	now := time.Now()
	res, l := d.admit(self.Key(), chain, paused, now, p.Overdue)
	if l.ID != "" {
		d.emit(domain.EventLeaseGranted, fmt.Sprintf("lease %s granted to pid %d (%s): %s", l.ID, p.PID, ownerLabel(l.Agent), res.Verdict.Reason),
			map[string]string{"lease_id": l.ID, "pid": strconv.Itoa(p.PID), "agent": l.Agent})
	}
	return res, nil
}

// acquireCaller resolves pid, the acquire caller, from the OS. It refuses a
// caller that is not the peer, cannot be identified, or belongs to another
// user.
func (d *Daemon) acquireCaller(ctx context.Context, pid int) (domain.ProcessSample, error) {
	self, code, why := d.caller(ctx, pid)
	if code != "" {
		return self, d.refused("acquire", pid, code, why)
	}
	return self, nil
}

// caller resolves pid, a caller asking about itself, from the OS. When it
// refuses the caller it returns a refusal reason and a sentence.
func (d *Daemon) caller(ctx context.Context, pid int) (self domain.ProcessSample, code, why string) {
	// A lease is the caller's own: where the kernel reports the peer PID,
	// it must be the process the lease is for.
	if peer, ok := transport.PeerPIDFrom(ctx); ok && peer != pid {
		return self, ReasonPeerMismatch, fmt.Sprintf("caller pid %d is not pid %d", peer, pid)
	}
	if d.ident == nil {
		return self, ReasonUnidentified, "cannot identify processes on this platform"
	}
	self, err := d.identify(ctx, pid)
	switch {
	case err != nil:
		return self, ReasonUnidentified, "cannot identify the caller: " + err.Error()
	case self.Start.IsZero():
		return self, ReasonUnidentified, "the caller has no start time"
	case self.UID != d.uid:
		return self, control.ReasonForeignUID, "the process belongs to another user"
	}
	return self, "", ""
}

// admit decides an acquire by key, whose ancestors are chain, and grants
// the lease it returns. The check and the grant happen under d.mu. A zero
// Lease means none was granted now, also when key already holds one.
func (d *Daemon) admit(key domain.ProcessKey, chain []int, paused []control.PausedWorkload, now time.Time, overdue bool) (AcquireResult, Lease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l, ok := d.leases.find(key); ok {
		return AcquireResult{Granted: true, Reason: "already holds a lease", LeaseID: l.ID}, Lease{}
	}
	st := d.statusWithLeases()
	st.Paused = paused
	agent := nearestAgent(chain, st.Agents)
	v := assess(st, agent, true)
	res := AcquireResult{Reason: v.Reason, Verdict: v}
	var l Lease
	switch {
	case d.leases.nested(chain, st.Heavy):
		res.Granted, res.Reason = true, ReasonNested
	case len(d.leases.leases) >= maxLeases:
		res.Reason = ReasonTooManyLeases
	case overdue:
		l = d.leases.grant(key, agent, now)
		res.Granted, res.LeaseID, res.Reason = true, l.ID, ReasonOverdue
	case !v.OKToStart:
	case st.Scheduler.Mode != scheduler.ModeOff && !d.firstInLine(key, agent, st, now):
		// A slot is free, but older work waits for it.
		res.Reason = fmt.Sprintf("older heavy work is waiting for a slot (%d queued, %d other throt run callers waiting)",
			len(st.Scheduler.Queued), max(len(d.waiting.w)-1, 0))
		res.Verdict.OKToStart, res.Verdict.Reason = false, res.Reason
	default:
		l = d.leases.grant(key, agent, now)
		res.Granted, res.LeaseID = true, l.ID
	}
	if res.Granted {
		d.waiting.remove(key)
	} else {
		d.waiting.touch(key, agent, now)
	}
	return res, l
}

// firstInLine reports whether caller key, a lease waiter of agent, comes
// first in the line shared with scheduler-queued workloads. d.mu is held.
func (d *Daemon) firstInLine(key domain.ProcessKey, agent string, st StatusResult, now time.Time) bool {
	me := scheduler.Waiter{Key: key, Owner: agent, Since: d.waiting.since(key, now), Lease: true}
	line := append(slices.Clone(d.queuedLine), d.waiting.line()...)
	// The budget is the one Assess just checked, from the same status.
	p := d.linePolicy
	p.PerOwnerLimit = st.Scheduler.PerAgentLimit
	return p.FirstInLine(now, me, line, busyByOwner(st))
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
	if a, ok := nearestAgentView(chain, agents); ok {
		return a.Agent
	}
	return ""
}

// nearestAgentView returns the first agent in chain.
func nearestAgentView(chain []int, agents []process.AgentView) (process.AgentView, bool) {
	for _, pid := range chain {
		for _, a := range agents {
			if a.Key.PID == pid {
				return a, true
			}
		}
	}
	return process.AgentView{}, false
}
