// Package pressure turns a stream of host samples into a stable pressure
// level using a rolling window and hysteresis. It is deterministic: it makes
// no OS calls, starts no goroutines, and takes time only from sample.Time.
package pressure

import (
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// MaxSamples caps the rolling window so a misconfigured window cannot grow
// memory without bound. The oldest samples are dropped first.
const MaxSamples = 256

// DefaultWindow is used when New is given a non-positive window. It is
// also the shortest window WindowFor returns.
const DefaultWindow = 30 * time.Second

// WindowSamples is how many sample intervals WindowFor fits in a window.
const WindowSamples = 10

// WindowFor returns the window for a sample interval: DefaultWindow, or
// WindowSamples intervals when that is longer, so the window's medians
// always come from several samples.
func WindowFor(interval time.Duration) time.Duration {
	return max(DefaultWindow, WindowSamples*interval)
}

// LevelThresholds are the enter thresholds for one pressure level. A signal
// reaches the level when its window statistic is >= the threshold. A zero
// threshold means the signal never triggers that level on its own.
type LevelThresholds struct {
	// KernelMemory is the kernel memory-pressure verdict (see
	// domain.MetricMemoryPressure) whose window median reaches this level.
	// It is ignored for the critical level: the kernel verdict alone never
	// makes pressure critical.
	KernelMemory domain.MemoryPressure
	// MemoryStallSome and MemoryStallFull are the window medians of the
	// Linux memory PSI some and full shares (see domain.MetricMemoryStall),
	// 0..1.
	MemoryStallSome float64
	MemoryStallFull float64
	// IOStallFull is the median I/O PSI full share, 0..1.
	IOStallFull float64
	// CPUStallSome is the median CPU PSI some share, 0..1.
	CPUStallSome float64
	// MemoryUsed is the median of 1 - MemAvailable/MemTotal, 0..1.
	MemoryUsed float64
	// SwapGrowth is swap-used growth across the window in bytes per second.
	SwapGrowth float64
	// CPU is the median busy fraction of all CPUs, 0..1.
	CPU float64
	// Load is the median 1-minute load average per logical CPU.
	Load float64
	// Latency is the median foreground timer lateness (see
	// domain.MetricLatency).
	Latency time.Duration
}

// Thresholds configures the engine. All fields are policy; none are derived.
type Thresholds struct {
	Elevated LevelThresholds
	High     LevelThresholds
	Critical LevelThresholds
	// MemoryUsedWhenKernelSignal keeps evaluating MemoryUsed while a kernel
	// memory signal is present in the window: the macOS memory-pressure
	// verdict or Linux memory PSI. By default (false) the kernel signal
	// takes precedence and MemoryUsed is ignored then: both kernels keep
	// memory deliberately full (the macOS compressor, file and page cache),
	// so the used fraction can sit at 80-90% on an idle machine and would
	// hold pressure at elevated forever, while the kernel measures what
	// actually hurts. Without a kernel signal in the window (an old kernel,
	// psi=0, or a failed read) MemoryUsed applies as usual.
	MemoryUsedWhenKernelSignal bool
	// ExitMargin is subtracted from the MemoryUsed, CPU and Load enter
	// thresholds to form exit thresholds, so a value hovering at a boundary
	// cannot flap the level.
	ExitMargin float64
	// SwapExitRatio scales a SwapGrowth enter threshold to its exit
	// threshold (a rate has no natural absolute margin).
	SwapExitRatio float64
	// LatencyExitRatio scales a Latency enter threshold to its exit
	// threshold. Lateness is noisy and has no natural absolute margin.
	LatencyExitRatio float64
	// StallExitRatio scales a PSI enter threshold to its exit threshold.
	// Stall thresholds are as small as 5%, where ExitMargin would leave no
	// margin at all.
	StallExitRatio float64
	// EscalateSamples is how many consecutive samples the raw level must
	// stay above the current level before escalating. Values < 1 mean 1.
	EscalateSamples int
	// RecoveryDwell is how long every signal must stay below the current
	// level's exit thresholds before de-escalating. Zero means 2x window.
	RecoveryDwell time.Duration
}

// DefaultThresholds returns defaults tuned for a developer laptop:
//
//   - kernel memory pressure (macOS): warning → elevated, critical → high,
//     never critical on its own. When present it replaces memory used (see
//     MemoryUsedWhenKernelSignal). The verdict is ordinal, so its exit
//     threshold is half a step below the enter one: entering needs the
//     window median at the verdict (a majority of samples), holding needs
//     at least half the window there.
//   - memory PSI (Linux, avg10 shares): some >= 0.10 → elevated, full >=
//     0.05 → high, full >= 0.20 → critical. "Some" means at least one task
//     waited on memory (reclaim, refaults); "full" means every non-idle
//     task did at once, which is time the whole machine lost. Like the
//     macOS verdict it replaces memory used while present, because page
//     cache keeps Linux memory nearly full too.
//   - I/O PSI full: elevated 0.20, high 0.50, never critical. I/O stalls
//     slow work but, unlike memory thrashing, do not escalate on their own.
//   - CPU PSI some: elevated 0.50 only, consistent with CPU capping at
//     elevated.
//   - memory used: elevated 0.80, high 0.90, critical 0.95. Memory is the
//     signal that most directly predicts an unresponsive machine.
//   - swap growth: elevated 1 MiB/s, high 8 MiB/s, critical 32 MiB/s.
//     Sustained swapping is the clearest sign of real memory pressure.
//   - CPU: elevated 0.85, high 0.95, never critical. Saturated CPU during a
//     build is normal and slows work rather than freezing the machine.
//   - load per CPU: elevated 1.0, high 1.5, critical 3.0. A deep run queue
//     is what makes the foreground feel sluggish.
//   - latency: elevated 25ms, high 100ms, never critical. Timer lateness is
//     a direct measure of responsiveness, but like CPU it slows rather than
//     freezes the machine.
//   - exit margin 0.05, swap exit ratio 0.5, latency exit ratio 0.75,
//     stall exit ratio 0.5, escalate after 3 consecutive samples, recover
//     after 2x the window.
func DefaultThresholds() Thresholds {
	const mib = 1 << 20
	return Thresholds{
		Elevated: LevelThresholds{
			KernelMemory: domain.MemoryPressureWarning, MemoryStallSome: 0.10, IOStallFull: 0.20, CPUStallSome: 0.50,
			MemoryUsed: 0.80, SwapGrowth: 1 * mib, CPU: 0.85, Load: 1.0, Latency: 25 * time.Millisecond,
		},
		// CPU and load stop at elevated: builds saturating every core is the
		// throughput Throt exists to protect, not a problem by itself. Their
		// effect on the person at the keyboard is measured directly by the
		// latency probe, which (with memory and swap) drives high/critical.
		High: LevelThresholds{
			KernelMemory: domain.MemoryPressureCritical, MemoryStallFull: 0.05, IOStallFull: 0.50,
			MemoryUsed: 0.90, SwapGrowth: 8 * mib, Latency: 100 * time.Millisecond,
		},
		Critical:         LevelThresholds{MemoryStallFull: 0.20, MemoryUsed: 0.95, SwapGrowth: 32 * mib},
		ExitMargin:       0.05,
		SwapExitRatio:    0.5,
		LatencyExitRatio: 0.75,
		StallExitRatio:   0.5,
		EscalateSamples:  3,
	}
}

// point is the part of a sample the engine retains. It copies values so the
// engine never holds on to the caller's Missing slice.
type point struct {
	t              time.Time
	cpu, load, mem float64
	// commit is the share of RAM and swap together in use.
	commit                                       float64
	latency                                      time.Duration
	swap                                         uint64
	memTotal                                     uint64
	kernel                                       domain.MemoryPressure
	memStall, ioStall, cpuStall                  domain.Stall
	hasCPU, hasLoad, hasMem, hasSwap, hasLatency bool
	hasKernel                                    bool
	hasMemStall, hasIOStall, hasCPUStall         bool
}

// stats are the window statistics evaluated against thresholds.
type stats struct {
	// latency is in nanoseconds; kernel is the median verdict ordinal.
	mem, swap, cpu, load, latency, kernel        float64
	commit                                       float64
	memSome, memFull, ioFull, cpuSome            float64
	hasMem, hasSwap, hasCPU, hasLoad, hasLatency bool
	hasKernel                                    bool
	hasMemStall, hasIOStall, hasCPUStall         bool
	// has reports whether any signal is present in the window.
	has bool
}

// Engine is a pressure state machine. It is not safe for concurrent use; the
// owner serializes calls.
type Engine struct {
	window  time.Duration
	dwell   time.Duration
	th      Thresholds
	samples []point
	scratch []float64

	level  domain.PressureLevel
	reason string
	// throughputOnly is ThroughputOnly as of the latest sample.
	throughputOnly bool

	// Escalation: consecutive samples whose raw level exceeds level, and the
	// lowest such raw level (the level that was actually sustained).
	upCount  int
	upLevel  domain.PressureLevel
	upReason string

	// Recovery: when the hold level first fell below level, and the highest
	// hold level seen since then.
	downSince  time.Time
	downLevel  domain.PressureLevel
	downReason string
}

// New returns an engine starting at PressureUnknown. A non-positive window
// means DefaultWindow.
func New(window time.Duration, th Thresholds) *Engine {
	if window <= 0 {
		window = DefaultWindow
	}
	th.EscalateSamples = max(th.EscalateSamples, 1)
	dwell := th.RecoveryDwell
	if dwell <= 0 {
		dwell = 2 * window
	}
	return &Engine{
		window:  window,
		dwell:   dwell,
		th:      th,
		samples: make([]point, 0, MaxSamples),
		scratch: make([]float64, 0, MaxSamples),
		level:   domain.PressureUnknown,
		reason:  "no_samples",
	}
}

// Window returns the rolling window.
func (e *Engine) Window() time.Duration { return e.window }

// Level returns the current stable level.
func (e *Engine) Level() domain.PressureLevel { return e.level }

// ThroughputOnly reports whether the current level is elevated by
// throughput signals only: at least one of CPU, load and CPU PSI some holds
// elevated (is at or above its elevated exit threshold), no signal reaches
// high on its enter threshold, and every other signal (memory used, swap
// growth, latency, the kernel memory verdict, memory and I/O PSI) is below
// its elevated exit threshold. Exit thresholds make the test conservative:
// a memory signal that is recovering but not yet clear still disqualifies.
// It is false at every other level. It does not change Observe's reason.
func (e *Engine) ThroughputOnly() bool { return e.throughputOnly }

// MemoryHeadroom returns how many bytes of memory the host can still fill
// before memory used reaches its elevated exit threshold, in the fullest
// sample of the window, so a working set that is still growing counts at its
// latest size. It is 0 when no sample in the window reports memory, when
// memory used is at or above that threshold, or when the elevated memory
// threshold is disabled.
func (e *Engine) MemoryHeadroom() uint64 {
	enter := e.th.Elevated.MemoryUsed
	if enter <= 0 {
		return 0
	}
	exit := e.exitThreshold(enter, kindFraction)
	var room uint64
	seen := false
	for _, p := range e.samples {
		if !p.hasMem {
			continue
		}
		r := uint64(0)
		if p.mem < exit {
			r = uint64((exit - p.mem) * float64(p.memTotal))
		}
		if !seen || r < room {
			room, seen = r, true
		}
	}
	return room
}

// Observe adds a sample and returns the current stable level, whether this
// sample changed it, and a short machine-readable reason for the level
// (e.g. "memory_used>=0.9"). Samples older than the newest one are ignored.
func (e *Engine) Observe(s domain.HostSample) (level domain.PressureLevel, changed bool, reason string) {
	if n := len(e.samples); n > 0 && s.Time.Before(e.samples[n-1].t) {
		return e.level, false, e.reason
	} else if n > 0 && s.Time.Sub(e.samples[n-1].t) > e.window {
		// After a gap longer than the window, s alone is the window: a
		// recovery that began before the gap starts its dwell again, so
		// one sample cannot de-escalate.
		e.resetDown()
	}
	e.add(s)
	st := e.stats()
	level, changed, reason = e.step(st, s.Time)
	e.throughputOnly = e.classifyThroughputOnly(st)
	return level, changed, reason
}

// classifyThroughputOnly computes ThroughputOnly for st at the current
// stable level.
func (e *Engine) classifyThroughputOnly(st stats) bool {
	if e.level != domain.PressureElevated {
		return false
	}
	if raw, _ := e.classify(st, false, false); raw > domain.PressureElevated {
		return false
	}
	if other, _ := e.classify(st, true, true); other > domain.PressureNormal {
		return false
	}
	hold, _ := e.classify(st, true, false)
	return hold >= domain.PressureElevated
}

// step advances the state machine with the window statistics of a new
// sample taken at now.
func (e *Engine) step(st stats, now time.Time) (domain.PressureLevel, bool, string) {
	if !st.has {
		if e.level == domain.PressureUnknown {
			e.reason = "no_signals"
			return e.level, false, e.reason
		}
		return e.set(domain.PressureUnknown, "no_signals")
	}

	enter, enterReason := e.classify(st, false, false)
	if enter > e.level {
		e.resetDown()
		if e.upCount == 0 || enter < e.upLevel {
			e.upLevel, e.upReason = enter, enterReason
		}
		e.upCount++
		if e.upCount >= e.th.EscalateSamples {
			return e.set(e.upLevel, e.upReason)
		}
		return e.level, false, e.reason
	}
	e.upCount = 0

	if e.level <= domain.PressureNormal {
		return e.level, false, e.reason
	}
	hold, holdReason := e.classify(st, true, false)
	if hold >= e.level {
		e.resetDown()
		return e.level, false, e.reason
	}
	if e.downSince.IsZero() {
		e.downSince, e.downLevel, e.downReason = now, hold, holdReason
	} else if hold > e.downLevel {
		e.downLevel, e.downReason = hold, holdReason
	}
	if now.Sub(e.downSince) >= e.dwell {
		return e.set(e.downLevel, e.downReason)
	}
	return e.level, false, e.reason
}

func (e *Engine) set(l domain.PressureLevel, reason string) (domain.PressureLevel, bool, string) {
	changed := l != e.level
	e.level, e.reason = l, reason
	e.upCount = 0
	e.resetDown()
	return l, changed, reason
}

func (e *Engine) resetDown() { e.downSince = time.Time{} }

// add appends s and prunes samples outside the window or over MaxSamples.
func (e *Engine) add(s domain.HostSample) {
	p := point{
		t:       s.Time,
		hasCPU:  s.Has(domain.MetricCPU),
		hasLoad: s.Has(domain.MetricLoad),
		hasMem:  s.Has(domain.MetricMemory) && s.MemTotal > 0,
		hasSwap: s.Has(domain.MetricSwap),
		cpu:     s.CPUUsage,
		load:    s.Load1,
		swap:    s.SwapUsed,
		latency: s.Latency,
	}
	if p.hasMem {
		p.memTotal = s.MemTotal
	}
	p.hasLatency = s.Has(domain.MetricLatency)
	p.kernel, p.hasKernel = s.MemoryPressure, s.HasMemoryPressure()
	p.memStall, p.hasMemStall = s.StallFor(domain.MetricMemoryStall)
	p.ioStall, p.hasIOStall = s.StallFor(domain.MetricIOStall)
	p.cpuStall, p.hasCPUStall = s.StallFor(domain.MetricCPUStall)

	if p.hasMem {
		avail := min(s.MemAvailable, s.MemTotal)
		p.mem = 1 - float64(avail)/float64(s.MemTotal)
		p.commit = p.mem
		if p.hasSwap && s.SwapTotal > 0 {
			used := float64(s.MemTotal-avail) + float64(min(s.SwapUsed, s.SwapTotal))
			p.commit = used / (float64(s.MemTotal) + float64(s.SwapTotal))
		}
	}
	if len(e.samples) == MaxSamples {
		e.samples = append(e.samples[:0], e.samples[1:]...)
	}
	e.samples = append(e.samples, p)

	cut := 0
	for cut < len(e.samples) && s.Time.Sub(e.samples[cut].t) > e.window {
		cut++
	}
	if cut > 0 {
		e.samples = append(e.samples[:0], e.samples[cut:]...)
	}
}

// stats computes medians for level-like signals and for the swap growth rate
// between adjacent samples that report swap, so one jump in swap usage does
// not dominate the whole window.
func (e *Engine) stats() stats {
	var st stats
	st.mem, st.hasMem = e.median(func(p point) (float64, bool) { return p.mem, p.hasMem })
	st.commit, _ = e.median(func(p point) (float64, bool) { return p.commit, p.hasMem })
	st.cpu, st.hasCPU = e.median(func(p point) (float64, bool) { return p.cpu, p.hasCPU })
	st.load, st.hasLoad = e.median(func(p point) (float64, bool) { return p.load, p.hasLoad })
	st.latency, st.hasLatency = e.median(func(p point) (float64, bool) { return float64(p.latency), p.hasLatency })
	st.kernel, st.hasKernel = e.median(func(p point) (float64, bool) { return float64(p.kernel), p.hasKernel })
	st.memSome, st.hasMemStall = e.median(func(p point) (float64, bool) { return p.memStall.Some, p.hasMemStall })
	st.memFull, _ = e.median(func(p point) (float64, bool) { return p.memStall.Full, p.hasMemStall })
	st.ioFull, st.hasIOStall = e.median(func(p point) (float64, bool) { return p.ioStall.Full, p.hasIOStall })
	st.cpuSome, st.hasCPUStall = e.median(func(p point) (float64, bool) { return p.cpuStall.Some, p.hasCPUStall })

	// Rates go in e.scratch, which median also uses; compute them last.
	e.scratch = e.scratch[:0]
	prev := -1
	for i, p := range e.samples {
		if !p.hasSwap {
			continue
		}
		st.hasSwap = true
		if prev >= 0 {
			a := e.samples[prev]
			if dt := p.t.Sub(a.t).Seconds(); dt > 0 {
				rate := 0.0
				if p.swap > a.swap {
					rate = float64(p.swap-a.swap) / dt
				}
				e.scratch = append(e.scratch, rate)
			}
		}
		prev = i
	}
	st.swap = medianOf(e.scratch)
	st.has = st.hasMem || st.hasCPU || st.hasLoad || st.hasSwap || st.hasLatency || st.hasKernel ||
		st.hasMemStall || st.hasIOStall || st.hasCPUStall
	return st
}

func (e *Engine) median(get func(point) (float64, bool)) (float64, bool) {
	e.scratch = e.scratch[:0]
	for _, p := range e.samples {
		if v, ok := get(p); ok {
			e.scratch = append(e.scratch, v)
		}
	}
	if len(e.scratch) == 0 {
		return 0, false
	}
	return medianOf(e.scratch), true
}

// medianOf sorts vals in place and returns their median, or 0 if empty.
func medianOf(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	slices.Sort(vals)
	if n%2 == 1 {
		return vals[n/2]
	}
	return (vals[n/2-1] + vals[n/2]) / 2
}

// classify returns the highest level any present signal reaches, using enter
// thresholds or, when exit is true, the lower exit thresholds. With
// skipThroughput it ignores the throughput signals (CPU, load and CPU PSI
// some), which saturate during any build without hurting the machine.
//
// A kernel memory signal in the window (the macOS verdict or Linux memory
// PSI) takes precedence over memory used unless MemoryUsedWhenKernelSignal
// is set. With memory PSI, near-exhaustion still counts toward critical:
// without swap, memory can fill with little reclaim stall until the OOM
// killer acts. It is measured as RAM and swap together in use
// (memory_and_swap_used, against the critical MemoryUsed threshold), so
// RAM full of idle pages while swap has room, such as workloads the
// scheduler paused, is left to PSI: counting RAM alone would hold critical,
// and block every resume, until the paused work exits. Without swap it is
// memory used. The macOS verdict never reaches critical on its own, and
// memory used does not count beside it: macOS grows swap on demand, so
// there is no fixed size to measure exhaustion against, and critical
// needs swap growth.
func (e *Engine) classify(st stats, exit, skipThroughput bool) (domain.PressureLevel, string) {
	kernelMem := st.hasKernel || st.hasMemStall
	useMem := st.hasMem && (!kernelMem || e.th.MemoryUsedWhenKernelSignal)
	exhaustion := st.hasMem && st.hasMemStall && !useMem
	levels := [...]struct {
		l  domain.PressureLevel
		th LevelThresholds
	}{
		{domain.PressureCritical, e.th.Critical},
		{domain.PressureHigh, e.th.High},
		{domain.PressureElevated, e.th.Elevated},
	}
	for _, lv := range levels {
		sigs := [...]struct {
			name       string
			v, t       float64
			present    bool
			kind       sigKind
			throughput bool
		}{
			{"kernel_memory_pressure", st.kernel, float64(lv.th.KernelMemory), st.hasKernel && lv.l != domain.PressureCritical, kindVerdict, false},
			{"memory_stall_full", st.memFull, lv.th.MemoryStallFull, st.hasMemStall, kindStall, false},
			{"memory_stall_some", st.memSome, lv.th.MemoryStallSome, st.hasMemStall, kindStall, false},
			{"memory_used", st.mem, lv.th.MemoryUsed, useMem, kindFraction, false},
			{"memory_and_swap_used", st.commit, lv.th.MemoryUsed, exhaustion && lv.l == domain.PressureCritical, kindFraction, false},
			{"swap_growth", st.swap, lv.th.SwapGrowth, st.hasSwap, kindRate, false},
			{"io_stall_full", st.ioFull, lv.th.IOStallFull, st.hasIOStall, kindStall, false},
			{"load", st.load, lv.th.Load, st.hasLoad, kindFraction, true},
			{"latency", st.latency, float64(lv.th.Latency), st.hasLatency, kindDuration, false},
			{"cpu", st.cpu, lv.th.CPU, st.hasCPU, kindFraction, true},
			{"cpu_stall_some", st.cpuSome, lv.th.CPUStallSome, st.hasCPUStall, kindStall, true},
		}
		for _, sg := range sigs {
			if !sg.present || sg.t <= 0 || (skipThroughput && sg.throughput) {
				continue
			}
			t := sg.t
			if exit {
				t = e.exitThreshold(t, sg.kind)
			}
			if sg.v >= t {
				shown := t
				if sg.kind == kindVerdict {
					shown = sg.t // name the verdict, not the half-step exit
				}
				return lv.l, sg.name + ">=" + formatThreshold(shown, sg.kind)
			}
		}
	}
	if exit {
		return domain.PressureNormal, "recovered"
	}
	return domain.PressureNormal, "below_thresholds"
}

// sigKind selects how a signal's exit threshold is derived and how its
// threshold is formatted in a reason.
type sigKind int

const (
	// kindFraction uses the absolute ExitMargin.
	kindFraction sigKind = iota
	// kindRate (swap growth) uses SwapExitRatio.
	kindRate
	// kindDuration (latency, in nanoseconds) uses LatencyExitRatio.
	kindDuration
	// kindVerdict (kernel memory pressure, an ordinal) exits half a step
	// below its enter threshold: ExitMargin is sized for fractions and
	// would make a split window flap.
	kindVerdict
	// kindStall (PSI shares) uses StallExitRatio.
	kindStall
)

// exitThreshold lowers an enter threshold by the configured hysteresis. A
// degenerate configuration that would make the exit threshold non-positive
// (and so impossible to clear) falls back to the enter threshold.
func (e *Engine) exitThreshold(enter float64, kind sigKind) float64 {
	var t float64
	switch kind {
	case kindRate:
		t = enter * e.th.SwapExitRatio
	case kindDuration:
		t = enter * e.th.LatencyExitRatio
	case kindVerdict:
		t = enter - 0.5
	case kindStall:
		t = enter * e.th.StallExitRatio
	default:
		t = enter - e.th.ExitMargin
	}
	if t <= 0 || t > enter {
		return enter
	}
	return t
}

func formatThreshold(t float64, kind sigKind) string {
	switch kind {
	case kindDuration:
		return time.Duration(t).Round(time.Microsecond).String()
	case kindVerdict:
		return domain.MemoryPressure(math.Round(t)).String()
	}
	return strconv.FormatFloat(math.Round(t*1000)/1000, 'f', -1, 64)
}
