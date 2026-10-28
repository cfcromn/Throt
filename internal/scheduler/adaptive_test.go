package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

func adaptive(ceiling int) Policy {
	p := enforce(ceiling)
	p.Adaptive = true
	return p
}

func TestAdaptivePolicyNormalize(t *testing.T) {
	got := New(Policy{Mode: ModeEnforce, AdaptInterval: -time.Second, PerOwnerLimit: -3}).Policy()
	if got.AdaptInterval != DefaultAdaptInterval || got.PerOwnerLimit != 0 {
		t.Fatalf("normalized = %+v", got)
	}
	if d := DefaultPolicy(); d.Adaptive || d.AdaptInterval != DefaultAdaptInterval || d.PerOwnerLimit != 0 {
		t.Fatalf("DefaultPolicy = %+v", d)
	}
}

func TestAdaptiveLimit(t *testing.T) {
	N, E, H, C, U := domain.PressureNormal, domain.PressureElevated, domain.PressureHigh, domain.PressureCritical, domain.PressureUnknown
	type step struct {
		sec      int
		pressure domain.PressureLevel
		limit    int
	}
	tests := []struct {
		name    string
		ceiling int
		steps   []step
		changes []LimitChange
	}{
		{
			name: "starts at the ceiling and holds at normal", ceiling: 8,
			steps: []step{{0, N, 8}, {60, N, 8}},
		},
		{
			name: "high halves at most once per interval", ceiling: 8,
			steps: []step{{0, H, 4}, {10, H, 4}, {29, H, 4}, {30, H, 2}, {60, H, 1}, {90, H, 1}},
			changes: []LimitChange{
				{8, 4, ReasonPressureHigh, at(0)}, {4, 2, ReasonPressureHigh, at(30)}, {2, 1, ReasonPressureHigh, at(60)},
			},
		},
		{
			name: "critical halves with its own reason", ceiling: 5,
			steps:   []step{{0, C, 2}},
			changes: []LimitChange{{5, 2, ReasonPressureCritical, at(0)}},
		},
		{
			name: "normal adds one per interval up to the ceiling", ceiling: 4,
			steps: []step{{0, H, 2}, {1, N, 2}, {29, N, 2}, {30, N, 3}, {45, N, 3}, {60, N, 4}, {90, N, 4}},
			changes: []LimitChange{
				{4, 2, ReasonPressureHigh, at(0)}, {2, 3, ReasonPressureNormal, at(30)}, {3, 4, ReasonPressureNormal, at(60)},
			},
		},
		{
			name: "elevated holds", ceiling: 4,
			steps:   []step{{0, H, 2}, {30, E, 2}, {300, E, 2}},
			changes: []LimitChange{{4, 2, ReasonPressureHigh, at(0)}},
		},
		{
			name: "unknown resets to the ceiling at once and does not delay a decrease", ceiling: 8,
			steps: []step{{0, H, 4}, {1, U, 8}, {2, H, 4}},
			changes: []LimitChange{
				{8, 4, ReasonPressureHigh, at(0)}, {4, 8, ReasonPressureUnknown, at(1)}, {8, 4, ReasonPressureHigh, at(2)},
			},
		},
		{
			name: "a ceiling of one never moves", ceiling: 1,
			steps: []step{{0, H, 1}, {30, C, 1}, {60, N, 1}, {90, U, 1}},
		},
		{
			name: "clock going backwards does not block adaptation", ceiling: 8,
			steps:   []step{{100, H, 4}, {50, H, 2}},
			changes: []LimitChange{{8, 4, ReasonPressureHigh, at(100)}, {4, 2, ReasonPressureHigh, at(50)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(adaptive(tt.ceiling))
			if s.Limit() != tt.ceiling {
				t.Fatalf("initial limit = %d", s.Limit())
			}
			for _, st := range tt.steps {
				s.Decide(Input{Now: at(st.sec), Pressure: st.pressure})
				if s.Limit() != st.limit {
					t.Fatalf("t=%d %v: limit = %d, want %d", st.sec, st.pressure, s.Limit(), st.limit)
				}
			}
			if got := s.Changes(); !slices.Equal(got, tt.changes) {
				t.Fatalf("changes = %+v, want %+v", got, tt.changes)
			}
			if got := s.Changes(); got != nil {
				t.Fatalf("second Changes = %+v, want nil", got)
			}
		})
	}
}

func TestAdaptiveIntervalPolicy(t *testing.T) {
	p := adaptive(8)
	p.AdaptInterval = 5 * time.Second
	s := New(p)
	s.Decide(Input{Now: at(0), Pressure: domain.PressureHigh})
	s.Decide(Input{Now: at(5), Pressure: domain.PressureHigh})
	if s.Limit() != 2 {
		t.Fatalf("limit = %d, want 2", s.Limit())
	}
}

func TestNonAdaptiveLimitIsFixed(t *testing.T) {
	s := New(enforce(4))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureCritical})
	s.Decide(Input{Now: at(100), Pressure: domain.PressureCritical})
	if s.Limit() != 4 || s.Changes() != nil {
		t.Fatalf("limit = %d", s.Limit())
	}
}

