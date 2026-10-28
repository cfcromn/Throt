// Package scheduler decides when heavy workloads should wait for a slot.
//
// Throt can only act on processes that already exist, so "queue" means:
// when a new heavy workload appears and no slot is free, pause it at once so
// it waits, and resume it later. The scheduler only returns decisions; the
// daemon executes them through the controller and reports back which
// workloads are paused.
//
// With Policy.Adaptive the number of heavy slots follows pressure (AIMD):
// HeavyLimit is the ceiling, and the effective limit grows by one per
// AdaptInterval under normal pressure and halves under high or critical
// pressure. Limit changes are not decisions; the daemon drains them with
// Changes and reports them as events.
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
	ReasonOwnerBudget      = "owner_budget_exhausted"
	// Limit change reasons; ReasonPressureHigh and ReasonPressureCritical
	// are also used for decreases.
	ReasonPressureNormal  = "pressure_normal"
	ReasonPressureUnknown = "pressure_unknown"
	ReasonPolicyChanged   = "policy_changed"
)

// Policy defaults.
const (
	DefaultHeavyLimit     = 2
	DefaultResumeCooldown = 10 * time.Second
	DefaultMaxQueueWait   = 5 * time.Minute
	DefaultAdaptInterval  = 30 * time.Second
)

// MaxLimitChanges bounds the limit changes retained between two calls to
// Changes; the oldest are dropped first.
const MaxLimitChanges = 32

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
	// Adaptive makes HeavyLimit a ceiling: the effective limit starts there
	// and follows pressure between 1 and HeavyLimit. Lowering it never
	// preempts; new starts and resumes just wait.
	Adaptive bool
	// AdaptInterval is the minimum time between two adaptive limit changes
	// driven by normal, high or critical pressure. Values <= 0 use the
	// default.
	AdaptInterval time.Duration
	// PerOwnerLimit caps the running heavy workloads of one owner, so one
	// agent cannot take every slot. 0 means unlimited; negative values are
	// treated as 0.
	PerOwnerLimit int
}

