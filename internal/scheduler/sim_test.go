package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// simJob is one heavy workload in a simulation. Times are in seconds.
type simJob struct {
	owner  string // "" is unattributed
	arrive int
	dur    int  // seconds of work; a paused job makes no progress
	pre    bool // already running when the scheduler starts (arrive must be 0)
	rss    uint64
}

type simScenario struct {
	name     string
	policy   Policy
	jobs     []simJob
	pressure func(t int) domain.PressureLevel
	// throughputOnly, when set, is Input.ThroughputOnly at each tick.
	throughputOnly func(t int) bool
	// memory, when set, is the host's memory: Input.MemoryHeadroom is what
	// is left of 3/4 of it (the elevated exit threshold) after the RSS of
	// every job that has arrived and not finished, paused or not.
	memory uint64
	// wantPreempt requires at least one preemption.
	wantPreempt bool
	// noBound skips the makespan bound, for scenarios where pressure
	// rather than the slot limit sets the makespan.
	noBound bool
	// check runs scenario-specific assertions.
	check func(t *testing.T, r simResult)
}

type simResult struct {
	peak, peakControlled       int
	queued, resumed, preempted int
	maxWait, makespan, ideal   int
	// limitMin and limitMax are the range of the effective limit.
	limitMin, limitMax int
	// limitEnd is the effective limit when the last job finished.
	limitEnd     int
	limitChanges int
	// ownerMaxWait and ownerFinish are per owner: the longest single wait
	// and when the owner's last job finished, relative to the first arrival.
	ownerMaxWait, ownerFinish map[string]int
}

func normalPressure(int) domain.PressureLevel { return domain.PressureNormal }

