// Package scheduler decides when heavy workloads should wait for a slot.
//
// Throt can only act on processes that already exist, so "queue" means:
// when a new heavy workload appears and no slot is free, pause it at once so
// it waits, and resume it later. The scheduler only returns decisions; the
// daemon executes them through the controller and reports back which
// workloads are paused.
//
// The package is deterministic: it makes no OS calls, starts no goroutines,
// and reads time only from Input.Now. A Scheduler is not safe for
// concurrent use; the daemon calls Decide from one goroutine.
package scheduler

import (
	"cmp"
	"slices"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// Mode selects whether the scheduler runs and whether its decisions act.
type Mode string

const (
	// ModeOff disables the scheduler: no decisions, no state.
	ModeOff Mode = "off"
	// ModeObserve decides and explains but never acts: every decision has
	// Observed set.
	ModeObserve Mode = "observe"
	// ModeEnforce returns decisions the daemon executes.
	ModeEnforce Mode = "enforce"
)

// Action is what a decision asks the daemon to do.
type Action string

const (
	// ActionQueue pauses a new workload so it waits for a slot.
	ActionQueue Action = "queue"
	// ActionResume resumes a queued workload.
	ActionResume Action = "resume"
	// ActionPreempt pauses a running workload under critical pressure.
	ActionPreempt Action = "preempt"
)

// Reason codes. They are stable for logs and the local protocol.
const (
	ReasonSlotsExhausted   = "heavy_slots_exhausted"
	ReasonQueueWaiting     = "queue_waiting"
	ReasonPressureHigh     = "pressure_high"
	ReasonPressureCritical = "pressure_critical"
	ReasonSlotAvailable    = "slot_available"
	ReasonAged             = "aged"
)

// Policy defaults.
const (
	DefaultHeavyLimit     = 2
	DefaultResumeCooldown = 10 * time.Second
	DefaultMaxQueueWait   = 5 * time.Minute
)

// Policy configures the scheduler.
type Policy struct {
	Mode Mode
	// HeavyLimit is the maximum number of concurrently running heavy
	// workloads. Values < 1 are treated as 1.
	HeavyLimit int
	// ResumeCooldown is the minimum time between two resumes, two
	// preemptions, or a preemption and a resume in either order, which
	// avoids oscillation. Negative values are treated as 0.
	ResumeCooldown time.Duration
	// PauseOnCritical allows pausing a running workload under critical
	// pressure.
	PauseOnCritical bool
	// MaxQueueWait is the aging threshold: a workload queued at least this
	// long outranks every other queued workload. Values <= 0 use the
	// default.
	MaxQueueWait time.Duration
}

// DefaultPolicy returns the default policy, which is off.
func DefaultPolicy() Policy {
	return Policy{
		Mode:           ModeOff,
		HeavyLimit:     DefaultHeavyLimit,
		ResumeCooldown: DefaultResumeCooldown,
		MaxQueueWait:   DefaultMaxQueueWait,
	}
}

func (p Policy) normalized() Policy {
	switch p.Mode {
	case ModeObserve, ModeEnforce:
	default:
		p.Mode = ModeOff
	}
	p.HeavyLimit = max(p.HeavyLimit, 1)
	p.ResumeCooldown = max(p.ResumeCooldown, 0)
	if p.MaxQueueWait <= 0 {
		p.MaxQueueWait = DefaultMaxQueueWait
	}
	return p
}

// Input is one decision round's view of the host.
type Input struct {
	Now      time.Time
	Pressure domain.PressureLevel
	// Heavy is the latest snapshot of heavy workloads.
	Heavy []process.WorkloadView
	// Paused lists workloads currently paused by the scheduler (queued or
	// preempted). Manual pauses must not be listed here.
	Paused []domain.ProcessKey
	// Held lists workloads paused manually. The scheduler never touches
	// them and does not count them as running.
	Held []domain.ProcessKey
}

// Decision is one action with the facts that justify it.
type Decision struct {
	Action Action            `json:"decision"`
	Key    domain.ProcessKey `json:"process"`
	Name   string            `json:"name"`
	Owner  string            `json:"owner"`
	// Reason is a stable code, e.g. heavy_slots_exhausted.
	Reason   string               `json:"reason"`
	Pressure domain.PressureLevel `json:"pressure"`
	// RunningHeavy is the number of running heavy workloads when the
	// decision was made, including any workload it resumes.
	RunningHeavy int `json:"running_heavy"`
	HeavyLimit   int `json:"heavy_limit"`
	// Observed is set in observe mode: the caller must not act.
	Observed bool `json:"observed,omitempty"`
}

// entry is what the scheduler remembers about one heavy workload. Entries
// are pruned as soon as the workload leaves the snapshot.
type entry struct {
	firstSeen time.Time
	// queuedSince is when the workload started waiting; zero when running.
	queuedSince time.Time
	preemptions int
	// preemptPending marks a preempt decision not yet confirmed by Paused.
	preemptPending bool
}

// Scheduler is a deterministic heavy-slot scheduler.
type Scheduler struct {
	policy Policy
	// baselined is false until the first round after start or a policy
	// change; that round adopts every workload as pre-existing.
	baselined bool
	seen      map[domain.ProcessKey]*entry
	// lastSwitch is when the last resume or preemption was decided.
	lastSwitch time.Time
}

// New returns a scheduler with policy p.
func New(p Policy) *Scheduler {
	return &Scheduler{policy: p.normalized(), seen: map[domain.ProcessKey]*entry{}}
}

// Policy returns the effective (normalized) policy.
func (s *Scheduler) Policy() Policy { return s.policy }

// SetPolicy replaces the policy. Workloads present at the next round are
// adopted as pre-existing and never queued; queue state of workloads that
// are already paused is kept so they are still resumed.
func (s *Scheduler) SetPolicy(p Policy) {
	s.policy = p.normalized()
	s.baselined = false
}

// cand is one workload in the current round.
type cand struct {
	v      process.WorkloadView
	e      *entry
	isNew  bool
	paused bool
	held   bool
}

// Decide returns this round's decisions, sorted by action (resume, queue,
// preempt) and then by process key.
func (s *Scheduler) Decide(in Input) []Decision {
	p := s.policy
	if p.Mode == ModeOff {
		s.reset()
		return nil
	}
	if !s.lastSwitch.IsZero() && in.Now.Before(s.lastSwitch) {
		// The clock went backwards; do not let the cooldown block forever.
		s.lastSwitch = time.Time{}
	}

	paused := keySet(in.Paused)
	held := keySet(in.Held)
	cands := s.observe(in, paused, held)

	// Running heavy workloads, before admitting new ones. Unattributed
	// workloads count: they consume resources even though they are never
	// controlled.
	running := 0
	perOwner := map[string]int{}
	var queued, admissible []*cand
	for _, c := range cands {
		switch {
		case c.held:
		case c.paused:
			queued = append(queued, c)
		case c.isNew && c.v.Owner != "":
			admissible = append(admissible, c)
		default:
			running++
			perOwner[c.v.Owner]++
		}
	}

	var out []Decision
	add := func(a Action, c *cand, reason string) {
		out = append(out, Decision{
			Action: a, Key: c.v.Key, Name: c.v.Name, Owner: c.v.Owner,
			Reason: reason, Pressure: in.Pressure,
			RunningHeavy: running, HeavyLimit: p.HeavyLimit,
			Observed: p.Mode == ModeObserve,
		})
	}

	// Resume at most one queued workload.
	if c, reason := s.pickResume(in, queued, running, perOwner); c != nil {
		running++
		perOwner[c.v.Owner]++
		add(ActionResume, c, reason)
		queued = slices.DeleteFunc(queued, func(q *cand) bool { return q == c })
		s.lastSwitch = in.Now
	}

	// Admit or queue new workloads in a stable order.
	slices.SortFunc(admissible, func(a, b *cand) int { return cmpKey(a.v.Key, b.v.Key) })
	for _, c := range admissible {
		reason := ""
		switch in.Pressure {
		case domain.PressureUnknown:
			// Degraded metrics: fail open.
		case domain.PressureHigh:
			reason = ReasonPressureHigh
		case domain.PressureCritical:
			reason = ReasonPressureCritical
		default:
			switch {
			case running >= p.HeavyLimit:
				reason = ReasonSlotsExhausted
			case len(queued) > 0:
				// Earlier workloads are waiting; a newcomer must not
				// overtake them, or a long queue could starve.
				reason = ReasonQueueWaiting
			}
		}
		if reason == "" {
			running++
			perOwner[c.v.Owner]++
			continue
		}
		c.e.queuedSince = in.Now
		add(ActionQueue, c, reason)
	}

	// Preempt at most one running attributed workload under critical
	// pressure: the youngest, ties broken by the higher PID. The last
	// running attributed workload is never preempted, so work always
	// progresses, and preemptions share the resume cooldown.
	if in.Pressure == domain.PressureCritical && p.PauseOnCritical && s.cooledDown(in.Now) {
		var victim *cand
		eligible := 0
		for _, c := range cands {
			// New workloads were queued above, under critical pressure.
			if c.held || c.paused || c.isNew || c.v.Owner == "" {
				continue
			}
			eligible++
			if victim == nil || c.e.firstSeen.After(victim.e.firstSeen) ||
				(c.e.firstSeen.Equal(victim.e.firstSeen) && c.v.Key.PID > victim.v.Key.PID) {
				victim = c
			}
		}
		if eligible > 1 {
			s.lastSwitch = in.Now
			victim.e.queuedSince = in.Now
			victim.e.preemptPending = true
			add(ActionPreempt, victim, ReasonPressureCritical)
			running--
		}
	}

	slices.SortFunc(out, func(a, b Decision) int {
		return cmp.Or(cmp.Compare(actionRank(a.Action), actionRank(b.Action)), cmpKey(a.Key, b.Key))
	})
	return out
}

// observe updates per-workload state from the snapshot and returns this
// round's candidates in snapshot order.
func (s *Scheduler) observe(in Input, paused, held map[domain.ProcessKey]struct{}) []*cand {
	present := make(map[domain.ProcessKey]struct{}, len(in.Heavy))
	cands := make([]*cand, 0, len(in.Heavy))
	for _, v := range in.Heavy {
		if _, dup := present[v.Key]; dup {
			continue
		}
		present[v.Key] = struct{}{}
		c := &cand{v: v}
		_, c.paused = paused[v.Key]
		_, c.held = held[v.Key]
		if c.held {
			c.paused = false
		}
		e, ok := s.seen[v.Key]
		if !ok {
			e = &entry{firstSeen: in.Now}
			s.seen[v.Key] = e
			// Only workloads that appear after the baseline are new;
			// held workloads are never queued.
			c.isNew = s.baselined && !c.held
		}
		c.e = e
		switch {
		case c.paused:
			if e.queuedSince.IsZero() {
				// Paused before this scheduler knew it (e.g. restart).
				e.queuedSince = in.Now
			}
			if e.preemptPending {
				e.preemptions++
			}
		default:
			// Resumed, or a queue/preempt decision was not executed.
			e.queuedSince = time.Time{}
		}
		e.preemptPending = false
		cands = append(cands, c)
	}
	for k := range s.seen {
		if _, ok := present[k]; !ok {
			delete(s.seen, k)
		}
	}
	s.baselined = true
	return cands
}

// pickResume returns the queued workload to resume this round, if any, and
// the reason.
func (s *Scheduler) pickResume(in Input, queued []*cand, running int, perOwner map[string]int) (*cand, string) {
	p := s.policy
	if len(queued) == 0 {
		return nil, ""
	}
	if !s.cooledDown(in.Now) {
		return nil, ""
	}
	aged := func(c *cand) bool { return in.Now.Sub(c.e.queuedSince) >= p.MaxQueueWait }
	best := slices.MinFunc(queued, func(a, b *cand) int {
		return cmp.Or(
			-cmpBool(aged(a), aged(b)),
			cmp.Compare(perOwner[a.v.Owner], perOwner[b.v.Owner]),
			cmp.Compare(a.e.preemptions, b.e.preemptions),
			a.e.queuedSince.Compare(b.e.queuedSince),
			cmpKey(a.v.Key, b.v.Key),
		)
	})
	reason := ReasonSlotAvailable
	if aged(best) {
		reason = ReasonAged
	}
	switch in.Pressure {
	case domain.PressureNormal, domain.PressureElevated:
		if running < p.HeavyLimit {
			return best, reason
		}
	case domain.PressureHigh:
		// Pausing frees no memory, so high pressure can outlast the queue.
		// Aged work still gets a free slot, one per cooldown, so nothing
		// starves.
		if running < p.HeavyLimit && aged(best) {
			return best, reason
		}
	case domain.PressureUnknown:
		// Fail open: resume with a free slot, and resume aged workloads
		// regardless of slots so nothing waits forever on bad metrics.
		if running < p.HeavyLimit || aged(best) {
			return best, reason
		}
	}
	return nil, ""
}

// cooledDown reports whether ResumeCooldown has elapsed since the last
// resume or preemption.
func (s *Scheduler) cooledDown(now time.Time) bool {
	return s.lastSwitch.IsZero() || now.Sub(s.lastSwitch) >= s.policy.ResumeCooldown
}

func (s *Scheduler) reset() {
	s.baselined = false
	s.lastSwitch = time.Time{}
	clear(s.seen)
}

func keySet(keys []domain.ProcessKey) map[domain.ProcessKey]struct{} {
	m := make(map[domain.ProcessKey]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

func cmpKey(a, b domain.ProcessKey) int {
	return cmp.Or(cmp.Compare(a.PID, b.PID), a.Start.Compare(b.Start))
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

func actionRank(a Action) int {
	switch a {
	case ActionResume:
		return 0
	case ActionQueue:
		return 1
	}
	return 2
}
