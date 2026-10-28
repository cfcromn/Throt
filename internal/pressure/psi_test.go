package pressure

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// series returns n samples, one per second from start; f fills sample i.
func series(start, n int, f func(i int) sig) []sig {
	out := make([]sig, n)
	for i := range out {
		out[i] = f(i)
		out[i].sec = float64(start + i)
	}
	return out
}

func stall(some, full float64) *domain.Stall { return &domain.Stall{Some: some, Full: full} }

// memStall samples memory used mem with a memory PSI reading.
func memStall(mem float64, st func(i int) *domain.Stall) func(int) sig {
	return func(i int) sig { return sig{mem: mem, memStall: st(i)} }
}

func steady(st *domain.Stall) func(int) *domain.Stall { return func(int) *domain.Stall { return st } }

// flap returns a for even samples and b for odd ones.
func flap(a, b *domain.Stall) func(int) *domain.Stall {
	return func(i int) *domain.Stall {
		if i%2 == 0 {
			return a
		}
		return b
	}
}

func TestStallSignals(t *testing.T) {
	N, E, H, C := domain.PressureNormal, domain.PressureElevated, domain.PressureHigh, domain.PressureCritical

	tests := []struct {
		name    string
		memUsed bool // Thresholds.MemoryUsedWhenKernelSignal
		script  []sig
		want    []transition // sec and reason are checked when set
		final   domain.PressureLevel
	}{
		// Memory.
		{
			name:   "memory some below 10%: normal",
			script: series(0, 20, memStall(0.3, steady(stall(0.09, 0.04)))),
			want:   []transition{{level: N, reason: "below_thresholds"}},
			final:  N,
		},
		{
			name:   "memory some 10%: elevated",
			script: series(0, 20, memStall(0.3, steady(stall(0.10, 0)))),
			want:   []transition{{sec: 2, level: E, reason: "memory_stall_some>=0.1"}},
			final:  E,
		},
		{
			name:   "memory full 5%: high",
			script: series(0, 20, memStall(0.3, steady(stall(0.30, 0.05)))),
			want:   []transition{{sec: 2, level: H, reason: "memory_stall_full>=0.05"}},
			final:  H,
		},
		{
			name:   "memory full 20%: critical",
			script: series(0, 20, memStall(0.3, steady(stall(0.60, 0.20)))),
			want:   []transition{{sec: 2, level: C, reason: "memory_stall_full>=0.2"}},
			final:  C,
		},
		{
			name: "critical easing to full 6%: recovers to high",
			script: concat(
				series(0, 10, memStall(0.3, steady(stall(0.6, 0.25)))),
				series(10, 30, memStall(0.3, steady(stall(0.2, 0.06)))),
			),
			want:  []transition{{sec: 2, level: C}, {sec: 23, level: H, reason: "memory_stall_full>=0.025"}},
			final: H,
		},

		// Precedence over memory used.
		{
			name:   "memory PSI present: memory used ignored below critical",
			script: series(0, 20, memStall(0.93, steady(stall(0, 0)))),
			want:   []transition{{level: N, reason: "below_thresholds"}},
			final:  N,
		},
		{
			name:    "memory used kept when MemoryUsedWhenKernelSignal is set",
			memUsed: true,
			script:  series(0, 20, memStall(0.93, steady(stall(0, 0)))),
			want:    []transition{{level: H, reason: "memory_used>=0.9"}},
			final:   H,
		},
		{
			name:   "memory PSI absent: memory used applies",
			script: series(0, 20, memStall(0.93, steady(nil))),
			want:   []transition{{level: H, reason: "memory_used>=0.9"}},
			final:  H,
		},
		{
			name: "memory PSI listed missing is ignored even if set",
			script: withMissing(series(0, 20, memStall(0.93, steady(stall(0.9, 0.9)))),
				domain.MetricMemoryStall),
			want:  []transition{{level: H, reason: "memory_used>=0.9"}},
			final: H,
		},
		{
			name: "io and cpu PSI do not displace memory used",
			script: series(0, 20, func(int) sig {
				return sig{mem: 0.93, ioStall: stall(0, 0), cpuStall: stall(0, 0)}
			}),
			want:  []transition{{level: H, reason: "memory_used>=0.9"}},
			final: H,
		},

		// I/O.
		{
			name:   "io some alone never escalates",
			script: series(0, 20, func(int) sig { return sig{ioStall: stall(0.9, 0.1)} }),
			want:   []transition{{level: N}},
			final:  N,
		},
		{
			name:   "io full 20%: elevated",
			script: series(0, 20, func(int) sig { return sig{ioStall: stall(0.5, 0.2)} }),
			want:   []transition{{sec: 2, level: E, reason: "io_stall_full>=0.2"}},
			final:  E,
		},
		{
			name:   "io full 100%: high, never critical",
			script: series(0, 120, func(int) sig { return sig{ioStall: stall(1, 1)} }),
			want:   []transition{{sec: 2, level: H, reason: "io_stall_full>=0.5"}},
			final:  H,
		},

		// CPU.
		{
			name:   "cpu some below 50%: normal",
			script: series(0, 20, func(int) sig { return sig{cpuStall: stall(0.49, 0)} }),
			want:   []transition{{level: N}},
			final:  N,
		},
		{
			name:   "cpu some 100%: elevated only",
			script: series(0, 120, func(int) sig { return sig{cpuStall: stall(1, 1)} }),
			want:   []transition{{sec: 2, level: E, reason: "cpu_stall_some>=0.5"}},
			final:  E,
		},

		// Missing and flapping.
		{
			name: "all PSI missing with nothing else: unknown",
			script: series(0, 5, func(int) sig {
				return sig{memStall: stall(1, 1), missing: []domain.Metric{
					domain.MetricCPU, domain.MetricLoad, domain.MetricMemory, domain.MetricSwap,
					domain.MetricLatency, domain.MetricMemoryStall,
				}}
			}),
			want:  nil,
			final: domain.PressureUnknown,
		},
		{
			name: "PSI alone is a signal",
			script: series(0, 5, func(int) sig {
				return sig{cpuStall: stall(0, 0), missing: []domain.Metric{
					domain.MetricCPU, domain.MetricLoad, domain.MetricMemory, domain.MetricSwap, domain.MetricLatency,
				}}
			}),
			want:  []transition{{level: N}},
			final: N,
		},
		{
			name: "single memory full spike does not count",
			script: series(0, 20, memStall(0.3, func(i int) *domain.Stall {
				if i == 5 {
					return stall(0.9, 0.9)
				}
				return stall(0, 0)
			})),
			want:  []transition{{level: N}},
			final: N,
		},
		{
			name:   "flapping memory full 0%/8% never escalates",
			script: series(0, 40, memStall(0.3, flap(stall(0, 0), stall(0.08, 0.08)))),
			want:   []transition{{level: N}},
			final:  N,
		},
		{
			// Enter high at full 5%; full 3% is above the 2.5% exit and
			// holds; 1% recovers after the dwell (2x the 5s window).
			name: "hysteresis: hovering below enter holds, recovery waits for dwell",
			script: concat(
				series(0, 10, memStall(0.3, steady(stall(0.08, 0.08)))),
				series(10, 30, memStall(0.3, steady(stall(0.03, 0.03)))),
				series(40, 30, memStall(0.3, steady(stall(0.01, 0.01)))),
			),
			want:  []transition{{sec: 2, level: H}, {sec: 52, level: N, reason: "recovered"}},
			final: N,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := DefaultThresholds()
			th.MemoryUsedWhenKernelSignal = tc.memUsed
			e := New(5*time.Second, th)
			got := run(t, e, tc.script)
			if len(got) != len(tc.want) {
				t.Fatalf("transitions = %+v, want %+v", got, tc.want)
			}
			for i, w := range tc.want {
				g := got[i]
				if g.level != w.level || (w.sec != 0 && g.sec != w.sec) || (w.reason != "" && g.reason != w.reason) {
					t.Fatalf("transition %d = %+v, want %+v", i, g, w)
				}
			}
			if e.Level() != tc.final {
				t.Fatalf("final level = %v, want %v", e.Level(), tc.final)
			}
		})
	}
}

