package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// withCPU is wv using cpu cores.
func withCPU(pid int, owner string, cpu float64) process.WorkloadView {
	v := wv(pid, owner)
	v.CPU = cpu
	return v
}

// idlePolicy is enforce with one slot and no aging within the tests.
func idlePolicy() Policy {
	p := enforce(1)
	p.MaxQueueWait = time.Hour
	return p
}

// TestIdleHolder drives one slot held by workload 1 while workload 2 waits
// in the queue, rounds every 10s, and checks when the holder's slot frees.
func TestIdleHolder(t *testing.T) {
	quiet, busy := IdleCPU/2, 1.0
	for _, tt := range []struct {
		name     string
		pressure domain.PressureLevel
		// cpu is the holder's CPU in the round at second sec.
		cpu func(sec int) float64
		// resumeAt is the round that resumes workload 2, or -1 for none.
		resumeAt int
	}{
		{"quiet for the window frees the slot", domain.PressureNormal,
			func(int) float64 { return quiet }, 120},
		{"elevated frees it too", domain.PressureElevated,
			func(int) float64 { return quiet }, 120},
		{"just under the threshold counts as quiet", domain.PressureNormal,
			func(int) float64 { return IdleCPU - 0.001 }, 120},
		{"at the threshold is busy", domain.PressureNormal,
			func(int) float64 { return IdleCPU }, -1},
		{"busy holds the slot", domain.PressureNormal,
			func(int) float64 { return busy }, -1},
		{"high pressure frees nothing", domain.PressureHigh,
			func(int) float64 { return quiet }, -1},
		{"critical pressure frees nothing", domain.PressureCritical,
			func(int) float64 { return quiet }, -1},
		// One busy round every 12th: 110s quiet stretches never reach the
		// 2 minute window, so admissions do not follow the flapping.
		{"flapping never reaches the window", domain.PressureNormal,
			func(sec int) float64 {
				if sec/10%12 == 11 {
					return busy
				}
				return quiet
			}, -1},
		// The window restarts after the last busy round at 50s.
		{"a busy round restarts the window", domain.PressureNormal,
			func(sec int) float64 {
				if sec == 50 {
					return busy
				}
				return quiet
			}, 180},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(idlePolicy())
			s.Decide(Input{Now: at(0), Pressure: tt.pressure, Heavy: []process.WorkloadView{withCPU(1, "a", tt.cpu(0))}})
			queueReason := ReasonSlotsExhausted
			switch tt.pressure {
			case domain.PressureHigh:
				queueReason = ReasonPressureHigh
			case domain.PressureCritical:
				queueReason = ReasonPressureCritical
			}
			expect(t, s.Decide(Input{Now: at(10), Pressure: tt.pressure, Heavy: []process.WorkloadView{withCPU(1, "a", tt.cpu(10)), wv(2, "b")}}),
				brief{ActionQueue, 2, queueReason})
			resumed := -1
			for sec := 20; sec <= 600 && resumed < 0; sec += 10 {
				heavy := []process.WorkloadView{withCPU(1, "a", tt.cpu(sec)), withCPU(2, "b", 0)}
				ds := s.Decide(Input{Now: at(sec), Pressure: tt.pressure, Heavy: heavy, Paused: keys(2)})
				switch {
				case len(ds) == 0:
				case len(ds) == 1 && ds[0].Action == ActionResume && ds[0].Key.PID == 2:
					if ds[0].Reason != ReasonHolderIdle || ds[0].RunningHeavy != 1 || ds[0].HeavyLimit != 1 {
						t.Fatalf("t=%d: resume %+v, want %s with 1/1 running", sec, ds[0], ReasonHolderIdle)
					}
					if got := s.Idle(); !slices.Equal(got, keys(1)) {
						t.Fatalf("t=%d: Idle() = %v, want the holder", sec, got)
					}
					resumed = sec
				default:
					t.Fatalf("t=%d: decisions %v", sec, briefs(ds))
				}
			}
			if resumed != tt.resumeAt {
				t.Fatalf("resumed at %d, want %d", resumed, tt.resumeAt)
			}
		})
	}
}

