package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func key(pid int) domain.ProcessKey { return domain.ProcessKey{PID: pid, Start: t0} }

func wv(pid int, owner string) process.WorkloadView {
	return process.WorkloadView{Key: key(pid), Name: "go", Class: domain.ClassBuild, Owner: owner}
}

func keys(pids ...int) []domain.ProcessKey {
	var ks []domain.ProcessKey
	for _, p := range pids {
		ks = append(ks, key(p))
	}
	return ks
}

func enforce(limit int) Policy {
	p := DefaultPolicy()
	p.Mode, p.HeavyLimit = ModeEnforce, limit
	return p
}

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// brief is a compact view of a decision for comparisons.
type brief struct {
	a      Action
	pid    int
	reason string
}

func briefs(ds []Decision) []brief {
	var out []brief
	for _, d := range ds {
		out = append(out, brief{d.Action, d.Key.PID, d.Reason})
	}
	return out
}

func expect(t *testing.T, got []Decision, want ...brief) {
	t.Helper()
	if g := briefs(got); !slices.Equal(g, want) {
		t.Fatalf("decisions = %v, want %v", g, want)
	}
}

func TestDefaultPolicyAndNormalize(t *testing.T) {
	d := DefaultPolicy()
	if d.Mode != ModeOff || d.HeavyLimit != 2 || d.ResumeCooldown != 10*time.Second ||
		d.PauseOnCritical || d.MaxQueueWait != 5*time.Minute {
		t.Fatalf("DefaultPolicy = %+v", d)
	}
	got := New(Policy{Mode: "bogus", HeavyLimit: 0, ResumeCooldown: -time.Second}).Policy()
	want := Policy{Mode: ModeOff, HeavyLimit: 1, ResumeCooldown: 0, MaxQueueWait: DefaultMaxQueueWait}
	if got != want {
		t.Fatalf("normalized = %+v, want %+v", got, want)
	}
	if New(Policy{Mode: ModeObserve}).Policy().Mode != ModeObserve {
		t.Fatal("observe mode not kept")
	}
}

func TestOffModeReturnsNilAndKeepsNoState(t *testing.T) {
	s := New(enforce(1))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(10, "a")}})
	s.SetPolicy(DefaultPolicy())
	heavy := []process.WorkloadView{wv(10, "a"), wv(11, "a")}
	if d := s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy}); d != nil {
		t.Fatalf("off mode decided %v", d)
	}
	if len(s.seen) != 0 {
		t.Fatalf("off mode kept %d entries", len(s.seen))
	}
	// Re-enabled: everything present is adopted, nothing is retroactively queued.
	s.SetPolicy(enforce(1))
	expect(t, s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy}))
}

func TestNoRetroactiveQueuing(t *testing.T) {
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "a"), wv(3, "b"), wv(4, "c")}
	s := New(enforce(1))
	expect(t, s.Decide(Input{Now: at(0), Pressure: domain.PressureCritical, Heavy: heavy}))
	expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy}))

	// A policy change re-baselines: 5 arrived together with the change.
	s.SetPolicy(enforce(1))
	heavy = append(heavy, wv(5, "a"))
	expect(t, s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy}))
	// A workload appearing after the baseline is new.
	heavy = append(heavy, wv(6, "a"))
	expect(t, s.Decide(Input{Now: at(3), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 6, ReasonSlotsExhausted})
}

func TestNewWorkloadByPressure(t *testing.T) {
	tests := []struct {
		name     string
		pressure domain.PressureLevel
		running  int
		want     []brief
	}{
		{"normal free slot", domain.PressureNormal, 1, nil},
		{"normal full", domain.PressureNormal, 2, []brief{{ActionQueue, 100, ReasonSlotsExhausted}}},
		{"normal over limit", domain.PressureNormal, 3, []brief{{ActionQueue, 100, ReasonSlotsExhausted}}},
		{"elevated free slot", domain.PressureElevated, 0, nil},
		{"elevated full", domain.PressureElevated, 2, []brief{{ActionQueue, 100, ReasonSlotsExhausted}}},
		{"high free slot", domain.PressureHigh, 0, []brief{{ActionQueue, 100, ReasonPressureHigh}}},
		{"high full", domain.PressureHigh, 2, []brief{{ActionQueue, 100, ReasonPressureHigh}}},
		{"critical free slot", domain.PressureCritical, 0, []brief{{ActionQueue, 100, ReasonPressureCritical}}},
		{"unknown full fails open", domain.PressureUnknown, 5, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var heavy []process.WorkloadView
			for i := range tt.running {
				heavy = append(heavy, wv(1+i, "a"))
			}
			s := New(enforce(2))
			s.Decide(Input{Now: at(0), Pressure: tt.pressure, Heavy: heavy})
			heavy = append(heavy, wv(100, "b"))
			got := s.Decide(Input{Now: at(1), Pressure: tt.pressure, Heavy: heavy})
			expect(t, got, tt.want...)
			for _, d := range got {
				if d.Pressure != tt.pressure || d.RunningHeavy != tt.running || d.HeavyLimit != 2 || d.Observed {
					t.Fatalf("decision facts = %+v", d)
				}
				if d.Name != "go" || d.Owner != "b" || d.Key != key(100) {
					t.Fatalf("decision target = %+v", d)
				}
			}
		})
	}
}