// runSim drives the scheduler with a 1s tick, executing every decision the
// way the daemon would, and checks the invariants on every tick.
func runSim(t *testing.T, sc simScenario) simResult {
	t.Helper()
	const maxTicks = 100000
	n := len(sc.jobs)
	remaining := make([]int, n)
	finish := make([]int, n)
	waitStart := make([]int, n)
	paused := make([]bool, n)
	for i, j := range sc.jobs {
		if j.pre != (j.arrive == 0) {
			t.Fatalf("job %d: pre-existing jobs, and only those, arrive at 0", i)
		}
		remaining[i], finish[i] = j.dur, -1
	}
	index := map[domain.ProcessKey]int{}
	keyOf := func(i int) domain.ProcessKey { return domain.ProcessKey{PID: 1000 + i, Start: t0} }
	for i := range sc.jobs {
		index[keyOf(i)] = i
	}

	s := New(sc.policy)
	limit := s.Policy().HeavyLimit
	budget := s.Policy().PerOwnerLimit
	cooldown := int(s.Policy().ResumeCooldown / time.Second)
	maxQueueWait := int(s.Policy().MaxQueueWait / time.Second)
	lastSwitch, lastPreempt, preemptedNow := -1, -1, false
	res := simResult{limitMin: s.Limit(), limitMax: s.Limit(), ownerMaxWait: map[string]int{}, ownerFinish: map[string]int{}}
	done := 0
	// counts returns the running controlled jobs and the running jobs per
	// owner at tick.
	counts := func(tick int) (int, map[string]int) {
		ctl, perOwner := 0, map[string]int{}
		for i, j := range sc.jobs {
			if j.arrive <= tick && finish[i] < 0 && !paused[i] {
				perOwner[j.owner]++
				if j.owner != "" && !j.pre {
					ctl++
				}
			}
		}
		return ctl, perOwner
	}
	prevCtl, prevOwner := 0, map[string]int{}
	tick := 0
	for ; done < n; tick++ {
		if tick > maxTicks {
			t.Fatalf("%s: %d of %d jobs never completed", sc.name, n-done, n)
		}
		pressure := sc.pressure(tick)
		in := Input{Now: at(tick), Pressure: pressure}
		if sc.throughputOnly != nil {
			in.ThroughputOnly = sc.throughputOnly(tick)
		}
		var used uint64
		for i, j := range sc.jobs {
			if finish[i] == tick {
				in.Exited = append(in.Exited, keyOf(i))
			}
			if j.arrive <= tick && finish[i] < 0 {
				in.Heavy = append(in.Heavy, process.WorkloadView{Key: keyOf(i), Name: "build", Class: domain.ClassBuild, Owner: j.owner, RSS: j.rss, CPU: 1})
				used += j.rss
				if paused[i] {
					in.Paused = append(in.Paused, keyOf(i))
				}
			}
		}
		if room := sc.memory / 4 * 3; room > used {
			in.MemoryHeadroom = room - used
		}
		preemptedNow = false
		decisions := s.Decide(in)
		lim := s.Limit()
		if lim < 1 || lim > limit {
			t.Fatalf("%s t=%d: effective limit %d outside [1, %d]", sc.name, tick, lim, limit)
		}
		res.limitMin, res.limitMax, res.limitEnd = min(res.limitMin, lim), max(res.limitMax, lim), lim
		res.limitChanges += len(s.Changes())
		for _, d := range decisions {
			if d.HeavyLimit != lim {
				t.Fatalf("%s t=%d: decision limit %d, effective %d", sc.name, tick, d.HeavyLimit, lim)
			}
		}
		// Resumes and preemptions are at least one cooldown apart, except
		// resumes exempt from it: at normal or throughput-only pressure, or
		// into a slot an exit freed, and never within a cooldown of a
		// preemption. So a round has at most one switch that is not exempt.
		spaced, switched := 0, false
		for _, d := range decisions {
			if d.Action == ActionQueue {
				continue
			}
			switched = true
			if cooldownExemptResume(d, pressure, in.ThroughputOnly) {
				if lastPreempt >= 0 && tick-lastPreempt < cooldown {
					t.Fatalf("%s t=%d: resume (%s) %ds after a preemption", sc.name, tick, d.Reason, tick-lastPreempt)
				}
				continue
			}
			if spaced++; spaced > 1 || (lastSwitch >= 0 && tick-lastSwitch < cooldown) {
				t.Fatalf("%s t=%d: %s (%s) %ds after the previous switch", sc.name, tick, d.Action, d.Reason, tick-lastSwitch)
			}
			if d.Action == ActionPreempt {
				lastPreempt = tick
			}
		}
		if switched {
			lastSwitch = tick
		}
		for _, d := range decisions {
			i := index[d.Key]
			j := sc.jobs[i]
			switch d.Action {
			case ActionQueue, ActionPreempt:
				if j.owner == "" {
					t.Fatalf("%s t=%d: paused unattributed job %d", sc.name, tick, i)
				}
				if d.Action == ActionQueue && j.pre {
					t.Fatalf("%s t=%d: queued pre-existing job %d", sc.name, tick, i)
				}
				if paused[i] {
					t.Fatalf("%s t=%d: paused job %d twice", sc.name, tick, i)
				}
				paused[i], waitStart[i] = true, tick
				if d.Action == ActionQueue {
					res.queued++
				} else {
					res.preempted++
					preemptedNow = true
				}
			case ActionResume:
				if !paused[i] {
					t.Fatalf("%s t=%d: resumed running job %d", sc.name, tick, i)
				}
				paused[i] = false
				res.resumed++
				w := tick - waitStart[i]
				res.maxWait = max(res.maxWait, w)
				res.ownerMaxWait[j.owner] = max(res.ownerMaxWait[j.owner], w)
				if sc.pressure(waitStart[i]) == domain.PressureNormal && pressure == domain.PressureNormal &&
					w > maxQueueWait+cooldown {
					t.Fatalf("%s: job %d waited %ds under normal pressure, bound %ds", sc.name, i, w, maxQueueWait+cooldown)
				}
			}
		}
		running, exempt, attributed := 0, 0, 0
		for i, j := range sc.jobs {
			if j.arrive <= tick && finish[i] < 0 && !paused[i] {
				running++
				if j.owner == "" || j.pre {
					exempt++
				}
				if j.owner != "" {
					attributed++
				}
			}
		}
		// Exempt jobs are never paused, so only the controlled ones are
		// bound by the limit.
		if preemptedNow && attributed == 0 {
			t.Fatalf("%s t=%d: preemption left no attributed job running", sc.name, tick)
		}
		if running-exempt > limit {
			t.Fatalf("%s t=%d: %d controlled jobs running over limit %d", sc.name, tick, running-exempt, limit)
		}
		res.peak = max(res.peak, running)
		res.peakControlled = max(res.peakControlled, running-exempt)
		// A lowered limit never preempts, but nothing new may start while
		// the controlled jobs exceed it, nor beyond an owner's budget.
		ctl, perOwner := counts(tick)
		if ctl > prevCtl && ctl > lim {
			t.Fatalf("%s t=%d: started a job with %d controlled running over effective limit %d", sc.name, tick, ctl, lim)
		}
		for o, n := range perOwner {
			if budget > 0 && o != "" && n > prevOwner[o] && n > budget {
				t.Fatalf("%s t=%d: owner %s started a job with %d running over budget %d", sc.name, tick, o, n, budget)
			}
		}
		for i, j := range sc.jobs {
			if j.arrive <= tick && finish[i] < 0 && !paused[i] {
				remaining[i]--
				if remaining[i] == 0 {
					finish[i] = tick + 1
					done++
				}
			}
		}
		prevCtl, prevOwner = counts(tick)
	}

	first, work, lastFree := sc.jobs[0].arrive, 0, 0
	ownerWork := map[string]int{}
	for i, j := range sc.jobs {
		first = min(first, j.arrive)
		work += j.dur
		ownerWork[j.owner] += j.dur
		lastFree = max(lastFree, j.arrive+j.dur)
		res.makespan = max(res.makespan, finish[i])
		res.ownerFinish[j.owner] = max(res.ownerFinish[j.owner], finish[i])
	}
	res.makespan -= first
	for o := range res.ownerFinish {
		res.ownerFinish[o] -= first
	}
	res.ideal = max(lastFree-first, (work+limit-1)/limit)
	if budget > 0 {
		// One owner's work cannot be packed tighter than its budget allows.
		for o, w := range ownerWork {
			if o != "" {
				res.ideal = max(res.ideal, (w+budget-1)/budget)
			}
		}
	}
	return res
}

