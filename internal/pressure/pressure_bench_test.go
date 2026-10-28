package pressure

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// BenchmarkPressureObserve feeds a full default window of 2s samples, the
// daemon's steady state, with every signal present.
func BenchmarkPressureObserve(b *testing.B) {
	e := New(DefaultWindow, DefaultThresholds())
	stall := &domain.Stall{Some: 0.01, Full: 0.001}
	s := domain.HostSample{
		Time: epoch, CPUUsage: 0.4, Load1: 0.5, MemTotal: 16 << 30, MemAvailable: 8 << 30,
		SwapTotal: 4 << 30, SwapUsed: 1 << 30, Latency: 2 * time.Millisecond,
		MemoryPressure: domain.MemoryPressureNormal, MemoryStall: stall, IOStall: stall, CPUStall: stall,
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		s.Time = s.Time.Add(2 * time.Second)
		s.CPUUsage = float64(i%7) / 10
		e.Observe(s)
	}
}
