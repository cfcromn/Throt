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
	// Hold the resume back with the cooldown after a pressure-driven pause,
	// which 1's exit does not skip.
	s.lastSwitch, s.lastPressureSwitch = at(3), at(3)
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

func TestAdaptiveStartLimit(t *testing.T) {
	// The balanced profile on 5 CPUs: start 2, ceiling 5.
	p := adaptive(5)
	p.StartLimit = 2
	s := New(p)
	if s.Limit() != 2 || s.Policy().StartLimit != 2 {
		t.Fatalf("initial limit %d, start %d; want 2", s.Limit(), s.Policy().StartLimit)
	}
	// CPU-saturated elevated pressure grows it to the ceiling and no further.
	for sec := 0; sec <= 150; sec += 30 {
		s.Decide(Input{Now: at(sec), Pressure: domain.PressureElevated, ThroughputOnly: true})
	}
	if s.Limit() != 5 {
		t.Fatalf("limit %d after growth, want the ceiling 5", s.Limit())
	}
	if got := len(s.Changes()); got != 3 {
		t.Fatalf("%d changes, want 3 (2 -> 5)", got)
	}

	// Off resets to the start; so does turning Adaptive on.
	s.SetPolicy(Policy{Mode: ModeOff, HeavyLimit: 5, StartLimit: 2, Adaptive: true})
	s.Decide(Input{Now: at(200)})
	if s.Limit() != 2 {
		t.Fatalf("limit %d after off, want 2", s.Limit())
	}
	s.SetPolicy(enforce(5))
	if s.Limit() != 5 {
		t.Fatalf("non-adaptive limit %d, want the fixed 5", s.Limit())
	}
	p.StartLimit = 3
	s.SetPolicy(p)
	if s.Limit() != 3 {
		t.Fatalf("limit %d after turning adaptive on, want start 3", s.Limit())
	}
	// Staying adaptive keeps the current limit, capped at the new ceiling.
	p.HeavyLimit, p.StartLimit = 8, 1
	s.SetPolicy(p)
	if s.Limit() != 3 {
		t.Fatalf("limit %d after a reload that stays adaptive, want 3 kept", s.Limit())
	}
}

func TestStartLimitNormalize(t *testing.T) {
	for _, tc := range []struct {
		start, want int
		adaptive    bool
		limit       int
	}{
		{0, 6, true, 6},  // unset: the ceiling
		{-1, 6, true, 6}, // invalid: the ceiling
		{9, 6, true, 6},  // above the ceiling: the ceiling
		{2, 2, true, 2},
		{2, 2, false, 6}, // ignored without Adaptive
	} {
		p := enforce(6)
		p.Adaptive, p.StartLimit = tc.adaptive, tc.start
		s := New(p)
		if s.Policy().StartLimit != tc.want || s.Limit() != tc.limit {
			t.Errorf("start %d adaptive %v: normalized start %d limit %d, want %d %d",
				tc.start, tc.adaptive, s.Policy().StartLimit, s.Limit(), tc.want, tc.limit)
		}
	}
}

func TestAdaptiveStartLimitSurvivesWarmUp(t *testing.T) {
	// The pressure engine reports unknown until it has escalated out of
	// no_samples, so every daemon start begins with a few unknown rounds.
	// They are not degraded metrics and must not throw the start limit away.
	U, N := domain.PressureUnknown, domain.PressureNormal
	p := adaptive(8)
	p.StartLimit = 2
	s := New(p)
	for sec := range 3 {
		s.Decide(Input{Now: at(sec), Pressure: U})
	}
	if s.Limit() != 2 {
		t.Fatalf("limit %d after warm-up, want the start limit 2", s.Limit())
	}
	if got := s.Changes(); got != nil {
		t.Fatalf("warm-up changed the limit: %+v", got)
	}
	s.Decide(Input{Now: at(3), Pressure: N})
	if s.Limit() != 3 {
		t.Fatalf("limit %d after the first normal round, want 3", s.Limit())
	}
	// Metrics lost after they were known still fail open.
	s.Decide(Input{Now: at(4), Pressure: U})
	if s.Limit() != 8 {
		t.Fatalf("limit %d with degraded metrics, want the ceiling 8", s.Limit())
	}
}