func TestUnattributedNeverQueuedButCounted(t *testing.T) {
	s := New(enforce(2))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal})
	// Two unattributed arrive: they are never queued, even over the limit.
	heavy := []process.WorkloadView{wv(1, ""), wv(2, ""), wv(3, "")}
	expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureHigh, Heavy: heavy}))
	// They hold the slots, so an attributed newcomer waits.
	heavy = append(heavy, wv(4, "a"))
	got := s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy})
	expect(t, got, brief{ActionQueue, 4, ReasonSlotsExhausted})
	if got[0].RunningHeavy != 3 {
		t.Fatalf("RunningHeavy = %d, want 3", got[0].RunningHeavy)
	}
}

func TestSeveralNewInOneRound(t *testing.T) {
	s := New(enforce(2))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
	heavy := []process.WorkloadView{wv(1, "a"), wv(30, "c"), wv(20, "b"), wv(10, "b")}
	got := s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy})
	// Lowest PID takes the free slot; the rest queue in key order.
	expect(t, got, brief{ActionQueue, 20, ReasonSlotsExhausted}, brief{ActionQueue, 30, ReasonSlotsExhausted})
	if got[0].RunningHeavy != 2 {
		t.Fatalf("RunningHeavy = %d, want 2", got[0].RunningHeavy)
	}
}

func TestNewcomerDoesNotOvertakeQueue(t *testing.T) {
	s := New(enforce(1))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
	expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 2, ReasonSlotsExhausted})
	// 1 finishes and 2 is resumed.
	heavy = []process.WorkloadView{wv(2, "b")}
	expect(t, s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2)}),
		brief{ActionResume, 2, ReasonSlotAvailable})
	// 2 finishes quickly; 3 is queued, then 4 arrives during the cooldown
	// while a slot is free: it must wait behind 3.
	heavy = []process.WorkloadView{wv(3, "c")}
	expect(t, s.Decide(Input{Now: at(3), Pressure: domain.PressureNormal, Heavy: heavy}))
	heavy = []process.WorkloadView{wv(5, "c")}
	s.Decide(Input{Now: at(4), Pressure: domain.PressureNormal, Heavy: heavy})
	heavy = []process.WorkloadView{wv(5, "c"), wv(6, "d")}
	expect(t, s.Decide(Input{Now: at(5), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 6, ReasonSlotsExhausted})
	heavy = []process.WorkloadView{wv(6, "d"), wv(7, "e")}
	expect(t, s.Decide(Input{Now: at(6), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(6)}),
		brief{ActionQueue, 7, ReasonQueueWaiting})
}