// TestIdleHolderWakes checks that an idle holder that becomes busy again
// counts at once, above the limit, without being preempted, and that
// nothing new starts until the count falls under the limit.
func TestIdleHolderWakes(t *testing.T) {
	p := idlePolicy()
	p.PauseOnCritical = true
	s := New(p)
	quiet := withCPU(1, "a", 0)
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{quiet}})
	expect(t, s.Decide(Input{Now: at(10), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{quiet, wv(2, "b")}}),
		brief{ActionQueue, 2, ReasonSlotsExhausted})
	for sec := 20; sec < 120; sec += 10 {
		expect(t, s.Decide(Input{Now: at(sec), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{quiet, wv(2, "b")}, Paused: keys(2)}))
	}
	expect(t, s.Decide(Input{Now: at(120), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{quiet, wv(2, "b")}, Paused: keys(2)}),
		brief{ActionResume, 2, ReasonHolderIdle})

	// The holder wakes: two run under a limit of one. Nothing is preempted
	// and a newcomer queues.
	woke := wv(1, "a")
	expect(t, s.Decide(Input{Now: at(130), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{woke, wv(2, "b")}}))
	if got := s.Idle(); len(got) != 0 {
		t.Fatalf("Idle() = %v after waking, want none", got)
	}
	got := s.Decide(Input{Now: at(140), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{woke, wv(2, "b"), wv(3, "c")}})
	expect(t, got, brief{ActionQueue, 3, ReasonSlotsExhausted})
	if got[0].RunningHeavy != 2 {
		t.Fatalf("running = %d, want 2 (over the limit)", got[0].RunningHeavy)
	}
	// One exit brings the count to the limit: still nothing starts.
	expect(t, s.Decide(Input{Now: at(150), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{woke, wv(3, "c")}, Paused: keys(3), Exited: keys(2)}))
	// Critical pressure preempts no workload for having been idle, and the
	// last running one never.
	expect(t, s.Decide(Input{Now: at(160), Pressure: domain.PressureCritical, Heavy: []process.WorkloadView{woke, wv(3, "c")}, Paused: keys(3)}))
	// The second exit frees the slot.
	expect(t, s.Decide(Input{Now: at(200), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{wv(3, "c")}, Paused: keys(3), Exited: keys(1)}),
		brief{ActionResume, 3, ReasonSlotAvailable})
}

// TestIdleWindowNeedsSamples checks that a sampling gap restarts the idle
// window, so one sample after a gap cannot free a slot, and that paused
// workloads are never idle.
func TestIdleWindowNeedsSamples(t *testing.T) {
	s := New(idlePolicy())
	quiet := withCPU(1, "a", 0)
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{quiet}})
	expect(t, s.Decide(Input{Now: at(10), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{quiet, withCPU(2, "b", 0)}}),
		brief{ActionQueue, 2, ReasonSlotsExhausted})
	round := func(sec int) []Decision {
		return s.Decide(Input{Now: at(sec), Pressure: domain.PressureNormal, Heavy: []process.WorkloadView{quiet, withCPU(2, "b", 0)}, Paused: keys(2)})
	}
	for sec := 20; sec <= 100; sec += 10 {
		expect(t, round(sec))
	}
	// 100s without a round: the window restarts at 200.
	for sec := 200; sec < 320; sec += 10 {
		expect(t, round(sec))
		if got := s.Idle(); len(got) != 0 {
			t.Fatalf("t=%d: Idle() = %v, want none", sec, got)
		}
	}
	expect(t, round(320), brief{ActionResume, 2, ReasonHolderIdle})
}

// TestIdleHolderResumesAreSpaced checks that slots freed by idle holders
// are refilled one per ResumeCooldown, even at normal pressure, where free
// slots are otherwise refilled at once.
func TestIdleHolderResumesAreSpaced(t *testing.T) {
	p := enforce(2)
	p.MaxQueueWait = time.Hour
	s := New(p)
	holders := []process.WorkloadView{withCPU(1, "a", 0), withCPU(2, "a", 0)}
	s.Decide(Input{Now: at(0), Pressure: domain.PressureNormal, Heavy: holders})
	heavy := append(slices.Clone(holders), wv(3, "b"), wv(4, "c"))
	expect(t, s.Decide(Input{Now: at(5), Pressure: domain.PressureNormal, Heavy: heavy}),
		brief{ActionQueue, 3, ReasonSlotsExhausted}, brief{ActionQueue, 4, ReasonSlotsExhausted})
	for sec := 10; sec < 120; sec += 10 {
		expect(t, s.Decide(Input{Now: at(sec), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(3, 4)}))
	}
	expect(t, s.Decide(Input{Now: at(120), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(3, 4)}),
		brief{ActionResume, 3, ReasonHolderIdle})
	expect(t, s.Decide(Input{Now: at(125), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(4)}))
	expect(t, s.Decide(Input{Now: at(130), Pressure: domain.PressureNormal, Heavy: heavy, Paused: keys(4)}),
		brief{ActionResume, 4, ReasonHolderIdle})
}
