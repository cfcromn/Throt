package scheduler

import (
	"fmt"
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
}

type simScenario struct {
	name     string
	policy   Policy
	jobs     []simJob
	pressure func(t int) domain.PressureLevel
	// wantPreempt requires at least one preemption.
	wantPreempt bool
}

type simResult struct {
	peak, peakControlled       int
	queued, resumed, preempted int
	maxWait, makespan, ideal   int
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
	cooldown := int(s.Policy().ResumeCooldown / time.Second)
	maxQueueWait := int(s.Policy().MaxQueueWait / time.Second)
	var res simResult
	done := 0
	tick := 0
	for ; done < n; tick++ {
		if tick > maxTicks {
			t.Fatalf("%s: %d of %d jobs never completed", sc.name, n-done, n)
		}
		pressure := sc.pressure(tick)
		in := Input{Now: at(tick), Pressure: pressure}
		for i, j := range sc.jobs {
			if j.arrive <= tick && finish[i] < 0 {
				in.Heavy = append(in.Heavy, process.WorkloadView{Key: keyOf(i), Name: "build", Class: domain.ClassBuild, Owner: j.owner})
				if paused[i] {
					in.Paused = append(in.Paused, keyOf(i))
				}
			}
		}
		for _, d := range s.Decide(in) {
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
				}
			case ActionResume:
				if !paused[i] {
					t.Fatalf("%s t=%d: resumed running job %d", sc.name, tick, i)
				}
				paused[i] = false
				res.resumed++
				w := tick - waitStart[i]
				res.maxWait = max(res.maxWait, w)
				if sc.pressure(waitStart[i]) == domain.PressureNormal && pressure == domain.PressureNormal &&
					w > maxQueueWait+cooldown {
					t.Fatalf("%s: job %d waited %ds under normal pressure, bound %ds", sc.name, i, w, maxQueueWait+cooldown)
				}
			}
		}
		running, exempt := 0, 0
		for i, j := range sc.jobs {
			if j.arrive <= tick && finish[i] < 0 && !paused[i] {
				running++
				if j.owner == "" || j.pre {
					exempt++
				}
			}
		}
		// Exempt jobs are never paused, so only the controlled ones are
		// bound by the limit.
		if running-exempt > limit {
			t.Fatalf("%s t=%d: %d controlled jobs running over limit %d", sc.name, tick, running-exempt, limit)
		}
		res.peak = max(res.peak, running)
		res.peakControlled = max(res.peakControlled, running-exempt)
		for i, j := range sc.jobs {
			if j.arrive <= tick && finish[i] < 0 && !paused[i] {
				remaining[i]--
				if remaining[i] == 0 {
					finish[i] = tick + 1
					done++
				}
			}
		}
	}

	first, work, lastFree := sc.jobs[0].arrive, 0, 0
	for i, j := range sc.jobs {
		first = min(first, j.arrive)
		work += j.dur
		lastFree = max(lastFree, j.arrive+j.dur)
		res.makespan = max(res.makespan, finish[i])
	}
	res.makespan -= first
	res.ideal = max(lastFree-first, (work+limit-1)/limit)
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
	}

	var tbl strings.Builder
	fmt.Fprintf(&tbl, "\n%-32s %4s %5s %4s %4s %6s %6s %7s %7s %8s %5s\n",
		"scenario", "jobs", "limit", "peak", "ctl", "queued", "resume", "preempt", "maxwait", "makespan", "ideal")
	for _, sc := range scenarios {
		r := runSim(t, sc)
		fmt.Fprintf(&tbl, "%-32s %4d %5d %4d %4d %6d %6d %7d %6ds %7ds %4ds\n", sc.name, len(sc.jobs),
			New(sc.policy).Policy().HeavyLimit, r.peak, r.peakControlled, r.queued, r.resumed, r.preempted, r.maxWait, r.makespan, r.ideal)
		if r.resumed != r.queued+r.preempted {
			t.Errorf("%s: %d resumes for %d pauses", sc.name, r.resumed, r.queued+r.preempted)
		}
		if sc.wantPreempt {
			if r.preempted == 0 {
				t.Errorf("%s: no preemption", sc.name)
			}
			continue // pressure, not the slot limit, sets the makespan
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