func TestSimulation(t *testing.T) {
	base := DefaultPolicy()
	base.Mode = ModeEnforce

	var overlap []simJob
	overlap = append(overlap, simJob{owner: "agent-0", arrive: 0, dur: 40, pre: true})
	for i := range 12 {
		overlap = append(overlap, simJob{owner: fmt.Sprintf("agent-%d", i%6), arrive: 1 + 5*i, dur: 30 + (i*7)%25})
	}
	overlap = append(overlap, simJob{owner: "", arrive: 20, dur: 60})

	starve := []simJob{
		{owner: "short-a", arrive: 1, dur: 30},
		{owner: "short-b", arrive: 2, dur: 30},
		{owner: "long", arrive: 5, dur: 300},
	}
	for i := range 24 {
		starve = append(starve, simJob{owner: fmt.Sprintf("short-%c", 'a'+i%3), arrive: 6 + 12*i, dur: 20})
	}

	critPolicy := base
	critPolicy.HeavyLimit, critPolicy.PauseOnCritical = 3, true
	var crit []simJob
	for i := range 6 {
		crit = append(crit, simJob{owner: fmt.Sprintf("agent-%d", i%3), arrive: 1 + 10*i, dur: 60})
	}

	// Adaptive: pressure turns high while 12 builds overlap, then eases.
	adaptPolicy := base
	adaptPolicy.HeavyLimit, adaptPolicy.Adaptive = 4, true
	var adapt []simJob
	for i := range 12 {
		adapt = append(adapt, simJob{owner: fmt.Sprintf("agent-%d", i%4), arrive: 1 + 5*i, dur: 40})
	}
	adaptPressure := func(t int) domain.PressureLevel {
		switch {
		case t >= 20 && t < 140:
			return domain.PressureHigh
		case t >= 140 && t < 170:
			return domain.PressureElevated
		}
		return domain.PressureNormal
	}

	// Owner budget: agent-a floods 10 builds; agent-b starts 2 later.
	budgetPolicy := base
	budgetPolicy.HeavyLimit, budgetPolicy.PerOwnerLimit = 4, 2
	var flood []simJob
	for i := range 10 {
		flood = append(flood, simJob{owner: "agent-a", arrive: 1 + i, dur: 30})
	}
	flood = append(flood, simJob{owner: "agent-b", arrive: 15, dur: 30}, simJob{owner: "agent-b", arrive: 16, dur: 30})

	// CPU-only oversubscription, as on the first real-machine benchmark: a
	// burst of 4x CPUs one-core builds (2 minutes each) on 8 CPUs, under the
	// balanced profile: start 2 (CPUs/4), ceiling 8 (the CPU count). Only
	// the CPU is saturated, which holds pressure at elevated; memory, swap
	// and latency stay calm.
	oversubPolicy := base
	oversubPolicy.HeavyLimit, oversubPolicy.StartLimit, oversubPolicy.Adaptive = 8, 2, true
	oversubPolicy.ResumeCooldown = time.Second
	var oversub []simJob
	for i := range 32 {
		oversub = append(oversub, simJob{owner: fmt.Sprintf("agent-%d", i%4), arrive: 1, dur: 120})
	}
	oversubPressure := func(int) domain.PressureLevel { return domain.PressureElevated }
	cpuOnly := func(int) bool { return true }
	memoryElevated := func(int) bool { return false }
	// ideal is the makespan with every build running at once on 8 CPUs.
	const oversubIdeal = 32 * 120 / 8

	// A burst of short builds at the default 10s cooldown, as on the 2-CPU
	// CI runner: 8 jobs of 5s at once, 2 slots. Each exit frees a slot that
	// is refilled the next round, so the cooldown does not serialize them.
	var burst []simJob
	for i := range 8 {
		burst = append(burst, simJob{owner: fmt.Sprintf("agent-%d", i%4), arrive: 1, dur: 5})
	}

	// A burst of short CPU-only builds under the balanced profile on 5
	// CPUs, as on the macOS CI runner: 20 jobs of 5s at once, start 2,
	// ceiling 5, default cooldown and adapt interval. The +1 per 30s alone
	// held 3 slots for the whole burst, 1.8x the baseline makespan; with
	// 50 MiB jobs on 16 GiB the limit may grow to the ceiling at once.
	const gib = 1 << 30
	shortPolicy := base
	shortPolicy.HeavyLimit, shortPolicy.StartLimit, shortPolicy.Adaptive = 5, 2, true
	var short []simJob
	for i := range 20 {
		short = append(short, simJob{owner: fmt.Sprintf("agent-%d", i%4), arrive: 1, dur: 5, rss: 50 << 20})
	}
	// The same burst with jobs of 22% of memory each, while the memory
	// signals still read calm (they lag the working set): the headroom
	// keeps the limit from growing at once.
	var heavyMem []simJob
	for i := range 8 {
		heavyMem = append(heavyMem, simJob{owner: fmt.Sprintf("agent-%d", i%4), arrive: 1, dur: 6, rss: 16 * gib * 22 / 100})
	}

	scenarios := []simScenario{
		{name: "1 agent, 1 build", policy: base, jobs: []simJob{{owner: "agent-0", arrive: 1, dur: 120}}, pressure: normalPressure},
		{name: "6 agents, 12 overlapping builds", policy: base, jobs: overlap, pressure: normalPressure},
		{name: "long job among short jobs", policy: base, jobs: starve, pressure: normalPressure},
		{
			name: "critical, PauseOnCritical", policy: critPolicy, jobs: crit, wantPreempt: true,
			pressure: func(t int) domain.PressureLevel {
				if t >= 25 && t < 70 {
					return domain.PressureCritical
				}
				return domain.PressureNormal
			},
		},
		{
			name: "adaptive, sustained high", policy: adaptPolicy, jobs: adapt, pressure: adaptPressure, noBound: true,
			check: func(t *testing.T, r simResult) {
				if r.limitMin != 1 || r.limitMax != 4 {
					t.Errorf("limit range [%d, %d], want [1, 4]: high must drive it down and recovery back up", r.limitMin, r.limitMax)
				}
			},
		},
		{
			name: "CPU-only oversubscription", policy: oversubPolicy, jobs: oversub,
			pressure: oversubPressure, throughputOnly: cpuOnly, noBound: true,
			check: func(t *testing.T, r simResult) {
				if r.limitEnd != 8 {
					t.Errorf("limit ended at %d, want the ceiling 8", r.limitEnd)
				}
				if bound := oversubIdeal * 130 / 100; r.makespan > bound {
					t.Errorf("makespan %ds over %ds, 1.3x the unthrottled ideal %ds", r.makespan, bound, oversubIdeal)
				}
			},
		},
		{
			name: "same, elevated by memory", policy: oversubPolicy, jobs: oversub,
			pressure: oversubPressure, throughputOnly: memoryElevated, noBound: true,
			check: func(t *testing.T, r simResult) {
				// Memory pressure holds the limit at its start.
				if r.limitEnd != 2 || r.limitMax != 2 {
					t.Errorf("limit ended at %d, want it held at 2", r.limitEnd)
				}
				if r.makespan < 3*oversubIdeal {
					t.Errorf("makespan %ds, want the held limit to cost at least 3x the ideal %ds", r.makespan, oversubIdeal)
				}
			},
		},
		{name: "burst of short builds", policy: base, jobs: burst, pressure: normalPressure},
		{
			name: "same, elevated by CPU", policy: base, jobs: burst,
			pressure: oversubPressure, throughputOnly: cpuOnly,
		},
		{
			name: "same, elevated by memory", policy: base, jobs: burst,
			pressure: oversubPressure, throughputOnly: memoryElevated,
		},
		{
			name: "short CPU-only burst, 5 CPUs", policy: shortPolicy, jobs: short, memory: 16 * gib,
			pressure: oversubPressure, throughputOnly: cpuOnly, noBound: true,
			check: func(t *testing.T, r simResult) {
				// 20 x 5s on 5 CPUs packs into 20s.
				if r.limitEnd != 5 || r.makespan > 22 {
					t.Errorf("limit ended at %d, makespan %ds; want the ceiling 5 within 10%% of 20s", r.limitEnd, r.makespan)
				}
			},
		},
		{
			name: "same, 22% of memory per job", policy: shortPolicy, jobs: heavyMem, memory: 16 * gib,
			pressure: oversubPressure, throughputOnly: cpuOnly, noBound: true,
			check: func(t *testing.T, r simResult) {
				// Only the first calm round's +1; the rest is too short for
				// another interval.
				if r.limitMax != 3 {
					t.Errorf("limit reached %d, want 3: the jobs do not fit in the headroom", r.limitMax)
				}
			},
		},
		{
			name: "owner budget, 1 agent floods", policy: budgetPolicy, jobs: flood, pressure: normalPressure,
			check: func(t *testing.T, r simResult) {
				// agent-b gets its two slots at once, so it finishes in
				// about one job duration although agent-a queued first.
				if w := r.ownerMaxWait["agent-b"]; w > 0 {
					t.Errorf("agent-b waited %ds", w)
				}
				if f := r.ownerFinish["agent-b"]; f > 16+30 {
					t.Errorf("agent-b finished at %ds, starved by agent-a", f)
				}
			},
		},
	}

	var tbl strings.Builder
	fmt.Fprintf(&tbl, "\n%-32s %4s %5s %4s %4s %6s %6s %7s %7s %8s %5s\n",
		"scenario", "jobs", "limit", "peak", "ctl", "queued", "resume", "preempt", "maxwait", "makespan", "ideal")
	for _, sc := range scenarios {
		r := runSim(t, sc)
		lim := fmt.Sprint(r.limitMax)
		if r.limitMin != r.limitMax {
			lim = fmt.Sprintf("%d-%d", r.limitMin, r.limitMax)
		}
		fmt.Fprintf(&tbl, "%-32s %4d %5s %4d %4d %6d %6d %7d %6ds %7ds %4ds\n", sc.name, len(sc.jobs),
			lim, r.peak, r.peakControlled, r.queued, r.resumed, r.preempted, r.maxWait, r.makespan, r.ideal)
		if !sc.policy.Adaptive && r.limitChanges != 0 {
			t.Errorf("%s: %d limit changes without Adaptive", sc.name, r.limitChanges)
		}
		if sc.check != nil {
			sc.check(t, r)
		}
		if r.resumed != r.queued+r.preempted {
			t.Errorf("%s: %d resumes for %d pauses", sc.name, r.resumed, r.queued+r.preempted)
		}
		if sc.wantPreempt {
			if r.preempted == 0 {
				t.Errorf("%s: no preemption", sc.name)
			}
			continue // pressure, not the slot limit, sets the makespan
		}
		if sc.noBound {
			continue
		}
		// Within 15% (plus one cooldown) of a perfect packing at the limit.
		if bound := r.ideal*115/100 + 10; r.makespan > bound {
			t.Errorf("%s: makespan %ds over bound %ds (ideal %ds)", sc.name, r.makespan, bound, r.ideal)
		}
		if sc.name == "1 agent, 1 build" && (r.queued != 0 || r.makespan != r.ideal) {
			t.Errorf("%s: scheduler got in the way: %+v", sc.name, r)
		}
	}
	t.Log(tbl.String())
}

