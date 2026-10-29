package app

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// Reservations hold an agent's place in line without blocking it: the
// Claude Code hook with --defer reserves instead of waiting and tells the
// agent to run `throt wait`, which polls the same reservation. They are
// keyed by the agent, not by a process, since each hook call is a new
// process. A reservation waits in the line shared with lease waiters and
// queued workloads; when it is first and a slot is free it becomes an
// agent lease (leaseTable.grantAgent), which the agent's next hook call
// consumes, and which expires after leaseTTL like any lease if it does not.
const (
	// reservationTTL drops a reservation that no deferred hook call or
	// `throt wait` poll renewed for this long.
	reservationTTL = 10 * time.Minute
	// maxReservations bounds the reservation table; agents beyond it are
	// answered but hold no place in line.
	maxReservations = 64
)

// ReasonNoAgent is the reserve result's reason when the caller runs under
// no agent throtd knows: there is nobody to hold a place for.
const ReasonNoAgent = "no_agent"

// ReserveParams name the caller of the "reserve" method.
type ReserveParams struct {
	// PID is the caller's own PID, checked and resolved as for acquire.
	// The reservation is for the caller's nearest agent.
	PID int `json:"pid"`
	// Wait polls for `throt wait`: a granted slot is reported but kept for
	// the agent's next hook call instead of being consumed.
	Wait bool `json:"wait,omitempty"`
}

// ReserveResult is returned by the "reserve" method.
type ReserveResult struct {
	// Granted means the agent holds a slot: without Wait the command about
	// to start may run now and the slot is its own.
	Granted bool   `json:"granted"`
	Reason  string `json:"reason"`
	LeaseID string `json:"lease_id,omitempty"`
	// Position is the agent's place in line while not granted, 1 first; 0
	// when it holds none (no agent, or the table is full).
	Position int     `json:"position,omitempty"`
	Verdict  Verdict `json:"verdict"`
}

// reservation is one agent waiting for a heavy slot.
type reservation struct {
	agent domain.ProcessKey
	owner string
	since time.Time // first reserved
	seen  time.Time // latest renewal
}

// reservationList is the reservation table, guarded by d.mu.
type reservationList struct {
	r []reservation
}

// touch renews agent's reservation at now, creating it if there is room,
// and reports whether it holds one.
func (l *reservationList) touch(agent domain.ProcessKey, owner string, now time.Time) bool {
	for i := range l.r {
		if l.r[i].agent == agent {
			l.r[i].seen, l.r[i].owner = now, owner
			return true
		}
	}
	if len(l.r) >= maxReservations {
		return false
	}
	l.r = append(l.r, reservation{agent: agent, owner: owner, since: now, seen: now})
	return true
}

// since returns when agent first reserved, or now if it holds none.
func (l *reservationList) since(agent domain.ProcessKey, now time.Time) time.Time {
	for _, r := range l.r {
		if r.agent == agent {
			return r.since
		}
	}
	return now
}

func (l *reservationList) remove(agent domain.ProcessKey) {
	l.r = slices.DeleteFunc(l.r, func(r reservation) bool { return r.agent == agent })
}

// prune drops reservations not renewed for reservationTTL and, given a
// process sample that started at sampled, those whose agent is gone.
func (l *reservationList) prune(now, sampled time.Time, procs []domain.ProcessSample) {
	var alive map[domain.ProcessKey]bool
	if procs != nil {
		alive = make(map[domain.ProcessKey]bool, len(procs))
		for _, p := range procs {
			alive[p.Key()] = true
		}
	}
	l.r = slices.DeleteFunc(l.r, func(r reservation) bool {
		if now.Sub(r.seen) >= reservationTTL {
			return true
		}
		return alive != nil && r.since.Before(sampled) && !alive[r.agent]
	})
}

// line returns the reservations as scheduler line entries.
func (l *reservationList) line() []scheduler.Waiter {
	out := make([]scheduler.Waiter, len(l.r))
	for i, r := range l.r {
		out[i] = r.waiter()
	}
	return out
}

func (r reservation) waiter() scheduler.Waiter {
	return scheduler.Waiter{Key: r.agent, Owner: r.owner, Since: r.since, Lease: true}
}

// reserve handles the "reserve" method.
func (d *Daemon) reserve(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ReserveParams
	if err := decodeParams(raw, &p); err != nil {
		return nil, err
	}
	if err := checkPID(p.PID); err != nil {
		return nil, err
	}
	self, code, why := d.caller(ctx, p.PID)
	if code != "" {
		return nil, fmt.Errorf("reserve refused: %s", why)
	}
	chain := d.ancestry(ctx, self.PPID)
	res, l := d.reserveFor(chain, d.ctl.Paused(), time.Now(), p.Wait)
	if l.ID != "" {
		d.emitAgentLease(l, res.Verdict.Reason)
	}
	return res, nil
}

