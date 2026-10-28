// Package stats accumulates per-agent resource usage from process graph
// snapshots: CPU time, peak memory, heavy workloads started, and time spent
// queued by the scheduler. It is deterministic: it makes no OS calls, starts
// no goroutines, and takes time only from the caller. Memory is bounded by
// MaxAgents rows.
package stats

import (
	"cmp"
	"slices"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// MaxAgents bounds the agent instances retained. Beyond it, the instance
// seen least recently is evicted; its usage stays in the totals.
const MaxAgents = 256

// Row is the usage of one agent instance, or of a bucket.
type Row struct {
	// Agent is the instance label (e.g. "claude-1"). Labels are reused
	// after an instance exits, so Process identifies the instance.
	Agent   string            `json:"agent,omitempty"`
	Process domain.ProcessKey `json:"process"`
	// CPUSeconds is CPU time used by the agent and its owned descendants
	// while observed.
	CPUSeconds float64 `json:"cpu_seconds"`
	// PeakRSS is the largest resident memory seen in one sample, in bytes.
	PeakRSS uint64 `json:"peak_rss"`
	// HeavyStarted counts distinct heavy workloads observed.
	HeavyStarted int `json:"heavy_started"`
	// QueuedSeconds is time its heavy workloads spent paused by the
	// scheduler (queued or preempted). Manual pauses are not counted.
	QueuedSeconds float64   `json:"queued_seconds"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
}

// ActiveFor is how long the row was observed.
func (r Row) ActiveFor() time.Duration { return r.LastSeen.Sub(r.FirstSeen) }

func (r *Row) seen(now time.Time) {
	if r.FirstSeen.IsZero() {
		r.FirstSeen = now
	}
	if now.After(r.LastSeen) {
		r.LastSeen = now
	}
}

// Report is a copy of the accounting.
type Report struct {
	// Since is the start of the window: the first observation. Zero until
	// one is made.
	Since time.Time `json:"since"`
	// Agents is sorted by CPU seconds descending, then PID ascending.
	Agents []Row `json:"agents"`
	// Total sums every agent instance, including evicted ones. Its PeakRSS
	// is the largest sum of agent memory in one sample.
	Total Row `json:"total"`
	// Unattributed covers heavy workloads no agent owns.
	Unattributed Row `json:"unattributed"`
}

// Accountant accumulates usage across Observe calls. It is not safe for
// concurrent use.
type Accountant struct {
	since   time.Time
	last    time.Time
	hasLast bool
	agents  map[domain.ProcessKey]*Row
	total   Row
	unattr  Row
	// heavy maps each heavy workload in the previous observation to its
	// owning agent (zero key when unattributed); bounded by the snapshot.
	heavy map[domain.ProcessKey]heavyRec
	// queued holds the workloads paused by the scheduler at the previous
	// observation; they are charged for the interval that follows.
	queued []domain.ProcessKey
}

type heavyRec struct {
	owner      domain.ProcessKey
	attributed bool
	// credited is set once the workload counted toward an agent's heavy
	// jobs; until then it counts as unattributed.
	credited bool
}

// New returns an empty accountant.
func New() *Accountant {
	return &Accountant{agents: map[domain.ProcessKey]*Row{}, heavy: map[domain.ProcessKey]heavyRec{}}
}

// Observe accounts one snapshot taken at now. queued lists the heavy
// workloads the scheduler holds paused from now on. CPU rates in snap cover
// the time since the previous snapshot, so CPU seconds are rate × the time
// since the previous Observe; callers must observe every snapshot they
// take. A now that does not advance accounts memory and starts only.
func (a *Accountant) Observe(now time.Time, snap process.Snapshot, queued []domain.ProcessKey) {
	dt := 0.0
	if a.hasLast {
		dt = max(now.Sub(a.last).Seconds(), 0)
	}
	if !a.hasLast {
		a.since = now
	}
	a.last, a.hasLast = now, true

	// Queue time for the interval that just ended.
	for _, k := range a.queued {
		h, ok := a.heavy[k]
		switch {
		case !ok:
		case h.attributed:
			if r := a.agents[h.owner]; r != nil {
				r.QueuedSeconds += dt
			}
			a.total.QueuedSeconds += dt
		default:
			a.unattr.QueuedSeconds += dt
		}
	}
	a.queued = slices.Clone(queued)

	byPID := make(map[int]domain.ProcessKey, len(snap.Agents))
	var rss uint64
	for _, v := range snap.Agents {
		byPID[v.Key.PID] = v.Key
		r := a.agents[v.Key]
		if r == nil {
			r = &Row{Process: v.Key}
			a.agents[v.Key] = r
		}
		r.Agent = v.Agent
		r.seen(now)
		r.CPUSeconds += v.CPU * dt
		r.PeakRSS = max(r.PeakRSS, v.RSS)
		a.total.CPUSeconds += v.CPU * dt
		rss += v.RSS
	}
	if len(snap.Agents) > 0 {
		a.total.seen(now)
		a.total.PeakRSS = max(a.total.PeakRSS, rss)
	}

	heavy := make(map[domain.ProcessKey]heavyRec, len(snap.Heavy))
	var unattrRSS uint64
	var anyUnattr bool
	for _, w := range snap.Heavy {
		owner, attributed := byPID[w.OwnerPID]
		attributed = attributed && w.Owner != ""
		prev, old := a.heavy[w.Key]
		rec := heavyRec{owner: owner, attributed: attributed, credited: old && prev.credited}
		if attributed && !rec.credited {
			// A workload first seen unattributed (its agent missing
			// from a snapshot) moves to the agent once it is known.
			if old {
				a.unattr.HeavyStarted--
			}
			a.agents[owner].HeavyStarted++
			a.total.HeavyStarted++
			rec.credited = true
		}
		heavy[w.Key] = rec
		if attributed {
			continue
		}
		anyUnattr = true
		if !old {
			a.unattr.HeavyStarted++
		}
		a.unattr.CPUSeconds += w.CPU * dt
		unattrRSS += w.RSS
	}
	if anyUnattr {
		a.unattr.seen(now)
		a.unattr.PeakRSS = max(a.unattr.PeakRSS, unattrRSS)
	}
	a.heavy = heavy
	a.evict()
}

// evict drops the least recently seen instances beyond MaxAgents. Among
// equals the most recently first seen goes first, so a newcomer cannot
// push out an instance with history when more than MaxAgents are live.
func (a *Accountant) evict() {
	if len(a.agents) <= MaxAgents {
		return
	}
	rows := make([]*Row, 0, len(a.agents))
	for _, r := range a.agents {
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(x, y *Row) int {
		return cmp.Or(x.LastSeen.Compare(y.LastSeen), y.FirstSeen.Compare(x.FirstSeen), cmp.Compare(y.Process.PID, x.Process.PID))
	})
	for _, r := range rows[:len(rows)-MaxAgents] {
		delete(a.agents, r.Process)
	}
}

// Report returns a copy of the accounting.
func (a *Accountant) Report() Report {
	rep := Report{Since: a.since, Agents: make([]Row, 0, len(a.agents)), Total: a.total, Unattributed: a.unattr}
	for _, r := range a.agents {
		rep.Agents = append(rep.Agents, *r)
	}
	slices.SortFunc(rep.Agents, func(x, y Row) int {
		return cmp.Or(cmp.Compare(y.CPUSeconds, x.CPUSeconds), cmp.Compare(x.Process.PID, y.Process.PID), x.Process.Start.Compare(y.Process.Start))
	})
	return rep
}
