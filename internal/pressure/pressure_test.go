package pressure

import (
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

const mib = 1 << 20

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// sig describes one scripted sample; zero values are calm except where set.
type sig struct {
	sec     float64 // seconds after epoch
	mem     float64 // memory used fraction
	cpu     float64
	load    float64
	swapMiB float64 // absolute swap used
	missing []domain.Metric
}

func (g sig) sample() domain.HostSample {
	const total = 1000_000
	return domain.HostSample{
		Time:         epoch.Add(time.Duration(g.sec * float64(time.Second))),
		CPUUsage:     g.cpu,
		Load1:        g.load,
		MemTotal:     total,
		MemAvailable: uint64((1 - g.mem) * total),
		SwapTotal:    4096 * mib,
		SwapUsed:     uint64(g.swapMiB * mib),
		Missing:      g.missing,
	}
}

// memSeries returns one sample per second starting at start, all with mem.
func memSeries(start, n int, mem float64) []sig {
	out := make([]sig, n)
	for i := range out {
		out[i] = sig{sec: float64(start + i), mem: mem}
	}
	return out
}

func concat(parts ...[]sig) []sig {
	var out []sig
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type transition struct {
	sec    float64
	level  domain.PressureLevel
	reason string
}

// run feeds script into e and returns every level change.
func run(t *testing.T, e *Engine, script []sig) []transition {
	t.Helper()
	var got []transition
	for _, g := range script {
		l, changed, reason := e.Observe(g.sample())
		if l != e.Level() {
			t.Fatalf("t=%v: Observe returned %v but Level() is %v", g.sec, l, e.Level())
		}
		if reason == "" {
			t.Fatalf("t=%v: empty reason", g.sec)
		}
		if changed {
			got = append(got, transition{g.sec, l, reason})
		}
	}
	return got
}

func levels(ts []transition) []domain.PressureLevel {
	out := make([]domain.PressureLevel, len(ts))
	for i, tr := range ts {
		out[i] = tr.level
	}
	return out
}

func equalLevels(a, b []domain.PressureLevel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTransitions(t *testing.T) {
	N, E, H, C, U := domain.PressureNormal, domain.PressureElevated, domain.PressureHigh, domain.PressureCritical, domain.PressureUnknown

	oscillate := func(start, n int, lo, hi float64) []sig {
		out := make([]sig, n)
		for i := range out {
			m := lo
			if i%2 == 1 {
				m = hi
			}
			out[i] = sig{sec: float64(start + i), mem: m}
		}
		return out
	}
	swapRamp := func(start, n int, perSec float64) []sig {
		out := make([]sig, n)
		for i := range out {
			out[i] = sig{sec: float64(start + i), mem: 0.3, swapMiB: 100 + perSec*float64(i)}
		}
		return out
	}

	tests := []struct {
		name   string
		window time.Duration
		script []sig
		want   []domain.PressureLevel
		final  domain.PressureLevel
		reason string // prefix of the last transition's reason
	}{
		{
			name:   "single spike in window is ignored",
			window: 10 * time.Second,
			script: concat(memSeries(0, 5, 0.3), []sig{{sec: 5, mem: 0.99}}, memSeries(6, 30, 0.3)),
			want:   []domain.PressureLevel{N},
			final:  N,
		},
		{
			// A window holding one sample isolates the consecutive-sample rule.
			name:   "two-sample spike below escalate count is ignored",
			window: 500 * time.Millisecond,
			script: concat(memSeries(0, 3, 0.3), memSeries(3, 2, 0.99), memSeries(5, 10, 0.3)),
			want:   []domain.PressureLevel{N},
			final:  N,
		},
		{
			name:   "sustained rise escalates step by step",
			window: 2 * time.Second,
			script: concat(memSeries(0, 5, 0.3), memSeries(5, 8, 0.85), memSeries(13, 8, 0.92), memSeries(21, 8, 0.97)),
			want:   []domain.PressureLevel{N, E, H, C},
			final:  C,
			reason: "memory_used>=0.95",
		},
		{
			name:   "jump straight to the sustained level",
			window: 2 * time.Second,
			script: concat(memSeries(0, 3, 0.3), memSeries(3, 6, 0.99)),
			want:   []domain.PressureLevel{N, C},
			final:  C,
		},
		{
			name:   "oscillation around enter threshold does not flap",
			window: 10 * time.Second,
			script: concat(memSeries(0, 5, 0.3), oscillate(5, 120, 0.79, 0.81)),
			want:   []domain.PressureLevel{N},
			final:  N,
		},
		{
			name:   "oscillation inside exit margin keeps level",
			window: 2 * time.Second,
			script: concat(memSeries(0, 5, 0.3), memSeries(5, 6, 0.82), oscillate(11, 120, 0.77, 0.81)),
			want:   []domain.PressureLevel{N, E},
			final:  E,
		},
		{
			name:   "drop to intermediate level that still holds its exit threshold",
			window: 2 * time.Second,
			script: concat(memSeries(0, 3, 0.3), memSeries(3, 6, 0.99), memSeries(9, 20, 0.87)),
			want:   []domain.PressureLevel{N, C, H},
			final:  H,
			reason: "memory_used>=0.85",
		},
		{
			name:   "rebound during dwell restarts recovery",
			window: 2 * time.Second,
			// High, then 3s calm (< 4s dwell), back to 0.88 (holds High exit),
			// then calm long enough.
			script: concat(memSeries(0, 3, 0.3), memSeries(3, 5, 0.92), memSeries(8, 3, 0.3),
				memSeries(11, 3, 0.88), memSeries(14, 10, 0.3)),
			want:   []domain.PressureLevel{N, H, N},
			final:  N,
			reason: "recovered",
		},
		{
			name:   "swap growth triggers",
			window: 5 * time.Second,
			script: swapRamp(0, 13, 10),
			want:   []domain.PressureLevel{N, H},
			final:  H,
			reason: "swap_growth>=8388608",
		},
		{
			name:   "swap shrinking is not growth",
			window: 5 * time.Second,
			script: swapRamp(0, 13, -5),
			want:   []domain.PressureLevel{N},
			final:  N,
		},
		{
			name:   "load triggers",
			window: 2 * time.Second,
			script: []sig{{sec: 0, load: 1.6}, {sec: 1, load: 1.6}, {sec: 2, load: 1.6}, {sec: 3, load: 1.6}},
			want:   []domain.PressureLevel{H},
			final:  H,
			reason: "load>=1.5",
		},
		{
			name:   "saturated cpu alone never reaches critical",
			window: 2 * time.Second,
			script: []sig{{sec: 0, cpu: 1}, {sec: 1, cpu: 1}, {sec: 2, cpu: 1}, {sec: 3, cpu: 1}},
			want:   []domain.PressureLevel{H},
			final:  H,
			reason: "cpu>=0.95",
		},
		{
			name:   "missing metrics are ignored",
			window: 2 * time.Second,
			// Raw fields would read as critical if they were trusted.
			script: []sig{
				{sec: 0, mem: 1, cpu: 1, load: 5, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad}},
				{sec: 1, mem: 1, cpu: 1, load: 5, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad}},
				{sec: 2, mem: 1, cpu: 1, load: 5, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad}},
				{sec: 3, mem: 1, cpu: 1, load: 5, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad}},
			},
			want:  []domain.PressureLevel{N},
			final: N,
		},
		{
			name:   "all metrics missing from the start stays unknown",
			window: 2 * time.Second,
			script: []sig{
				{sec: 0, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}},
				{sec: 1, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}},
				{sec: 2, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}},
			},
			want:  []domain.PressureLevel{},
			final: U,
		},
		{
			name:   "all metrics missing for whole window yields unknown",
			window: 2 * time.Second,
			script: concat(memSeries(0, 4, 0.3), []sig{
				{sec: 4, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}},
				{sec: 5, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}},
				{sec: 6, missing: []domain.Metric{domain.MetricMemory, domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}},
			}),
			want:   []domain.PressureLevel{N, U},
			final:  U,
			reason: "no_signals",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := New(tc.window, DefaultThresholds())
			got := run(t, e, tc.script)
			if gl := levels(got); !equalLevels(gl, tc.want) {
				t.Fatalf("transitions = %v, want %v (%+v)", gl, tc.want, got)
			}
			if e.Level() != tc.final {
				t.Fatalf("final level = %v, want %v", e.Level(), tc.final)
			}
			if tc.reason != "" && !strings.HasPrefix(got[len(got)-1].reason, tc.reason) {
				t.Fatalf("last reason = %q, want prefix %q", got[len(got)-1].reason, tc.reason)
			}
		})
	}
}