func TestAdaptiveGrowsForWaitingWork(t *testing.T) {
	// The balanced profile on 5 CPUs: start 2, ceiling 5. Round 0 knows the
	// pressure and adds one; a burst arrives at 1 and the scheduler queues
	// what does not fit; round 2 may grow for it.
	const mb = 1 << 20
	E, N, H := domain.PressureElevated, domain.PressureNormal, domain.PressureHigh
	type step struct {
		sec            int
		pressure       domain.PressureLevel
		throughputOnly bool
		jobs           int    // heavy workloads, PIDs 1..jobs
		rss            uint64 // RSS of each
		headroom       uint64
		reserved       int
		limit          int
	}
	burst := func(p domain.PressureLevel, tp bool, rss1, rss2, headroom uint64, limit int) []step {
		return []step{
			{0, E, true, 0, 0, 0, 0, 3},
			{1, p, tp, 10, rss1, headroom, 0, 3},
			{2, p, tp, 10, rss2, headroom, 0, limit},
		}
	}
	tests := []struct {
		name    string
		steps   []step
		resumed int
		reason  string
	}{
		{name: "CPU-only burst grows to the ceiling", steps: burst(E, true, 5*mb, 5*mb, 8<<30, 5), resumed: 2, reason: ReasonPressureThroughput},
		{name: "normal pressure grows to the ceiling", steps: burst(N, false, 5*mb, 5*mb, 8<<30, 5), resumed: 2, reason: ReasonPressureNormal},
		{name: "memory-elevated holds", steps: burst(E, false, 5*mb, 5*mb, 8<<30, 3)},
		{name: "headroom for one more", steps: burst(E, true, 1<<30, 1<<30, 3<<29, 4), resumed: 1, reason: ReasonPressureThroughput},
		{name: "no headroom holds", steps: burst(E, true, 5*mb, 5*mb, 0, 3)},
		{name: "a growing working set holds", steps: burst(E, true, 512*mb, 1<<30, 8<<30, 3)},
		{name: "growth within 1/8 counts as settled", steps: burst(E, true, 800*mb, 900*mb, 8<<30, 5), resumed: 2, reason: ReasonPressureThroughput},
		{name: "unknown RSS holds", steps: burst(E, true, 0, 0, 8<<30, 3)},
		{
			name: "only as far as the work waiting",
			steps: []step{
				{0, E, true, 0, 0, 0, 0, 3},
				{1, E, true, 4, 5 * mb, 8 << 30, 0, 3},
				{2, E, true, 4, 5 * mb, 8 << 30, 0, 4},
			},
			resumed: 1, reason: ReasonPressureThroughput,
		},
		{
			name: "leases take fitted slots",
			steps: []step{
				{0, E, true, 0, 0, 0, 0, 3},
				{1, E, true, 2, 1 << 30, 2 << 30, 0, 3},
				{2, E, true, 4, 1 << 30, 2 << 30, 2, 3},
			},
		},
		{
			name: "not within an interval of high pressure",
			steps: []step{
				{0, H, false, 0, 0, 0, 0, 1},
				{1, E, true, 10, 5 * mb, 8 << 30, 0, 1},
				{2, E, true, 10, 5 * mb, 8 << 30, 0, 1},
				{29, E, true, 10, 5 * mb, 8 << 30, 0, 1},
				{30, E, true, 10, 5 * mb, 8 << 30, 0, 5},
			},
			resumed: 4, reason: ReasonPressureThroughput,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The blind limit is below the ceiling: grow, sized by memory,
			// is what takes the limit past it.
			p := adaptive(5)
			p.StartLimit, p.BlindLimit = 2, 3
			s := New(p)
			paused := map[int]bool{}
			resumed := 0
			for _, st := range tt.steps {
				in := Input{
					Now: at(st.sec), Pressure: st.pressure, ThroughputOnly: st.throughputOnly,
					MemoryHeadroom: st.headroom, Reserved: make([]string, st.reserved),
				}
				for pid := 1; pid <= st.jobs; pid++ {
					v := wv(pid, "a")
					v.RSS = st.rss
					in.Heavy = append(in.Heavy, v)
					if paused[pid] {
						in.Paused = append(in.Paused, key(pid))
					}
				}
				for _, d := range s.Decide(in) {
					switch d.Action {
					case ActionQueue:
						paused[d.Key.PID] = true
					case ActionResume:
						paused[d.Key.PID] = false
						resumed++
					}
					if d.HeavyLimit != s.Limit() {
						t.Fatalf("t=%d: decision limit %d, effective %d", st.sec, d.HeavyLimit, s.Limit())
					}
				}
				if s.Limit() != st.limit {
					t.Fatalf("t=%d: limit = %d, want %d", st.sec, s.Limit(), st.limit)
				}
			}
			if resumed != tt.resumed {
				t.Fatalf("resumed %d, want %d", resumed, tt.resumed)
			}
			var last LimitChange
			if cs := s.Changes(); len(cs) > 0 {
				last = cs[len(cs)-1]
			}
			if tt.reason != "" && last.Reason != tt.reason {
				t.Fatalf("last change %+v, want reason %s", last, tt.reason)
			}
		})
	}
}

// TestAdaptiveBlindLimit checks that without memory evidence the limit
// rises, and resets at unknown pressure, only to BlindLimit. That grow,
// sized by memory headroom, goes past it is TestAdaptiveGrowsForWaitingWork.
func TestAdaptiveBlindLimit(t *testing.T) {
	p := adaptive(16)
	p.StartLimit, p.BlindLimit = 2, 4
	s := New(p)
	for sec := 0; sec <= 300; sec += 30 {
		s.Decide(Input{Now: at(sec), Pressure: domain.PressureNormal})
	}
	if s.Limit() != 4 {
		t.Fatalf("limit after calm minutes = %d, want the blind limit 4", s.Limit())
	}
	s.Decide(Input{Now: at(301), Pressure: domain.PressureHigh})
	s.Decide(Input{Now: at(302), Pressure: domain.PressureUnknown})
	if s.Limit() != 4 {
		t.Fatalf("limit after unknown pressure = %d, want the blind limit 4", s.Limit())
	}
}
