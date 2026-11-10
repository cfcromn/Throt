package platform

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestOtherStats(t *testing.T) {
	const own, root = 501, 0
	other := func(p domain.ProcessSample) bool { return p.UID != own }
	sample := func(cpu time.Duration, rss uint64) []domain.ProcessSample {
		return []domain.ProcessSample{
			{PID: 1, UID: root, Start: time.Unix(10, 0), CPUTime: cpu, RSS: rss},
			{PID: 2, UID: own, Start: time.Unix(10, 0), CPUTime: 7 * time.Second, RSS: 70},
		}
	}
	var o otherStats
	type step struct {
		measured bool
		cpu      time.Duration // what ps would print, if it ran
		rss      uint64
		wantCPU  time.Duration
		wantRSS  uint64
		wantDue  bool
	}
	for i, s := range []step{
		// First seen in a ps pass: the cumulative time, as before.
		{measured: true, cpu: 100 * time.Second, rss: 10, wantCPU: 100 * time.Second, wantRSS: 10},
		// Two passes in a row: the delta between them.
		{measured: true, cpu: 101 * time.Second, rss: 11, wantCPU: 101 * time.Second, wantRSS: 11},
		// Not measured: carried, so its rate reads 0.
		{measured: false, cpu: 999 * time.Second, rss: 99, wantCPU: 101 * time.Second, wantRSS: 11},
		// The first pass after a gap credits nothing for the gap.
		{measured: true, cpu: 150 * time.Second, rss: 15, wantCPU: 101 * time.Second, wantRSS: 15},
		{measured: true, cpu: 152 * time.Second, rss: 16, wantCPU: 103 * time.Second, wantRSS: 16},
		// A CPU time that went back is not credited either.
		{measured: true, cpu: 140 * time.Second, rss: 16, wantCPU: 103 * time.Second, wantRSS: 16},
	} {
		procs := sample(s.cpu, s.rss)
		o.observe(procs, other, s.measured)
		if procs[0].CPUTime != s.wantCPU || procs[0].RSS != s.wantRSS {
			t.Errorf("step %d: other = %v, %d; want %v, %d", i, procs[0].CPUTime, procs[0].RSS, s.wantCPU, s.wantRSS)
		}
		if procs[1].CPUTime != 7*time.Second || procs[1].RSS != 70 {
			t.Errorf("step %d: own process changed: %+v", i, procs[1])
		}
	}

	// A process first seen while not measured reads 0 until two passes in
	// a row measure it.
	var n otherStats
	if !n.due() {
		t.Fatal("first sample not due")
	}
	procs := sample(5*time.Second, 50)
	n.observe(procs, other, false)
	if procs[0].CPUTime != 0 || procs[0].RSS != 0 {
		t.Errorf("unmeasured new process = %+v", procs[0])
	}
	for range othersRefresh - 2 {
		n.observe(sample(0, 0), other, false)
	}
	if !n.due() {
		t.Errorf("not due after %d unmeasured samples", othersRefresh-1)
	}
	procs = sample(6*time.Second, 60)
	n.observe(procs, other, true)
	if procs[0].CPUTime != 0 || procs[0].RSS != 60 || n.due() {
		t.Errorf("first measured = %+v, due %v", procs[0], n.due())
	}
	// A reused PID is a new process.
	procs = []domain.ProcessSample{{PID: 1, UID: root, Start: time.Unix(20, 0), CPUTime: time.Second, RSS: 1}}
	n.observe(procs, other, true)
	if procs[0].CPUTime != time.Second || len(n.last) != 1 {
		t.Errorf("reused pid = %+v, %d carried", procs[0], len(n.last))
	}
}