// lineItem is one arrival in a line simulation: an agent's heavy job, which
// the scheduler may queue, or a `throt run` caller (lease), which polls for
// an admission lease once per tick and starts its job a tick after the
// grant. With loop set, the item arrives again the tick after it finishes,
// until the horizon, as an agent working back to back would.
type lineItem struct {
	name   string
	lease  bool
	owner  string
	arrive int
	dur    int
	loop   bool
}

type lineResult struct {
	// order lists item names in the order they started running.
	order []string
	// maxWait is the longest wait per kind ("job" or "lease").
	maxWait map[string]int
	// served counts started arrivals per kind.
	served map[string]int
}

// runLineSim drives the scheduler and an emulation of the daemon's acquire
// with a 1s tick until every arrival before horizon has finished. It checks
// that running work plus reserved slots never exceed the limit.
func runLineSim(t *testing.T, policy Policy, items []lineItem, horizon int) lineResult {
	t.Helper()
	type inst struct {
		it        lineItem
		key       domain.ProcessKey
		arrive    int
		waitStart int
		remaining int
		present   bool // a workload in the snapshot
		paused    bool
		waiting   bool // a lease caller not granted yet
		reserved  bool // granted; its job appears next tick
		started   bool
		done      bool
	}
	s := New(policy)
	p := s.Policy()
	res := lineResult{maxWait: map[string]int{}, served: map[string]int{}}
	var all []*inst
	nextPID := 1000
	spawn := func(it lineItem, tick int) {
		nextPID++
		in := &inst{it: it, key: domain.ProcessKey{PID: nextPID, Start: t0}, arrive: tick, waitStart: tick, remaining: it.dur}
		if it.lease {
			in.waiting = true
		} else {
			in.present = true
		}
		all = append(all, in)
	}
	kind := func(in *inst) string {
		if in.it.lease {
			return "lease"
		}
		return "job"
	}
	start := func(in *inst, tick int) {
		in.started = true
		res.order = append(res.order, in.it.name)
		res.served[kind(in)]++
		res.maxWait[kind(in)] = max(res.maxWait[kind(in)], tick-in.waitStart)
	}
	pending := slices.Clone(items)
	for tick := 0; ; tick++ {
		if tick > 100000 {
			t.Fatal("line simulation did not finish")
		}
		for i := 0; i < len(pending); {
			if pending[i].arrive == tick {
				spawn(pending[i], tick)
				pending = slices.Delete(pending, i, i+1)
				continue
			}
			i++
		}
		in := Input{Now: at(tick), Pressure: domain.PressureNormal}
		for _, x := range all {
			switch {
			case x.done:
			case x.reserved:
				// The leased command shows up as its workload and takes
				// over the lease.
				x.reserved, x.present = false, true
				in.Admitted = append(in.Admitted, x.key)
				start(x, tick)
			case x.waiting:
				in.Waiters = append(in.Waiters, Waiter{Key: x.key, Owner: x.it.owner, Since: at(x.waitStart), Lease: true})
			}
			if x.present && !x.done {
				in.Heavy = append(in.Heavy, process.WorkloadView{Key: x.key, Name: x.it.name, Class: domain.ClassBuild, Owner: x.it.owner, CPU: 1})
				if x.paused {
					in.Paused = append(in.Paused, x.key)
				}
			}
		}
		byKey := map[domain.ProcessKey]*inst{}
		for _, x := range all {
			byKey[x.key] = x
		}
		for _, d := range s.Decide(in) {
			x := byKey[d.Key]
			switch d.Action {
			case ActionQueue, ActionPreempt:
				x.paused, x.waitStart = true, tick
			case ActionResume:
				x.paused = false
				start(x, tick)
			}
		}
		// New jobs the scheduler admitted at once start now.
		for _, x := range all {
			if x.present && !x.paused && !x.started && !x.done {
				start(x, tick)
			}
		}
		// Between ticks every waiting caller polls once, newest first, so
		// the outcome cannot depend on polling order.
		busy := func() (int, map[string]int) {
			n, per := 0, map[string]int{}
			for _, x := range all {
				if !x.done && ((x.present && !x.paused) || x.reserved) {
					n++
					per[x.it.owner]++
				}
			}
			return n, per
		}
		for i := len(all) - 1; i >= 0; i-- {
			x := all[i]
			if !x.waiting {
				continue
			}
			n, per := busy()
			if n >= p.HeavyLimit || (p.PerOwnerLimit > 0 && per[x.it.owner] >= p.PerOwnerLimit) {
				continue
			}
			line := s.Queued()
			for _, y := range all {
				if y.waiting {
					line = append(line, Waiter{Key: y.key, Owner: y.it.owner, Since: at(y.waitStart), Lease: true})
				}
			}
			me := Waiter{Key: x.key, Owner: x.it.owner, Since: at(x.waitStart), Lease: true}
			if p.FirstInLine(at(tick), me, line, per) {
				x.waiting, x.reserved = false, true
			}
		}
		if n, _ := busy(); n > p.HeavyLimit {
			t.Fatalf("t=%d: %d running or reserved over limit %d", tick, n, p.HeavyLimit)
		}
		// Work progresses; looping items come back the next tick.
		for _, x := range all {
			if x.present && !x.paused && !x.done {
				x.remaining--
				if x.remaining == 0 {
					x.done = true
					if x.it.loop && tick+1 < horizon {
						next := x.it
						next.arrive = tick + 1
						pending = append(pending, next)
					}
				}
			}
		}
		if len(pending) == 0 && !slices.ContainsFunc(all, func(x *inst) bool { return !x.done }) {
			return res
		}
	}
}