func TestRecoveryWaitsForDwell(t *testing.T) {
	tests := []struct {
		name  string
		dwell time.Duration
	}{
		{"default dwell is twice the window", 0},
		{"explicit dwell", 15 * time.Second},
	}
	const window = 5 * time.Second
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := DefaultThresholds()
			th.RecoveryDwell = tc.dwell
			wantDwell := tc.dwell
			if wantDwell == 0 {
				wantDwell = 2 * window
			}
			e := New(window, th)
			run(t, e, concat(memSeries(0, 3, 0.3), memSeries(3, 10, 0.93)))
			if e.Level() != domain.PressureHigh {
				t.Fatalf("setup: level = %v, want high", e.Level())
			}

			// From t=13 memory is calm. The median clears the exit threshold
			// once calm samples are the window majority, at t=15.
			const calmStart, belowAt = 13, 15
			got := run(t, e, memSeries(calmStart, 60, 0.3))
			if len(got) != 1 {
				t.Fatalf("transitions = %+v, want exactly one", got)
			}
			if got[0].level != domain.PressureNormal || got[0].reason != "recovered" {
				t.Fatalf("transition = %+v, want normal/recovered", got[0])
			}
			wantAt := float64(belowAt) + wantDwell.Seconds()
			if got[0].sec != wantAt {
				t.Fatalf("recovered at t=%v, want t=%v", got[0].sec, wantAt)
			}
		})
	}
}

