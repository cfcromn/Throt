package pressure

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestThroughputOnly(t *testing.T) {
	N, E, H := domain.PressureNormal, domain.PressureElevated, domain.PressureHigh
	busy := func(g sig) sig { g.cpu = 0.95; return g }
	tests := []struct {
		name  string
		f     func(i int) sig
		level domain.PressureLevel
		want  bool
	}{
		{"calm", func(int) sig { return sig{cpu: 0.3} }, N, false},
		{"cpu only", func(int) sig { return busy(sig{}) }, E, true},
		{"load only", func(int) sig { return sig{load: 1.2} }, E, true},
		{"cpu psi some only", func(int) sig { return sig{cpuStall: stall(0.6, 0)} }, E, true},
		{"cpu and load", func(int) sig { return busy(sig{load: 1.4}) }, E, true},
		{"cpu, memory well below exit", func(int) sig { return busy(sig{mem: 0.70}) }, E, true},
		{"cpu, latency below exit", func(int) sig { return busy(sig{lat: 15 * time.Millisecond}) }, E, true},
		// A kernel signal replaces memory used, so a full page cache does not
		// disqualify CPU-only pressure.
		{"cpu, full memory, kernel normal", func(int) sig { return busy(sig{mem: 0.88, kp: domain.MemoryPressureNormal}) }, E, true},
		{"cpu, memory psi calm", func(int) sig { return busy(sig{mem: 0.88, memStall: stall(0.01, 0)}) }, E, true},

		{"memory only", func(int) sig { return sig{mem: 0.85} }, E, false},
		{"cpu and memory used", func(int) sig { return busy(sig{mem: 0.85}) }, E, false},
		// Between exit (0.75) and enter (0.80): memory is not clear yet.
		{"cpu, memory between exit and enter", func(int) sig { return busy(sig{mem: 0.77}) }, E, false},
		{"cpu and swap growth", func(i int) sig { return busy(sig{swapMiB: 2 * float64(i)}) }, E, false},
		{"cpu and latency", func(int) sig { return busy(sig{lat: 30 * time.Millisecond}) }, E, false},
		{"cpu, latency between exit and enter", func(int) sig { return busy(sig{lat: 20 * time.Millisecond}) }, E, false},
		{"cpu and kernel warning", func(int) sig { return busy(sig{kp: domain.MemoryPressureWarning}) }, E, false},
		{"cpu and memory psi some", func(int) sig { return busy(sig{memStall: stall(0.15, 0)}) }, E, false},
		{"cpu and io psi full", func(int) sig { return busy(sig{ioStall: stall(0.3, 0.25)}) }, E, false},
		{"cpu and high latency", func(int) sig { return busy(sig{lat: 150 * time.Millisecond}) }, H, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := New(30*time.Second, DefaultThresholds())
			for _, g := range series(0, 40, tc.f) {
				e.Observe(g.sample())
			}
			if e.Level() != tc.level || e.ThroughputOnly() != tc.want {
				t.Fatalf("level %v, ThroughputOnly %v; want %v, %v", e.Level(), e.ThroughputOnly(), tc.level, tc.want)
			}
		})
	}
}

// Memory raises pressure while the CPU is saturated; once memory clears its
// exit threshold, CPU alone holds elevated and the flag turns on, with the
// reason string untouched.
func TestThroughputOnlyFollowsMemory(t *testing.T) {
	e := New(30*time.Second, DefaultThresholds())
	feed := func(start, n int, mem float64) {
		for _, g := range series(start, n, func(int) sig { return sig{cpu: 0.95, mem: mem} }) {
			e.Observe(g.sample())
		}
	}
	feed(0, 40, 0.85)
	if e.Level() != domain.PressureElevated || e.ThroughputOnly() {
		t.Fatalf("memory and cpu: level %v, ThroughputOnly %v; want elevated, false", e.Level(), e.ThroughputOnly())
	}
	_, _, before := e.Observe(sig{sec: 40, cpu: 0.95, mem: 0.85}.sample())
	feed(41, 40, 0.60)
	_, _, after := e.Observe(sig{sec: 81, cpu: 0.95, mem: 0.60}.sample())
	if e.Level() != domain.PressureElevated || !e.ThroughputOnly() {
		t.Fatalf("cpu only: level %v, ThroughputOnly %v; want elevated, true", e.Level(), e.ThroughputOnly())
	}
	if before != after || before != "memory_used>=0.8" {
		t.Fatalf("reason changed from %q to %q", before, after)
	}
	feed(82, 40, 0.85)
	if e.ThroughputOnly() {
		t.Fatal("ThroughputOnly stayed on after memory came back")
	}
}

func TestThroughputOnlyIgnoresStaleSample(t *testing.T) {
	e := New(30*time.Second, DefaultThresholds())
	for _, g := range series(0, 10, func(int) sig { return sig{cpu: 0.95} }) {
		e.Observe(g.sample())
	}
	if !e.ThroughputOnly() {
		t.Fatal("want ThroughputOnly after sustained cpu")
	}
	e.Observe(sig{sec: 1, cpu: 0.95, mem: 0.9}.sample()) // older: ignored
	if !e.ThroughputOnly() {
		t.Fatal("a stale sample changed ThroughputOnly")
	}
}

func TestMemoryHeadroom(t *testing.T) {
	// sig.sample uses a total of 1,000,000 bytes; the elevated exit
	// threshold for memory used is 0.75.
	noMem := []domain.Metric{domain.MetricMemory}
	disabled := DefaultThresholds()
	disabled.Elevated.MemoryUsed = 0
	tests := []struct {
		name string
		th   Thresholds
		sigs []sig
		want uint64
	}{
		{"no samples", DefaultThresholds(), nil, 0},
		{"steady", DefaultThresholds(), memSeries(0, 5, 0.5), 250_000},
		{"growing counts at its fullest", DefaultThresholds(), concat(memSeries(0, 5, 0.25), memSeries(5, 2, 0.5)), 250_000},
		{"at the exit threshold", DefaultThresholds(), memSeries(0, 5, 0.75), 0},
		{"above it", DefaultThresholds(), memSeries(0, 5, 0.9), 0},
		{"a full sample left the window", DefaultThresholds(), concat(memSeries(0, 1, 0.5), memSeries(40, 2, 0.25)), 500_000},
		{"memory missing", DefaultThresholds(), []sig{{sec: 0, mem: 0.25, missing: noMem}}, 0},
		{"threshold disabled", disabled, memSeries(0, 5, 0.25), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(30*time.Second, tt.th)
			for _, g := range tt.sigs {
				e.Observe(g.sample())
			}
			if got := e.MemoryHeadroom(); got != tt.want {
				t.Fatalf("MemoryHeadroom = %d, want %d", got, tt.want)
			}
		})
	}
}
