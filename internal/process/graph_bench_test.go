package process

import (
	"strconv"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/domain"
)

// benchTable returns n processes shaped like a developer machine: mostly
// idle helpers under a few parents, two agents each running a build with
// compilers below it. cpu is added to every process's CPU time.
func benchTable(n int, cpu time.Duration) []domain.ProcessSample {
	out := make([]domain.ProcessSample, 0, n)
	add := func(pid, ppid int, name string) {
		out = append(out, domain.ProcessSample{
			PID: pid, PPID: ppid, UID: 501, Name: name,
			Start:   epoch.Add(time.Duration(pid) * time.Millisecond),
			CPUTime: time.Duration(pid)*time.Millisecond + cpu,
			RSS:     64 << 20, State: domain.StateSleeping,
		})
	}
	add(1, 0, "launchd")
	for a := range 2 {
		base := 100 + a*10
		add(base, 1, "claude")
		add(base+1, base, "make")
		for c := range 4 {
			add(base+2+c, base+1, "cc1")
		}
	}
	for pid := 1000; len(out) < n; pid++ {
		add(pid, 1000+(pid-1000)/20*20, "helper-"+strconv.Itoa(pid%50))
	}
	return out
}

func BenchmarkGraphUpdate(b *testing.B) {
	g := NewGraph(agent.DefaultAdapters(), WithCPUCount(10), WithAgentUID(501))
	tables := [2][]domain.ProcessSample{benchTable(1500, 0), benchTable(1500, time.Second)}
	now := epoch
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		now = now.Add(2 * time.Second)
		g.Update(now, tables[i%2])
		_ = g.Snapshot()
	}
}
