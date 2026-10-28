package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// FuzzDecideInvariants drives a scheduler with a random sequence of
// workloads appearing and exiting, manual holds, pressure levels and clock
// steps (including backwards), executing every decision as the daemon
// does. After each round it checks the invariants every caller relies on;
// at the end it lets running work finish under normal pressure and checks
// that the queue never loses a workload.
func FuzzDecideInvariants(f *testing.F) {
	f.Add(uint8(2), uint8(1), true, true, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
	f.Add(uint8(1), uint8(0), false, true, []byte{0, 0, 0, 0x34, 0x34, 0x34, 5, 5, 0x44, 0x44, 9})
	f.Add(uint8(4), uint8(2), true, false, []byte{0, 0, 0, 0, 0x14, 0x24, 0x34, 0x44, 0x07, 0x17, 0x27})
	// High pressure queues three arrivals; at normal pressure they refill
	// free slots together, which the cooldown does not space.
	f.Add(uint8(4), uint8(0), false, false, []byte{0x34, 0x11, 0x21, 0x31, 0x14, 0x25})
	f.Fuzz(func(t *testing.T, ceiling, perOwner uint8, adaptiveOn, pauseOnCritical bool, ops []byte) {
		ops = ops[:min(len(ops), 256)] // rounds are O(workloads); keep each input fast
		p := DefaultPolicy()
		p.Mode, p.HeavyLimit, p.PerOwnerLimit = ModeEnforce, int(ceiling%8), int(perOwner%4)
		p.Adaptive, p.StartLimit, p.PauseOnCritical = adaptiveOn, int(perOwner%3), pauseOnCritical
		p.ResumeCooldown, p.MaxQueueWait = 2*time.Second, 20*time.Second
		s := New(p)
		ceil := s.Policy().HeavyLimit

		owners := [...]string{"", "a", "b", "c"}
		levels := [...]domain.PressureLevel{domain.PressureUnknown, domain.PressureNormal, domain.PressureElevated, domain.PressureHigh, domain.PressureCritical}
		var live []process.WorkloadView
		paused := map[domain.ProcessKey]bool{}
		held := map[domain.ProcessKey]bool{}
		now, pid := t0, 100
		pressure := domain.PressureNormal
		throughputOnly := false
		var exited []domain.ProcessKey // exits since the last round

		round := func(level domain.PressureLevel) []Decision {
			var ps, hs []domain.ProcessKey
			for _, v := range live {
				if paused[v.Key] {
					ps = append(ps, v.Key)
				}
				if held[v.Key] {
					hs = append(hs, v.Key)
				}
			}
			ds := s.Decide(Input{Now: now, Pressure: level, ThroughputOnly: throughputOnly && level == domain.PressureElevated,
				Heavy: slices.Clone(live), Paused: ps, Held: hs, Exited: exited})
			exited = nil
			if l := s.Limit(); l < 1 || l > ceil {
				t.Fatalf("limit %d outside [1, %d]", l, ceil)
			}
			spaced, preempts := 0, 0
			for _, d := range ds {
				if held[d.Key] {
					t.Fatalf("decision %+v on a held workload", d)
				}
				switch d.Action {
				case ActionResume:
					if !cooldownExemptResume(d, level, throughputOnly && level == domain.PressureElevated) {
						spaced++
					}
					if !paused[d.Key] {
						t.Fatalf("resume of %d, which is not paused", d.Key.PID)
					}
					delete(paused, d.Key)
				case ActionQueue, ActionPreempt:
					if d.Owner == "" {
						t.Fatalf("%s of unattributed %d", d.Action, d.Key.PID)
					}
					if paused[d.Key] {
						t.Fatalf("%s of %d, which is already paused", d.Action, d.Key.PID)
					}
					if d.Action == ActionPreempt {
						preempts++
						spaced++
						if level != domain.PressureCritical || !s.Policy().PauseOnCritical {
							t.Fatalf("preempt at %v", level)
						}
					}
					paused[d.Key] = true
				}
			}
			if spaced > 1 || preempts > 1 {
				t.Fatalf("%d spaced switches and %d preemptions in one round", spaced, preempts)
			}
			return ds
		}

		for _, op := range ops {
			switch arg := int(op >> 4); op % 8 {
			case 0, 1: // a workload appears
				pid++
				live = append(live, process.WorkloadView{Key: domain.ProcessKey{PID: pid, Start: t0}, Name: "go", Class: domain.ClassBuild, Owner: owners[arg%len(owners)]})
			case 2: // one exits
				if len(live) > 0 {
					i := arg % len(live)
					exited = append(exited, live[i].Key)
					delete(paused, live[i].Key)
					delete(held, live[i].Key)
					live = slices.Delete(live, i, i+1)
				}
			case 3: // a manual hold is placed or lifted
				if len(live) > 0 {
					k := live[arg%len(live)].Key
					if !paused[k] {
						held[k] = !held[k]
					}
				}
			case 4:
				pressure = levels[arg%len(levels)]
			case 5:
				now = now.Add(time.Duration(arg+1) * time.Second)
			case 6:
				now = now.Add(-time.Duration(arg+1) * time.Second)
			case 7:
				throughputOnly = !throughputOnly
			}
			round(pressure)
		}

		// Running work finishes; every queued workload must get its turn.
		for _, v := range live {
			if !paused[v.Key] {
				delete(held, v.Key)
			}
		}
		live = slices.DeleteFunc(live, func(v process.WorkloadView) bool { return !paused[v.Key] && !held[v.Key] })
		for range 4 * (len(live) + 2) {
			now = now.Add(time.Minute)
			for _, d := range round(domain.PressureNormal) {
				if d.Action == ActionResume {
					live = slices.DeleteFunc(live, func(v process.WorkloadView) bool { return v.Key == d.Key })
				}
			}
		}
		for _, v := range live {
			if paused[v.Key] {
				t.Fatalf("queued workload %d was never resumed", v.Key.PID)
			}
		}
	})
}