func TestLineFIFOAcrossKinds(t *testing.T) {
	p := enforce(1)
	p.ResumeCooldown = 0
	// One slot, held by x until t=10. Queued jobs and lease callers arrive
	// alternately behind it and must start in arrival order.
	items := []lineItem{
		{name: "x", owner: "a", arrive: 1, dur: 9},
		{name: "job1", owner: "b", arrive: 2, dur: 3},
		{name: "lease1", lease: true, owner: "c", arrive: 3, dur: 3},
		{name: "job2", owner: "d", arrive: 4, dur: 3},
		{name: "lease2", lease: true, owner: "e", arrive: 5, dur: 3},
		{name: "job3", owner: "b", arrive: 6, dur: 3},
	}
	r := runLineSim(t, p, items, 0)
	want := []string{"x", "job1", "lease1", "job2", "lease2", "job3"}
	if !slices.Equal(r.order, want) {
		t.Fatalf("start order %v, want %v", r.order, want)
	}
}

func TestLineNoStarvation(t *testing.T) {
	// Three agents work back to back on two slots, so the scheduler's queue
	// is never empty; throt run callers arrive every 7s. Before the shared
	// line, every caller was refused while anything was queued.
	p := enforce(2)
	p.ResumeCooldown = 2 * time.Second
	p.MaxQueueWait = 60 * time.Second
	const horizon = 900
	var items []lineItem
	for i := range 3 {
		items = append(items, lineItem{name: fmt.Sprintf("agent-%d", i), owner: fmt.Sprintf("agent-%d", i), arrive: 1 + i, dur: 10, loop: true})
	}
	for i := range horizon / 7 {
		items = append(items, lineItem{name: fmt.Sprintf("run-%d", i), lease: true, owner: fmt.Sprintf("agent-%d", i%3), arrive: 5 + 7*i, dur: 4})
	}
	r := runLineSim(t, p, items, horizon)
	bound := int((p.MaxQueueWait + p.ResumeCooldown) / time.Second)
	t.Logf("served %v, max wait %v (bound %ds)", r.served, r.maxWait, bound)
	if r.served["lease"] != horizon/7 {
		t.Errorf("%d of %d lease callers served", r.served["lease"], horizon/7)
	}
	for k, w := range r.maxWait {
		if w > bound {
			t.Errorf("%s waited up to %ds, bound %ds", k, w, bound)
		}
	}
}

// cooldownExemptResume reports whether d is a resume the cooldown does not
// space: into a free slot at normal or throughput-only pressure, or into a
// slot an exit freed below high pressure. Every other resume and every
// preemption is spaced, so a round has at most one of those.
func cooldownExemptResume(d Decision, pressure domain.PressureLevel, throughputOnly bool) bool {
	if d.Action != ActionResume {
		return false
	}
	switch d.Reason {
	case ReasonPressureNormal:
		return pressure == domain.PressureNormal
	case ReasonPressureThroughput:
		return pressure == domain.PressureElevated && throughputOnly
	case ReasonWorkloadExited:
		return pressure != domain.PressureHigh && pressure != domain.PressureCritical
	}
	return false
}
