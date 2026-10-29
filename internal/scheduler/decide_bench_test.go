package scheduler

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// BenchmarkDecide runs one enforce round per sample over 64 heavy
// workloads of 8 agents, half of them queued: a busy machine's steady
// state, at the daemon's 2s interval.
func BenchmarkDecide(b *testing.B) {
	p := DefaultPolicy()
	p.Mode, p.HeavyLimit, p.Adaptive = ModeEnforce, 32, true
	s := New(p)
	in := Input{Now: t0, Pressure: domain.PressureElevated, ThroughputOnly: true, MemoryHeadroom: 4 << 30}
	owners := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for pid := 1; pid <= 64; pid++ {
		v := wv(pid, owners[pid%len(owners)])
		v.RSS = 200 << 20
		in.Heavy = append(in.Heavy, v)
		if pid > 32 {
			in.Paused = append(in.Paused, v.Key)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		in.Now = in.Now.Add(2 * time.Second)
		s.Decide(in)
	}
}
