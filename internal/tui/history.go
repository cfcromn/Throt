package tui

import (
	"math"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// historyLen bounds the client-side metric history: one sample per poll.
const historyLen = 60

// histSample is what the TUI remembers of one poll's host sample.
type histSample struct {
	t        time.Time
	cpu, mem float64
	// swapRate is swap growth in bytes per second since the previous sample.
	swapRate float64
	latency  time.Duration
	swapUsed uint64

	hasCPU, hasMem, hasSwap, hasSwapRate, hasLatency bool
}

// history is a fixed-size ring of the newest historyLen samples. It is a
// value: copying a Model copies it, so older models never see new samples.
type history struct {
	buf  [historyLen]histSample
	next int // index the next sample is written to
	n    int // samples held, at most historyLen
}

// add records h, deriving swap growth from the previous sample. A poll that
// returns the same host sample again keeps the previous growth rate.
func (r *history) add(h domain.HostSample) {
	s := histSample{t: h.Time, latency: h.Latency}
	s.cpu, s.hasCPU = h.CPUUsage, h.Has(domain.MetricCPU)
	if h.Has(domain.MetricMemory) && h.MemTotal > 0 {
		s.mem, s.hasMem = ratio(h.MemTotal-min(h.MemAvailable, h.MemTotal), h.MemTotal), true
	}
	s.swapUsed, s.hasSwap = h.SwapUsed, h.Has(domain.MetricSwap)
	s.hasLatency = h.Has(domain.MetricLatency)
	if prev, ok := r.last(); ok && prev.hasSwap && s.hasSwap {
		switch dt := s.t.Sub(prev.t).Seconds(); {
		case dt > 0:
			s.hasSwapRate = true
			if s.swapUsed > prev.swapUsed {
				s.swapRate = float64(s.swapUsed-prev.swapUsed) / dt
			}
		case dt == 0:
			s.swapRate, s.hasSwapRate = prev.swapRate, prev.hasSwapRate
		}
	}
	r.buf[r.next] = s
	r.next = (r.next + 1) % historyLen
	r.n = min(r.n+1, historyLen)
}

func (r *history) last() (histSample, bool) {
	if r.n == 0 {
		return histSample{}, false
	}
	return r.buf[(r.next+historyLen-1)%historyLen], true
}

// series returns the newest n values of one signal, oldest first; ok[i]
// is false where the sample lacked it.
func (r *history) series(n int, get func(histSample) (float64, bool)) (vals []float64, ok []bool) {
	n = min(n, r.n)
	vals, ok = make([]float64, n), make([]bool, n)
	for i := range n {
		vals[i], ok[i] = get(r.buf[(r.next-n+i+historyLen)%historyLen])
	}
	return vals, ok
}

// sparkBlocks are the eight sparkline levels, lowest first.
var sparkBlocks = [...]rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Sparkline scale floors: below these the line stays low, so noise on an
// idle machine does not look like a spike.
const (
	latencyScaleFloor = float64(20 * time.Millisecond)
	swapScaleFloor    = float64(1 << 20) // bytes per second
)

// sparkline renders vals in exactly width cells, right-aligned so the
// newest sample is always in the last cell. A value maps to the level
// round(v/scale*7); scale <= 0 means "the largest value shown, at least
// floor". Missing samples are blank.
func sparkline(vals []float64, ok []bool, scale, floor float64, width int) string {
	if len(vals) > width {
		vals, ok = vals[len(vals)-width:], ok[len(ok)-width:]
	}
	if scale <= 0 {
		scale = floor
		for i, v := range vals {
			if ok[i] {
				scale = max(scale, v)
			}
		}
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", width-len(vals)))
	for i, v := range vals {
		if !ok[i] {
			b.WriteByte(' ')
			continue
		}
		level := int(math.Round(v / scale * float64(len(sparkBlocks)-1)))
		b.WriteRune(sparkBlocks[max(0, min(level, len(sparkBlocks)-1))])
	}
	return b.String()
}

func cpuOf(s histSample) (float64, bool)  { return s.cpu, s.hasCPU }
func memOf(s histSample) (float64, bool)  { return s.mem, s.hasMem }
func swapOf(s histSample) (float64, bool) { return s.swapRate, s.hasSwapRate }
func latencyOf(s histSample) (float64, bool) {
	return float64(s.latency), s.hasLatency
}