func TestAdaptiveLimitGatesStartsWithoutPreempting(t *testing.T) {
	s := New(adaptive(4))
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "b"), wv(3, "c")}
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: heavy})

	// High pressure halves the limit to 2 while 3 run: nobody is preempted.
	got := s.Decide(Input{Now: at(1), Pressure: domain.PressureHigh, Heavy: heavy})
	expect(t, got)
	if s.Limit() != 2 {
		t.Fatalf("limit = %d, want 2", s.Limit())
	}

	// Pressure eases to elevated: the limit holds at 2, so a newcomer waits
	// although the ceiling has a free slot.
	heavy = append(heavy, wv(4, "d"))
	got = s.Decide(Input{Now: at(2), Pressure: domain.PressureElevated, Heavy: heavy})
	expect(t, got, brief{ActionQueue, 4, ReasonSlotsExhausted})
	if got[0].HeavyLimit != 2 || got[0].RunningHeavy != 3 {
		t.Fatalf("decision facts = %+v", got[0])
	}

	// Two finish: one slot under the effective limit frees and 4 resumes.
	heavy = []process.WorkloadView{wv(3, "c"), wv(4, "d")}
	got = s.Decide(Input{Now: at(3), Pressure: domain.PressureElevated, Heavy: heavy, Paused: keys(4)})
	expect(t, got, brief{ActionResume, 4, ReasonSlotAvailable})
	if got[0].HeavyLimit != 2 {
		t.Fatalf("HeavyLimit = %d, want 2", got[0].HeavyLimit)
	}
}

func TestAdaptiveSetPolicy(t *testing.T) {
	t.Run("adaptive to adaptive keeps the limit, clamped", func(t *testing.T) {
		s := New(adaptive(8))
		s.Decide(Input{Now: at(0), Pressure: domain.PressureHigh}) // 4
		s.SetPolicy(adaptive(6))
		if s.Limit() != 4 {
			t.Fatalf("limit = %d, want 4", s.Limit())
		}
		s.SetPolicy(adaptive(3))
		if s.Limit() != 3 {
			t.Fatalf("limit = %d, want 3", s.Limit())
		}
		want := []LimitChange{{8, 4, ReasonPressureHigh, at(0)}, {4, 3, ReasonPolicyChanged, at(0)}}
		if got := s.Changes(); !slices.Equal(got, want) {
			t.Fatalf("changes = %+v, want %+v", got, want)
		}
		// A policy change restarts nothing: the next decrease may happen
		// at once.
		s.Decide(Input{Now: at(1), Pressure: domain.PressureHigh})
		if s.Limit() != 1 {
			t.Fatalf("limit = %d, want 1", s.Limit())
		}
	})
	t.Run("leaving adaptive restores the ceiling", func(t *testing.T) {
		s := New(adaptive(8))
		s.Decide(Input{Now: at(0), Pressure: domain.PressureHigh})
		s.SetPolicy(enforce(8))
		if s.Limit() != 8 {
			t.Fatalf("limit = %d, want 8", s.Limit())
		}
	})
	t.Run("off mode resets the limit and drops changes", func(t *testing.T) {
		s := New(adaptive(8))
		s.Decide(Input{Now: at(0), Pressure: domain.PressureHigh})
		p := adaptive(8)
		p.Mode = ModeOff
		s.SetPolicy(p)
		s.Decide(Input{Now: at(1), Pressure: domain.PressureHigh})
		if s.Limit() != 8 || s.Changes() != nil {
			t.Fatalf("limit = %d", s.Limit())
		}
	})
}