func TestStallExitRatioDegenerate(t *testing.T) {
	th := DefaultThresholds()
	th.StallExitRatio = 0 // unset: exit falls back to the enter threshold
	e := New(5*time.Second, th)
	got := run(t, e, concat(
		series(0, 10, memStall(0.3, steady(stall(0.08, 0.08)))),
		series(10, 30, memStall(0.3, steady(stall(0.04, 0.04)))),
	))
	want := []transition{{sec: 2, level: domain.PressureHigh}, {sec: 23, level: domain.PressureNormal}}
	if len(got) != len(want) || got[0].level != want[0].level || got[1].level != want[1].level || got[1].sec != want[1].sec {
		t.Fatalf("transitions = %+v, want %+v", got, want)
	}
}

// TestMemoryExhaustion covers near-exhausted memory beside a kernel memory
// signal: with PSI it reaches critical only when RAM and swap together are
// nearly used, and beside the macOS verdict memory used never counts.
func TestMemoryExhaustion(t *testing.T) {
	const gib = 1 << 30
	N, H, C := domain.PressureNormal, domain.PressureHigh, domain.PressureCritical
	type host struct {
		availMiB, swapTotalGiB, swapUsedMiB uint64
		noSwap                              bool // swap metric missing
		psi                                 *domain.Stall
		verdict                             domain.MemoryPressure
	}
	tests := []struct {
		name   string
		h      host
		final  domain.PressureLevel
		reason string
	}{
		{
			// The memory bench with every job queued: RAM full of stopped
			// pages, swap 70% used, nobody stalled. Critical here held
			// every resume back until max_queue_wait.
			name:  "PSI calm, RAM full, swap has room: PSI decides",
			h:     host{availMiB: 3, swapTotalGiB: 1, swapUsedMiB: 713, psi: stall(0, 0)},
			final: N, reason: "below_thresholds",
		},
		{
			name:  "PSI calm, RAM and swap both nearly full: critical",
			h:     host{availMiB: 3, swapTotalGiB: 1, swapUsedMiB: 1000, psi: stall(0, 0)},
			final: C, reason: "memory_and_swap_used>=0.95",
		},
		{
			name:  "PSI calm, no swap configured: RAM alone is exhaustion",
			h:     host{availMiB: 20, psi: stall(0, 0)},
			final: C, reason: "memory_and_swap_used>=0.95",
		},
		{
			name:  "PSI calm, swap unreadable: RAM alone is exhaustion",
			h:     host{availMiB: 20, swapTotalGiB: 1, noSwap: true, psi: stall(0, 0)},
			final: C, reason: "memory_and_swap_used>=0.95",
		},
		{
			name:  "PSI stalled with swap room: PSI still reaches critical",
			h:     host{availMiB: 3, swapTotalGiB: 1, swapUsedMiB: 500, psi: stall(0.6, 0.3)},
			final: C, reason: "memory_stall_full>=0.2",
		},
		{
			name:  "macOS verdict normal, RAM full: memory used ignored",
			h:     host{availMiB: 3, swapTotalGiB: 1, swapUsedMiB: 1020, verdict: domain.MemoryPressureNormal},
			final: N, reason: "below_thresholds",
		},
		{
			name:  "macOS verdict critical, RAM full: high, never critical",
			h:     host{availMiB: 3, swapTotalGiB: 1, swapUsedMiB: 1020, verdict: domain.MemoryPressureCritical},
			final: H, reason: "kernel_memory_pressure>=critical",
		},
		{
			name:  "no kernel signal: memory used applies as before",
			h:     host{availMiB: 3, swapTotalGiB: 1},
			final: C, reason: "memory_used>=0.95",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := New(5*time.Second, DefaultThresholds())
			var level domain.PressureLevel
			var reason string
			for i := range 20 {
				s := domain.HostSample{
					Time:           epoch.Add(time.Duration(i) * time.Second),
					MemTotal:       gib,
					MemAvailable:   tc.h.availMiB * mib,
					SwapTotal:      tc.h.swapTotalGiB * gib,
					SwapUsed:       tc.h.swapUsedMiB * mib,
					MemoryStall:    tc.h.psi,
					MemoryPressure: tc.h.verdict,
					Missing:        []domain.Metric{domain.MetricCPU, domain.MetricLoad, domain.MetricLatency},
				}
				if tc.h.noSwap {
					s.Missing = append(s.Missing, domain.MetricSwap)
				}
				if tc.h.psi == nil {
					s.Missing = append(s.Missing, domain.MetricMemoryStall)
				}
				if !tc.h.verdict.Known() {
					s.Missing = append(s.Missing, domain.MetricMemoryPressure)
				}
				level, _, reason = e.Observe(s)
			}
			if level != tc.final || reason != tc.reason {
				t.Fatalf("level %v (%s), want %v (%s)", level, reason, tc.final, tc.reason)
			}
		})
	}
}