func TestWindowPrunesAndStaysBounded(t *testing.T) {
	t.Run("old samples leave the window", func(t *testing.T) {
		e := New(5*time.Second, DefaultThresholds())
		run(t, e, memSeries(0, 100, 0.3))
		if n := len(e.samples); n != 6 {
			t.Fatalf("len = %d, want 6 (samples within 5s inclusive)", n)
		}
		if first := e.samples[0].t; !first.Equal(epoch.Add(94 * time.Second)) {
			t.Fatalf("oldest sample at %v", first)
		}
	})
	t.Run("hard cap bounds a huge window", func(t *testing.T) {
		e := New(24*time.Hour, DefaultThresholds())
		run(t, e, memSeries(0, 3*MaxSamples, 0.3))
		if len(e.samples) != MaxSamples || cap(e.samples) != MaxSamples {
			t.Fatalf("len=%d cap=%d, want %d", len(e.samples), cap(e.samples), MaxSamples)
		}
		if cap(e.scratch) != MaxSamples {
			t.Fatalf("scratch cap = %d", cap(e.scratch))
		}
		if last := e.samples[MaxSamples-1].t; !last.Equal(epoch.Add(time.Duration(3*MaxSamples-1) * time.Second)) {
			t.Fatalf("newest sample at %v", last)
		}
	})
	t.Run("caller missing slice is not retained", func(t *testing.T) {
		e := New(5*time.Second, DefaultThresholds())
		missing := []domain.Metric{domain.MetricCPU}
		s := sig{sec: 0, mem: 0.3, missing: missing}.sample()
		e.Observe(s)
		missing[0] = domain.MetricMemory
		if !e.samples[0].hasMem || e.samples[0].hasCPU {
			t.Fatalf("stored presence changed with caller slice: %+v", e.samples[0])
		}
	})
}