func TestLimitChangesAreBounded(t *testing.T) {
	p := adaptive(2)
	p.AdaptInterval = time.Second
	s := New(p)
	for i := range 3 * MaxLimitChanges {
		pressure := domain.PressureHigh
		if i%2 == 1 {
			pressure = domain.PressureNormal
		}
		s.Decide(Input{Now: at(i), Pressure: pressure})
	}
	got := s.Changes()
	if len(got) != MaxLimitChanges {
		t.Fatalf("len = %d, want %d", len(got), MaxLimitChanges)
	}
	if last := got[len(got)-1]; !last.At.Equal(at(3*MaxLimitChanges - 1)) {
		t.Fatalf("newest change at %v, oldest kept must be dropped first", last.At)
	}
}

func budget(limit, perOwner int) Policy {
	p := enforce(limit)
	p.PerOwnerLimit = perOwner
	return p
}

func TestOwnerBudgetQueuesNewWorkloads(t *testing.T) {
	tests := []struct {
		name     string
		pressure domain.PressureLevel
		owner    string
		want     []brief
	}{
		{"owner at budget with free slots", domain.PressureNormal, "a", []brief{{ActionQueue, 100, ReasonOwnerBudget}}},
		{"owner at budget under elevated", domain.PressureElevated, "a", []brief{{ActionQueue, 100, ReasonOwnerBudget}}},
		{"other owner is admitted", domain.PressureNormal, "b", nil},
		{"high pressure reason wins", domain.PressureHigh, "a", []brief{{ActionQueue, 100, ReasonPressureHigh}}},
		{"unknown fails open", domain.PressureUnknown, "a", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(budget(4, 2))
			heavy := []process.WorkloadView{wv(1, "a"), wv(2, "a")}
			s.Decide(Input{Now: at(0), Pressure: tt.pressure, Heavy: heavy})
			heavy = append(heavy, wv(100, tt.owner))
			expect(t, s.Decide(Input{Now: at(1), Pressure: tt.pressure, Heavy: heavy}), tt.want...)
		})
	}
}

func TestOwnerBudgetInOneRound(t *testing.T) {
	s := New(budget(4, 1))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal})
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "a"), wv(3, "b"), wv(4, "a")}
	expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 2, ReasonOwnerBudget}, brief{ActionQueue, 4, ReasonOwnerBudget})
}

func TestOwnerBudgetDoesNotBlockOtherOwners(t *testing.T) {
	s := New(budget(4, 1))
	heavy := []process.WorkloadView{wv(1, "a")}
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: heavy})
	heavy = append(heavy, wv(2, "a"))
	expect(t, s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 2, ReasonOwnerBudget})
	// 2 waits only for its owner's budget, so b is admitted rather than
	// queued behind it.
	heavy = append(heavy, wv(3, "b"))
	expect(t, s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2)}))
	// But work that could run still blocks a newcomer.
	heavy = []process.WorkloadView{wv(2, "a"), wv(3, "b")}
	s.lastSwitch = at(3) // hold the resume back with the cooldown
	heavy = append(heavy, wv(5, "c"))
	expect(t, s.Decide(Input{Now: at(4), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2)}),
		brief{ActionQueue, 5, ReasonQueueWaiting})
}