// reserveFor decides a reserve by a caller whose ancestors are chain, for
// its nearest agent, under d.mu: an agent lease it holds answers at once
// (consumed unless wait); otherwise a free slot that is the agent's turn
// is granted as an agent lease; otherwise the agent's reservation is
// created or renewed. A zero Lease means none was granted now.
func (d *Daemon) reserveFor(chain []int, paused []control.PausedWorkload, now time.Time, wait bool) (ReserveResult, Lease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.statusWithLeases()
	st.Paused = paused
	a, ok := nearestAgentView(chain, st.Agents)
	if !ok {
		return ReserveResult{Reason: ReasonNoAgent}, Lease{}
	}
	if l, _, ok := d.leases.agentLease(a.Key); ok {
		if !wait {
			d.leases.consume(l.ID, now, agentHeavy(st, a.Key.PID))
		}
		return ReserveResult{Granted: true, Reason: "a heavy slot is held for " + a.Agent, LeaseID: l.ID}, Lease{}
	}
	v := assess(st, a.Agent, true)
	res := ReserveResult{Reason: v.Reason, Verdict: v}
	ahead := 0
	if st.Scheduler.Mode != scheduler.ModeOff {
		me := scheduler.Waiter{Key: a.Key, Owner: a.Agent, Since: d.reserved.since(a.Key, now), Lease: true}
		ahead = d.aheadInLine(me, st, now)
	}
	switch {
	case len(d.leases.leases) >= maxLeases:
		res.Reason = ReasonTooManyLeases
	case !v.OKToStart:
	case ahead > 0:
		res.Reason = fmt.Sprintf("older heavy work is waiting for a slot (%d ahead)", ahead)
		res.Verdict.OKToStart, res.Verdict.Reason = false, res.Reason
	default:
		var before []domain.ProcessKey
		if !wait {
			before = agentHeavy(st, a.Key.PID)
		}
		l := d.leases.grantAgent(a.Key, a.Agent, now, !wait, before)
		d.reserved.remove(a.Key)
		res.Granted, res.LeaseID = true, l.ID
		return res, l
	}
	if d.reserved.touch(a.Key, a.Agent, now) {
		res.Position = ahead + 1
	}
	return res, Lease{}
}

// grantReservations grants an agent lease to each reservation whose turn
// it is and for which a slot is free, oldest first. A slot freed while no
// `throt wait` polls then goes to the agent waiting longest instead of
// staying empty, and an abandoned reservation holds it at most leaseTTL.
func (d *Daemon) grantReservations(paused []control.PausedWorkload, now time.Time) []Lease {
	d.mu.Lock()
	defer d.mu.Unlock()
	byAge := slices.SortedFunc(slices.Values(d.reserved.r), func(a, b reservation) int {
		return cmp.Or(a.since.Compare(b.since), cmp.Compare(a.agent.PID, b.agent.PID))
	})
	var granted []Lease
	for _, r := range byAge {
		if len(d.leases.leases) >= maxLeases {
			break
		}
		st := d.statusWithLeases()
		st.Paused = paused
		if !assess(st, r.owner, true).OKToStart {
			continue
		}
		if st.Scheduler.Mode != scheduler.ModeOff && d.aheadInLine(r.waiter(), st, now) > 0 {
			continue
		}
		granted = append(granted, d.leases.grantAgent(r.agent, r.owner, now, false, nil))
		d.reserved.remove(r.agent)
	}
	return granted
}

// agentHeavy returns the keys of the heavy workloads the agent with PID
// agent owns in st.
func agentHeavy(st StatusResult, agent int) []domain.ProcessKey {
	var out []domain.ProcessKey
	for _, h := range st.Heavy {
		if h.OwnerPID == agent {
			out = append(out, h.Key)
		}
	}
	return out
}

// emitAgentLease emits lease_granted for an agent lease.
func (d *Daemon) emitAgentLease(l Lease, reason string) {
	d.emit(domain.EventLeaseGranted, fmt.Sprintf("lease %s granted to agent pid %d (%s) for its reservation: %s", l.ID, l.Process.PID, ownerLabel(l.Agent), reason),
		map[string]string{"lease_id": l.ID, "pid": strconv.Itoa(l.Process.PID), "agent": l.Agent})
}