func TestResumeConditions(t *testing.T) {
	tests := []struct {
		name     string
		pressure domain.PressureLevel
		running  int
		sinceRes time.Duration // since the previous resume
		want     []brief
	}{
		{"normal free", domain.PressureNormal, 1, time.Minute, []brief{{ActionResume, 100, ReasonSlotAvailable}}},
		{"elevated free", domain.PressureElevated, 0, time.Minute, []brief{{ActionResume, 100, ReasonSlotAvailable}}},
		{"unknown free fails open", domain.PressureUnknown, 1, time.Minute, []brief{{ActionResume, 100, ReasonSlotAvailable}}},
		{"normal full", domain.PressureNormal, 2, time.Minute, nil},
		{"unknown full not aged", domain.PressureUnknown, 2, time.Minute, nil},
		{"high free", domain.PressureHigh, 0, time.Minute, nil},
		{"critical free", domain.PressureCritical, 0, time.Minute, nil},
		{"cooldown pending", domain.PressureNormal, 0, 9 * time.Second, nil},
		{"cooldown just elapsed", domain.PressureNormal, 0, 10 * time.Second, []brief{{ActionResume, 100, ReasonSlotAvailable}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(enforce(2))
			s.baselined, s.lastSwitch = true, at(20)
			var heavy []process.WorkloadView
			for i := range tt.running {
				heavy = append(heavy, wv(1+i, "a"))
				s.seen[key(1+i)] = &entry{firstSeen: t0}
			}
			s.seen[key(100)] = &entry{firstSeen: at(1), queuedSince: at(1)}
			now := at(20).Add(tt.sinceRes)
			heavy = append(heavy, wv(100, "b"))
			// Within MaxQueueWait, so not aged.
			got := s.Decide(Input{Now: now, Pressure: tt.pressure, Heavy: heavy, Paused: keys(100)})
			expect(t, got, tt.want...)
			for _, d := range got {
				if d.RunningHeavy != tt.running+1 {
					t.Fatalf("RunningHeavy = %d, want %d", d.RunningHeavy, tt.running+1)
				}
			}
		})
	}
}

// qw is one queued workload with a chosen wait start and preemption count.
type qw struct {
	pid         int
	owner       string
	since       time.Time
	preemptions int
}

func TestResumePriority(t *testing.T) {
	now := at(1000)
	tests := []struct {
		name    string
		running []process.WorkloadView
		queued  []qw
		want    brief
	}{
		{
			"aged outranks fairness",
			[]process.WorkloadView{wv(1, "a")},
			[]qw{{10, "a", now.Add(-5 * time.Minute), 3}, {11, "b", now.Add(-time.Second), 0}},
			brief{ActionResume, 10, ReasonAged},
		},
		{
			"owner with fewest running wins",
			[]process.WorkloadView{wv(1, "a")},
			[]qw{{10, "a", now.Add(-4 * time.Minute), 0}, {11, "b", now.Add(-time.Second), 5}},
			brief{ActionResume, 11, ReasonSlotAvailable},
		},
		{
			"fewer preemptions wins",
			nil,
			[]qw{{10, "a", now.Add(-4 * time.Minute), 2}, {11, "b", now.Add(-time.Second), 1}},
			brief{ActionResume, 11, ReasonSlotAvailable},
		},
		{
			"longest wait wins",
			nil,
			[]qw{{10, "a", now.Add(-time.Minute), 0}, {11, "b", now.Add(-2 * time.Minute), 0}},
			brief{ActionResume, 11, ReasonSlotAvailable},
		},
		{
			"lower PID breaks ties",
			nil,
			[]qw{{11, "a", now.Add(-time.Minute), 0}, {10, "b", now.Add(-time.Minute), 0}},
			brief{ActionResume, 10, ReasonSlotAvailable},
		},
		{
			"both aged, then fairness",
			[]process.WorkloadView{wv(1, "a")},
			[]qw{{10, "a", now.Add(-time.Hour), 0}, {11, "b", now.Add(-6 * time.Minute), 0}},
			brief{ActionResume, 11, ReasonAged},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(enforce(2))
			heavy := slices.Clone(tt.running)
			var paused []domain.ProcessKey
			for _, q := range tt.queued {
				heavy = append(heavy, wv(q.pid, q.owner))
				paused = append(paused, key(q.pid))
				s.seen[key(q.pid)] = &entry{firstSeen: q.since, queuedSince: q.since, preemptions: q.preemptions}
			}
			for _, r := range tt.running {
				s.seen[r.Key] = &entry{firstSeen: t0}
			}
			s.baselined = true
			expect(t, s.Decide(Input{Now: now, Pressure: domain.PressureNormal, Heavy: heavy, Paused: paused}), tt.want)
		})
	}
}

func TestUnknownPressureResumesAgedRegardlessOfSlots(t *testing.T) {
	s := New(enforce(1))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
	expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 2, ReasonSlotsExhausted})
	expect(t, s.Decide(Input{Now: at(200), Pressure: domain.PressureUnknown, Heavy: heavy, Paused: keys(2)}))
	// Under normal pressure an aged workload still respects the limit.
	expect(t, s.Decide(Input{Now: at(400), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2)}))
	got := s.Decide(Input{Now: at(401), Pressure: domain.PressureUnknown, Heavy: heavy, Paused: keys(2)})
	expect(t, got, brief{ActionResume, 2, ReasonAged})
	if got[0].RunningHeavy != 2 {
		t.Fatalf("RunningHeavy = %d, want 2", got[0].RunningHeavy)
	}
}