func TestOwnerBudgetResumeSelection(t *testing.T) {
	s := New(budget(4, 1))
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
	// 2 (owner a, at budget) queued first; 3 (owner b) queued behind a
	// full global limit.
	s.SetPolicy(budget(1, 1))
	s.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
	heavy := []process.WorkloadView{wv(1, "a"), wv(2, "a")}
	expect(t, s.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 2, ReasonOwnerBudget})
	heavy = append(heavy, wv(3, "b"))
	expect(t, s.Decide(Input{Now: at(3), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2)}),
		brief{ActionQueue, 3, ReasonSlotsExhausted})
	// Slots grow; 2 is older but its owner is at budget, so 3 resumes.
	s.policy.HeavyLimit, s.limit = 4, 4
	expect(t, s.Decide(Input{Now: at(4), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2, 3)}),
		brief{ActionResume, 3, ReasonSlotAvailable})
	// Even aged, 2 waits for its owner's budget...
	expect(t, s.Decide(Input{Now: at(400), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(2)}))
	// ...except under unknown pressure, which fails open for aged work.
	expect(t, s.Decide(Input{Now: at(401), Pressure: domain.PressureUnknown, Heavy: heavy, Paused: keys(2)}),
		brief{ActionResume, 2, ReasonAged})
	// 1 finishes, so a's budget frees.
	s2 := New(budget(4, 1))
	s2.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(1, "a")}})
	heavy = []process.WorkloadView{wv(1, "a"), wv(2, "a")}
	s2.Decide(Input{Now: at(1), Pressure: domain.PressureNormal, Heavy: heavy})
	expect(t, s2.Decide(Input{Now: at(2), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(2, "a")}, Paused: keys(2)}),
		brief{ActionResume, 2, ReasonSlotAvailable})
}

func TestAdaptiveThroughputOnly(t *testing.T) {
	N, E, H := domain.PressureNormal, domain.PressureElevated, domain.PressureHigh
	type step struct {
		sec            int
		pressure       domain.PressureLevel
		throughputOnly bool
		limit          int
	}
	tests := []struct {
		name    string
		steps   []step
		changes []LimitChange
	}{
		{
			name: "throughput-only elevated adds one per interval up to the ceiling",
			steps: []step{
				{0, H, false, 3}, {30, H, false, 1}, {31, E, true, 1}, {59, E, true, 1}, {60, E, true, 2},
				{90, E, true, 3}, {120, E, true, 4}, {150, E, true, 5}, {180, E, true, 6}, {210, E, true, 6},
			},
			changes: []LimitChange{
				{6, 3, ReasonPressureHigh, at(0)}, {3, 1, ReasonPressureHigh, at(30)},
				{1, 2, ReasonPressureThroughput, at(60)}, {2, 3, ReasonPressureThroughput, at(90)},
				{3, 4, ReasonPressureThroughput, at(120)}, {4, 5, ReasonPressureThroughput, at(150)},
				{5, 6, ReasonPressureThroughput, at(180)},
			},
		},
		{
			name:    "memory-elevated holds",
			steps:   []step{{0, H, false, 3}, {30, E, false, 3}, {60, E, false, 3}, {300, E, false, 3}},
			changes: []LimitChange{{6, 3, ReasonPressureHigh, at(0)}},
		},
		{
			name: "the cause may change between rounds",
			steps: []step{
				// The hold at 60 does not use up the interval, so 61 is due.
				{0, H, false, 3}, {30, E, true, 4}, {60, E, false, 4}, {61, E, true, 5}, {90, E, true, 5}, {91, N, false, 6},
			},
			changes: []LimitChange{
				{6, 3, ReasonPressureHigh, at(0)}, {3, 4, ReasonPressureThroughput, at(30)},
				{4, 5, ReasonPressureThroughput, at(61)}, {5, 6, ReasonPressureNormal, at(91)},
			},
		},
		{
			// The flag only matters at elevated.
			name:    "high still halves",
			steps:   []step{{0, H, true, 3}, {30, H, true, 1}},
			changes: []LimitChange{{6, 3, ReasonPressureHigh, at(0)}, {3, 1, ReasonPressureHigh, at(30)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(adaptive(6))
			for _, st := range tt.steps {
				s.Decide(Input{Now: at(st.sec), Pressure: st.pressure, ThroughputOnly: st.throughputOnly})
				if s.Limit() != st.limit {
					t.Fatalf("t=%d %v throughputOnly=%v: limit = %d, want %d", st.sec, st.pressure, st.throughputOnly, s.Limit(), st.limit)
				}
			}
			if got := s.Changes(); !slices.Equal(got, tt.changes) {
				t.Fatalf("changes = %+v, want %+v", got, tt.changes)
			}
		})
	}
}