func TestObserveEdgeCases(t *testing.T) {
	t.Run("out of order sample is ignored", func(t *testing.T) {
		e := New(5*time.Second, DefaultThresholds())
		run(t, e, memSeries(10, 4, 0.3))
		l, changed, _ := e.Observe(sig{sec: 1, mem: 0.99}.sample())
		if l != domain.PressureNormal || changed || len(e.samples) != 4 {
			t.Fatalf("level=%v changed=%v len=%d", l, changed, len(e.samples))
		}
	})
	t.Run("defaults for non-positive config", func(t *testing.T) {
		th := DefaultThresholds()
		th.EscalateSamples = 0
		e := New(0, th)
		if e.window != DefaultWindow || e.dwell != 2*DefaultWindow || e.th.EscalateSamples != 1 {
			t.Fatalf("window=%v dwell=%v escalate=%d", e.window, e.dwell, e.th.EscalateSamples)
		}
		if l, changed, _ := e.Observe(sig{sec: 0, mem: 0.3}.sample()); l != domain.PressureNormal || !changed {
			t.Fatalf("escalate=1: level=%v changed=%v", l, changed)
		}
	})
	t.Run("initial state", func(t *testing.T) {
		e := New(time.Second, DefaultThresholds())
		if e.Level() != domain.PressureUnknown || e.reason != "no_samples" {
			t.Fatalf("level=%v reason=%q", e.Level(), e.reason)
		}
	})
	t.Run("zero memory total counts as missing memory", func(t *testing.T) {
		e := New(time.Second, DefaultThresholds())
		s := domain.HostSample{Time: epoch, Missing: []domain.Metric{domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}}
		if l, changed, reason := e.Observe(s); l != domain.PressureUnknown || changed || reason != "no_signals" {
			t.Fatalf("level=%v changed=%v reason=%q", l, changed, reason)
		}
	})
	t.Run("memory available above total is clamped", func(t *testing.T) {
		e := New(time.Second, DefaultThresholds())
		s := domain.HostSample{Time: epoch, MemTotal: 100, MemAvailable: 200}
		e.Observe(s)
		if e.samples[0].mem != 0 {
			t.Fatalf("mem = %v", e.samples[0].mem)
		}
	})
}

func TestExitThreshold(t *testing.T) {
	tests := []struct {
		name          string
		margin, ratio float64
		enter         float64
		rate          bool
		want          float64
	}{
		{"absolute margin", 0.05, 0.5, 0.9, false, 0.85},
		{"rate ratio", 0.05, 0.5, 8, true, 4},
		{"margin larger than threshold falls back", 0.5, 0.5, 0.3, false, 0.3},
		{"zero ratio falls back", 0.05, 0, 8, true, 8},
		{"ratio above one falls back", 0.05, 2, 8, true, 8},
		{"negative margin falls back", -0.1, 0.5, 0.9, false, 0.9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := DefaultThresholds()
			th.ExitMargin, th.SwapExitRatio = tc.margin, tc.ratio
			e := New(time.Second, th)
			if got := e.exitThreshold(tc.enter, tc.rate); got < tc.want-1e-9 || got > tc.want+1e-9 {
				t.Fatalf("exitThreshold = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatThreshold(t *testing.T) {
	for in, want := range map[float64]string{0.9 - 0.05: "0.85", 8 * mib: "8388608", 1.5: "1.5"} {
		if got := formatThreshold(in); got != want {
			t.Errorf("formatThreshold(%v) = %q, want %q", in, got, want)
		}
	}
}