func TestResumeAtMostOnePerRound(t *testing.T) {
	s := New(enforce(4))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal})
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b"), wv(3, "c")}
	got := s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(1, 2, 3)})
	expect(t, got, brief{ActionResume, 1, ReasonSlotAvailable})
}

func TestPreemptUnderCritical(t *testing.T) {
	crit := enforce(4)
	crit.PauseOnCritical = true
	t.Run("youngest first, then higher PID, with cooldown and floor", func(t *testing.T) {
		s := New(crit)
		s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
		heavy := []process.WorkloadView{wv(1, "a"), wv(5, "b"), wv(4, "c"), wv(9, "")}
		expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy}))
		got := s.Decide(Input{Now: at(2), Pressure: domain.PressureCritical, Heavy: heavy})
		expect(t, got, brief{ActionPreempt, 5, ReasonPressureCritical})
		if got[0].RunningHeavy != 4 {
			t.Fatalf("RunningHeavy = %d, want 4", got[0].RunningHeavy)
		}
		// Two preemptions are at least one cooldown apart.
		expect(t, s.Decide(Input{Now: at(11), Pressure: domain.PressureCritical, Heavy: heavy, Paused: keys(5)}))
		expect(t, s.Decide(Input{Now: at(12), Pressure: domain.PressureCritical, Heavy: heavy, Paused: keys(5)}),
			brief{ActionPreempt, 4, ReasonPressureCritical})
		// 1 is the last attributed workload running (9 is unattributed):
		// it is never preempted.
		expect(t, s.Decide(Input{Now: at(30), Pressure: domain.PressureCritical, Heavy: heavy, Paused: keys(4, 5)}))
		if s.seen[key(5)].preemptions != 1 || s.seen[key(4)].preemptions != 1 || s.seen[key(1)].preemptions != 0 {
			t.Fatal("preemptions not counted")
		}
	})
	t.Run("preempt and resume share the cooldown", func(t *testing.T) {
		s := New(crit)
		heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b"), wv(3, "c")}
		s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: heavy})
		expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureCritical, Heavy: heavy}),
			brief{ActionPreempt, 3, ReasonPressureCritical})
		// Preempt then resume: the resume waits for the cooldown.
		expect(t, s.Decide(Input{Now: at(10), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(3)}))
		expect(t, s.Decide(Input{Now: at(11), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(3)}),
			brief{ActionResume, 3, ReasonSlotAvailable})
		// Resume then preempt: the preempt waits for the cooldown.
		expect(t, s.Decide(Input{Now: at(20), Pressure: domain.PressureCritical, Heavy: heavy}))
		expect(t, s.Decide(Input{Now: at(21), Pressure: domain.PressureCritical, Heavy: heavy}),
			brief{ActionPreempt, 3, ReasonPressureCritical})
	})
	t.Run("new workloads queue and the last running one is kept", func(t *testing.T) {
		s := New(crit)
		s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
		heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
		expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureCritical, Heavy: heavy}),
			brief{ActionQueue, 2, ReasonPressureCritical})
	})
	t.Run("disabled by default", func(t *testing.T) {
		s := New(enforce(4))
		heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
		s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: heavy})
		expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureCritical, Heavy: heavy}))
	})
	t.Run("unknown pressure preempts nothing", func(t *testing.T) {
		s := New(crit)
		heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
		s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: heavy})
		expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureUnknown, Heavy: heavy}))
	})
	t.Run("unexecuted preempt is not counted", func(t *testing.T) {
		s := New(crit)
		heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
		s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: heavy})
		expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureCritical, Heavy: heavy}),
			brief{ActionPreempt, 2, ReasonPressureCritical})
		s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy})
		if e := s.seen[key(2)]; e.preemptions != 0 || !e.queuedSince.IsZero() || e.preemptPending {
			t.Fatalf("entry = %+v", e)
		}
	})
}