// DefaultPolicy returns the default policy, which is off.
func DefaultPolicy() Policy {
	return Policy{
		Mode:           ModeOff,
		HeavyLimit:     DefaultHeavyLimit,
		ResumeCooldown: DefaultResumeCooldown,
		MaxQueueWait:   DefaultMaxQueueWait,
		AdaptInterval:  DefaultAdaptInterval,
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
	if p.AdaptInterval <= 0 {
		p.AdaptInterval = DefaultAdaptInterval
	}
	p.PerOwnerLimit = max(p.PerOwnerLimit, 0)
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
	// Reserved holds the owner ("" when unattributed) of each admission
	// lease: a slot promised to a command that has not shown up as a heavy
	// workload yet. Each counts as one running heavy workload of its owner,
	// so a newcomer is never admitted into a promised slot. The daemon drops
	// a lease once its workload appears, so nothing is counted twice.
	Reserved []string
	// Admitted lists workloads that took over an admission lease this
	// round. They move into the slot the lease held, so they are never
	// queued, not even behind workloads that queued meanwhile.
	Admitted []domain.ProcessKey
	// Waiters are the `throt run` callers waiting for an admission lease.
	// They share one FIFO with queued workloads (see Policy.FirstInLine):
	// a queued workload is not resumed, and a newcomer not admitted, while a
	// waiter that could take the slot is ahead in line.
	Waiters []Waiter
}

// Waiter is one entry in the line for a heavy slot: a queued workload, or a
// `throt run` caller refused an admission lease (Lease set).
type Waiter struct {
	Key   domain.ProcessKey
	Owner string
	// Since is when it started waiting: when the workload was queued, or
	// when the caller first asked.
	Since time.Time
	Lease bool
}

// ahead reports whether a comes before b in the line: entries waiting at
// least MaxQueueWait come first, then the longer wait; ties go to queued
// workloads, then to the lower key. p must be normalized.
func (p Policy) ahead(now time.Time, a, b Waiter) bool {
	agedA, agedB := now.Sub(a.Since) >= p.MaxQueueWait, now.Sub(b.Since) >= p.MaxQueueWait
	return cmp.Or(
		-cmpBool(agedA, agedB),
		a.Since.Compare(b.Since),
		cmpBool(a.Lease, b.Lease),
		cmpKey(a.Key, b.Key),
	) < 0
}

// FirstInLine reports whether w comes first in the one FIFO shared by lease
// waiters and queued workloads: no entry of line (w itself is skipped) is
// ahead of it, counting only entries whose owner is within its
// PerOwnerLimit, given perOwner (running and reserved heavy work per owner).
// Work held back only by its owner's budget does not block other owners.
func (p Policy) FirstInLine(now time.Time, w Waiter, line []Waiter, perOwner map[string]int) bool {
	p = p.normalized()
	for _, o := range line {
		if o.Key == w.Key && o.Lease == w.Lease {
			continue
		}
		if p.PerOwnerLimit > 0 && perOwner[o.Owner] >= p.PerOwnerLimit {
			continue
		}
		if p.ahead(now, o, w) {
			return false
		}
	}
	return true
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
	// decision was made, including any workload it resumes and every
	// reserved slot (Input.Reserved).
	RunningHeavy int `json:"running_heavy"`
	// HeavyLimit is the effective limit, which Adaptive may lower below
	// Policy.HeavyLimit.
	HeavyLimit int `json:"heavy_limit"`
	// Observed is set in observe mode: the caller must not act.
	Observed bool `json:"observed,omitempty"`
}

// LimitChange records one change of the effective heavy limit.
type LimitChange struct {
	From   int       `json:"from"`
	To     int       `json:"to"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// entry is what the scheduler remembers about one heavy workload. Entries
// are pruned as soon as the workload leaves the snapshot.
type entry struct {
	owner     string
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
	// limit is the effective heavy limit, in [1, policy.HeavyLimit].
	limit int
	// lastAdapt is when the last interval-limited limit change happened;
	// zero allows one at once.
	lastAdapt time.Time
	// lastRound is Input.Now of the latest round, used to timestamp limit
	// changes made by SetPolicy.
	lastRound time.Time
	// changes are limit changes not yet drained by Changes.
	changes []LimitChange
}

// New returns a scheduler with policy p.
func New(p Policy) *Scheduler {
	p = p.normalized()
	return &Scheduler{policy: p, seen: map[domain.ProcessKey]*entry{}, limit: p.HeavyLimit}
}

// Policy returns the effective (normalized) policy.
func (s *Scheduler) Policy() Policy { return s.policy }

// SetPolicy replaces the policy. Workloads present at the next round are
// adopted as pre-existing and never queued; queue state of workloads that
// are already paused is kept so they are still resumed.
//
// If the scheduler was and stays adaptive, the effective limit is kept,
// clamped to the new ceiling; otherwise it becomes the new HeavyLimit.
func (s *Scheduler) SetPolicy(p Policy) {
	p = p.normalized()
	limit := p.HeavyLimit
	if p.Adaptive && s.policy.Adaptive {
		limit = min(s.limit, p.HeavyLimit)
	}
	s.policy = p
	s.baselined = false
	s.lastAdapt = time.Time{}
	s.setLimit(limit, ReasonPolicyChanged, s.lastRound)
}

// Limit returns the effective heavy limit: Policy().HeavyLimit unless
// Adaptive lowered it.
func (s *Scheduler) Limit() int { return s.limit }

// Queued returns the workloads waiting in line (queued or preempted) as of
// the latest round, for Policy.FirstInLine.
func (s *Scheduler) Queued() []Waiter {
	var out []Waiter
	for k, e := range s.seen {
		if !e.queuedSince.IsZero() {
			out = append(out, Waiter{Key: k, Owner: e.owner, Since: e.queuedSince})
		}
	}
	slices.SortFunc(out, func(a, b Waiter) int { return cmpKey(a.Key, b.Key) })
	return out
}

// Changes returns the limit changes since the previous call, oldest first,
// and forgets them. At most MaxLimitChanges are kept. A change made by
// SetPolicy is stamped with the time of the latest round.
func (s *Scheduler) Changes() []LimitChange {
	out := s.changes
	s.changes = nil
	return out
}

// adapt moves the effective limit for this round's pressure:
//
//   - normal: +1, at most once per AdaptInterval, up to HeavyLimit;
//   - elevated: hold;
//   - high or critical: halve (floor 1), at most once per AdaptInterval;
//   - unknown: reset to HeavyLimit at once (fail open).
func (s *Scheduler) adapt(now time.Time, pressure domain.PressureLevel) {
	p := s.policy
	if !p.Adaptive {
		return
	}
	if !s.lastAdapt.IsZero() && now.Before(s.lastAdapt) {
		// The clock went backwards; do not let the interval block forever.
		s.lastAdapt = time.Time{}
	}
	due := s.lastAdapt.IsZero() || now.Sub(s.lastAdapt) >= p.AdaptInterval
	next, reason := s.limit, ""
	switch pressure {
	case domain.PressureUnknown:
		// Degraded metrics must not keep work throttled. The reset is not
		// rate limited and does not delay the next decrease.
		s.lastAdapt = time.Time{}
		s.setLimit(p.HeavyLimit, ReasonPressureUnknown, now)
		return
	case domain.PressureNormal:
		next, reason = min(s.limit+1, p.HeavyLimit), ReasonPressureNormal
	case domain.PressureHigh:
		next, reason = max(s.limit/2, 1), ReasonPressureHigh
	case domain.PressureCritical:
		next, reason = max(s.limit/2, 1), ReasonPressureCritical
	}
	if due && next != s.limit {
		s.lastAdapt = now
		s.setLimit(next, reason, now)
	}
}

// setLimit changes the effective limit and records the change.
func (s *Scheduler) setLimit(limit int, reason string, at time.Time) {
	if limit == s.limit {
		return
	}
	if len(s.changes) == MaxLimitChanges {
		s.changes = append(s.changes[:0], s.changes[1:]...)
	}
	s.changes = append(s.changes, LimitChange{From: s.limit, To: limit, Reason: reason, At: at})
	s.limit = limit
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
	s.lastRound = in.Now
	s.adapt(in.Now, in.Pressure)
	limit := s.limit

	paused := keySet(in.Paused)
	held := keySet(in.Held)
	cands := s.observe(in, paused, held, keySet(in.Admitted))

	// Running heavy workloads, before admitting new ones. Unattributed
	// workloads count: they consume resources even though they are never
	// controlled. So do reserved slots.
	running := len(in.Reserved)
	perOwner := map[string]int{}
	for _, o := range in.Reserved {
		perOwner[o]++
	}
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
			RunningHeavy: running, HeavyLimit: limit,
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
			case s.atBudget(c, perOwner):
				reason = ReasonOwnerBudget
			case running >= limit:
				reason = ReasonSlotsExhausted
			case slices.ContainsFunc(queued, func(q *cand) bool { return !s.atBudget(q, perOwner) }),
				slices.ContainsFunc(in.Waiters, func(w Waiter) bool { return !s.ownerAtBudget(w.Owner, perOwner) }):
				// Earlier workloads or lease waiters that could run are
				// waiting; a newcomer must not overtake them, or a long
				// queue could starve. Work held back only by its owner's
				// budget does not block other owners.
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
func (s *Scheduler) observe(in Input, paused, held, admitted map[domain.ProcessKey]struct{}) []*cand {
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
			// held and admitted workloads are never queued.
			_, ok := admitted[v.Key]
			c.isNew = s.baselined && !c.held && !ok
		}
		c.e = e
		e.owner = v.Owner
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
	if len(queued) == 0 || !s.cooledDown(in.Now) {
		return nil, ""
	}
	aged := func(c *cand) bool { return in.Now.Sub(c.e.queuedSince) >= p.MaxQueueWait }
	// Skip owners at budget. Under unknown pressure aged work ignores the
	// budget as it ignores the slot limit, so bad metrics cannot strand it.
	queued = slices.DeleteFunc(slices.Clone(queued), func(c *cand) bool {
		return s.atBudget(c, perOwner) && (in.Pressure != domain.PressureUnknown || !aged(c))
	})
	if len(queued) == 0 {
		return nil, ""
	}
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
		// An older lease waiter gets the free slot through acquire.
		if running < s.limit && !s.yields(in, best, perOwner) {
			return best, reason
		}
	case domain.PressureHigh:
		// Pausing frees no memory, so high pressure can outlast the queue.
		// Aged work still gets a free slot under the configured ceiling (not
		// the adaptive limit, which halves to 1 here), one per cooldown, so
		// nothing starves.
		if running < p.HeavyLimit && aged(best) {
			return best, reason
		}
	case domain.PressureUnknown:
		// Fail open: resume with a free slot, and resume aged workloads
		// regardless of slots so nothing waits forever on bad metrics.
		if (running < s.limit && !s.yields(in, best, perOwner)) || (running >= s.limit && aged(best)) {
			return best, reason
		}
	}
	return nil, ""
}

// atBudget reports whether c's owner already runs PerOwnerLimit heavy
// workloads.
func (s *Scheduler) atBudget(c *cand, perOwner map[string]int) bool {
	return s.ownerAtBudget(c.v.Owner, perOwner)
}

func (s *Scheduler) ownerAtBudget(owner string, perOwner map[string]int) bool {
	return s.policy.PerOwnerLimit > 0 && perOwner[owner] >= s.policy.PerOwnerLimit
}

// yields reports whether a lease waiter that could take a free slot is
// ahead of c in line, so c must not be resumed into that slot.
func (s *Scheduler) yields(in Input, c *cand, perOwner map[string]int) bool {
	me := Waiter{Key: c.v.Key, Owner: c.v.Owner, Since: c.e.queuedSince}
	return slices.ContainsFunc(in.Waiters, func(w Waiter) bool {
		return !s.ownerAtBudget(w.Owner, perOwner) && s.policy.ahead(in.Now, w, me)
	})
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
	s.limit = s.policy.HeavyLimit
	s.lastAdapt = time.Time{}
	s.changes = nil
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
