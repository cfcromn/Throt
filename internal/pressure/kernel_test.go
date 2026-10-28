package pressure

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// kernelSeries returns one sample per second starting at start, all with
// mem; verdict(i) picks the kernel verdict of the i-th sample.
func kernelSeries(start, n int, mem float64, verdict func(i int) domain.MemoryPressure) []sig {
	out := make([]sig, n)
	for i := range out {
		out[i] = sig{sec: float64(start + i), mem: mem, kp: verdict(i)}
	}
	return out
}

func always(v domain.MemoryPressure) func(int) domain.MemoryPressure {
	return func(int) domain.MemoryPressure { return v }
}

// alternate returns a for even samples and b for odd ones.
func alternate(a, b domain.MemoryPressure) func(int) domain.MemoryPressure {
	return func(i int) domain.MemoryPressure {
		if i%2 == 0 {
			return a
		}
		return b
	}
}

// blip returns base everywhere except v at sample at.
func blip(base, v domain.MemoryPressure, at int) func(int) domain.MemoryPressure {
	return func(i int) domain.MemoryPressure {
		if i == at {
			return v
		}
		return base
	}
}

func withLoad(s []sig, load float64) []sig {
	for i := range s {
		s[i].load = load
	}
	return s
}

func withMissing(s []sig, m ...domain.Metric) []sig {
	for i := range s {
		s[i].missing = m
	}
	return s
}

func TestKernelMemoryPressure(t *testing.T) {
	N, E, H := domain.PressureNormal, domain.PressureElevated, domain.PressureHigh
	KU, KN, KW, KC := domain.MemoryPressureUnknown, domain.MemoryPressureNormal, domain.MemoryPressureWarning, domain.MemoryPressureCritical

	tests := []struct {
		name    string
		memUsed bool // Thresholds.MemoryUsedWhenKernelSignal
		script  []sig
		// want lists every transition; sec and reason are checked when set.
		want  []transition
		final domain.PressureLevel
	}{
		{
			name:   "signal absent: memory used applies",
			script: kernelSeries(0, 10, 0.85, always(KU)),
			want:   []transition{{level: E, reason: "memory_used>=0.8"}},
			final:  E,
		},
		{
			name:   "signal listed missing is ignored even if set",
			script: withMissing(kernelSeries(0, 10, 0.85, always(KC)), domain.MetricMemoryPressure),
			want:   []transition{{level: E, reason: "memory_used>=0.8"}},
			final:  E,
		},
		{
			name:   "signal present and normal: memory used ignored",
			script: kernelSeries(0, 20, 0.93, always(KN)),
			want:   []transition{{level: N, reason: "below_thresholds"}},
			final:  N,
		},
		{
			name:    "memory used kept when MemoryUsedWhenKernelSignal is set",
			memUsed: true,
			script:  kernelSeries(0, 20, 0.93, always(KN)),
			want:    []transition{{level: H, reason: "memory_used>=0.9"}},
			final:   H,
		},
		{
			name:   "warning sustained: elevated",
			script: kernelSeries(0, 10, 0.3, always(KW)),
			want:   []transition{{sec: 2, level: E, reason: "kernel_memory_pressure>=warning"}},
			final:  E,
		},
		{
			name:   "critical sustained: high, never critical",
			script: kernelSeries(0, 120, 0.3, always(KC)),
			want:   []transition{{sec: 2, level: H, reason: "kernel_memory_pressure>=critical"}},
			final:  H,
		},
		{
			name:   "single warning blip does not count",
			script: kernelSeries(0, 20, 0.3, blip(KN, KW, 5)),
			want:   []transition{{level: N}},
			final:  N,
		},
		{
			name:   "flapping normal/warning never escalates",
			script: kernelSeries(0, 40, 0.3, alternate(KN, KW)),
			want:   []transition{{level: N}},
			final:  N,
		},
		{
			// Entering needs the median at warning; holding needs only half
			// the window there, so a flapping verdict keeps elevated until
			// normal is the majority for the recovery dwell (2x the window).
			name: "hysteresis: flapping holds elevated, normal recovers after dwell",
			script: concat(
				kernelSeries(0, 10, 0.3, always(KW)),
				kernelSeries(10, 30, 0.3, alternate(KN, KW)),
				kernelSeries(40, 30, 0.3, always(KN)),
			),
			want:  []transition{{sec: 2, level: E}, {sec: 51, level: N, reason: "recovered"}},
			final: N,
		},
		{
			name: "critical easing to warning: high recovers to elevated",
			script: concat(
				kernelSeries(0, 10, 0.3, always(KC)),
				kernelSeries(10, 30, 0.3, always(KW)),
			),
			want:  []transition{{sec: 2, level: H}, {sec: 23, level: E, reason: "kernel_memory_pressure>=warning"}},
			final: E,
		},
		{
			name:   "other signals still escalate while the verdict is normal",
			script: withLoad(kernelSeries(0, 10, 0.3, always(KN)), 1.2),
			want:   []transition{{level: E, reason: "load>=1"}},
			final:  E,
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