func TestHeldWorkloadsAreNeverTouched(t *testing.T) {
	crit := enforce(2)
	crit.PauseOnCritical = true
	s := New(crit)
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a"), wv(4, "d")}, Held: keys(1)})
	// 1 is held, so it does not take a slot: 2 runs. 3 is held on arrival
	// and is never queued.
	heavy := []process.WorkloadView{wv(1, "a"), wv(4, "d"), wv(2, "b"), wv(3, "c")}
	got := s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy, Held: keys(1, 3)})
	expect(t, got)
	// Under critical pressure 1 is never a candidate, even if it is also
	// (wrongly) listed as scheduler-paused, and does not count toward the
	// one workload kept running.
	got = s.Decide(Input{Now: at(2), Pressure: domain.PressureCritical, Heavy: heavy, Held: keys(1, 3), Paused: keys(1)})
	expect(t, got, brief{ActionPreempt, 2, ReasonPressureCritical})
	got = s.Decide(Input{Now: at(30), Pressure: domain.PressureCritical, Heavy: heavy, Held: keys(1, 3), Paused: keys(1, 2)})
	expect(t, got)
	// A held workload is never resumed by the scheduler.
	got = s.Decide(Input{Now: at(60), Pressure: domain.PressureNormal, Heavy: heavy, Held: keys(1, 3), Paused: keys(1, 2)})
	expect(t, got, brief{ActionResume, 2, ReasonSlotAvailable})
}

func TestObserveMode(t *testing.T) {
	p := enforce(1)
	p.Mode = ModeObserve
	s := New(p)
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
	got := s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy})
	expect(t, got, brief{ActionQueue, 2, ReasonSlotsExhausted})
	if !got[0].Observed {
		t.Fatal("observe-mode decision not marked Observed")
	}
	// Nothing acted, so 2 runs and is no longer new or queued.
	expect(t, s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy}))
	if !s.seen[key(2)].queuedSince.IsZero() {
		t.Fatal("unexecuted queue decision kept queue state")
	}
}

func TestStateIsPrunedAndBounded(t *testing.T) {
	s := New(enforce(2))
	for i := range 1000 {
		heavy := []process.WorkloadView{wv(10+i, "a"), wv(11+i, "b")}
		s.Decide(Input{Now: at(i), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(10 + i)})
	}
	if len(s.seen) != 2 {
		t.Fatalf("seen = %d entries, want 2", len(s.seen))
	}
}

func TestPausedBeforeSchedulerKnewIt(t *testing.T) {
	// After a daemon restart the queue is reported through Paused.
	s := New(enforce(1))
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b")}
	expect(t, s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2, 77)}))
	if !s.seen[key(2)].queuedSince.Equal(at(0)) {
		t.Fatal("queuedSince not set for adopted paused workload")
	}
	expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy[1:], Paused: keys(2)}),
		brief{ActionResume, 2, ReasonSlotAvailable})
}

func TestClockGoingBackwardsDoesNotBlockResumes(t *testing.T) {
	s := New(enforce(1))
	heavy := []process.WorkloadView{wv(2, "b"), wv(3, "c")}
	s.Decide(Input{Now: at(100), Pressure: domain.PressureNormal})
	expect(t, s.Decide(Input{Now: at(100), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2, 3)}),
		brief{ActionResume, 2, ReasonSlotAvailable})
	expect(t, s.Decide(Input{Now: at(50), Pressure: domain.PressureNormal, Heavy: heavy[1:], Paused: keys(3)}),
		brief{ActionResume, 3, ReasonSlotAvailable})
}

func TestDuplicateSnapshotEntriesAndSortedOutput(t *testing.T) {
	s := New(enforce(1))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a"), wv(50, "a")}})
	heavy := []process.WorkloadView{wv(1, "a"), wv(50, "a"), wv(9, "b"), wv(9, "b"), wv(3, "c"),
		{Key: domain.ProcessKey{PID: 3, Start: t0.Add(time.Second)}, Owner: "c"}}
	got := s.Decide(Input{Now: at(20), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(50)})
	expect(t, got,
		brief{ActionQueue, 3, ReasonSlotsExhausted}, brief{ActionQueue, 3, ReasonSlotsExhausted},
		brief{ActionQueue, 9, ReasonSlotsExhausted})
	if !got[0].Key.Start.Before(got[1].Key.Start) {
		t.Fatal("output not sorted by start time within a PID")
	}
	crit := enforce(5)
	crit.PauseOnCritical = true
	s = New(crit)
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a"), wv(3, "c")}})
	got = s.Decide(Input{Now: at(1), Pressure: domain.PressureCritical,
		Heavy: []process.WorkloadView{wv(3, "c"), wv(1, "a"), wv(2, "b")}})
	expect(t, got, brief{ActionQueue, 2, ReasonPressureCritical}, brief{ActionPreempt, 3, ReasonPressureCritical})
}
