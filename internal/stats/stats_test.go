package stats

import (
	"fmt"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

var t0 = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func key(pid, start int) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: time.Unix(int64(start), 0).UTC()}
}

func agent(label string, k domain.ProcessKey, cpu float64, rss uint64) process.AgentView {
	return process.AgentView{Key: k, Agent: label, Name: "claude", CPU: cpu, RSS: rss}
}

func heavy(k domain.ProcessKey, owner string, ownerPID int, cpu float64, rss uint64) process.WorkloadView {
	return process.WorkloadView{Key: k, Name: "go", Class: domain.ClassBuild, Owner: owner, OwnerPID: ownerPID, CPU: cpu, RSS: rss}
}

type step struct {
	at     int
	snap   process.Snapshot
	queued []domain.ProcessKey
}

func run(steps []step) Report {
	a := New()
	for _, s := range steps {
		a.Observe(at(s.at), s.snap, s.queued)
	}
	return a.Report()
}

func find(t *testing.T, rep Report, k domain.ProcessKey) Row {
	t.Helper()
	for _, r := range rep.Agents {
		if r.Process == k {
			return r
		}
	}
	t.Fatalf("no row for %v in %+v", k, rep.Agents)
	return Row{}
}

func TestIntegration(t *testing.T) {
	c1, x1 := key(10, 1), key(20, 1)
	snap := func(c, x float64, rss uint64) process.Snapshot {
		return process.Snapshot{Agents: []process.AgentView{agent("claude-1", c1, c, rss), agent("codex-1", x1, x, 100)}}
	}
	tests := []struct {
		name          string
		steps         []step
		wantC, wantX  float64
		wantPeak      uint64
		wantTotalPeak uint64
		wantActive    time.Duration
	}{
		{
			name:  "first snapshot has no interval",
			steps: []step{{at: 0, snap: snap(3, 3, 50)}},
			wantC: 0, wantX: 0, wantPeak: 50, wantTotalPeak: 150,
		},
		{
			name:  "rate times interval",
			steps: []step{{at: 0, snap: snap(0, 0, 50)}, {at: 2, snap: snap(1.5, 0.5, 400)}, {at: 5, snap: snap(1, 2, 200)}},
			// 1.5*2 + 1*3; 0.5*2 + 2*3
			wantC: 6, wantX: 7, wantPeak: 400, wantTotalPeak: 500, wantActive: 5 * time.Second,
		},
		{
			name:  "time not advancing adds nothing",
			steps: []step{{at: 0, snap: snap(0, 0, 50)}, {at: 0, snap: snap(4, 4, 50)}, {at: -1, snap: snap(4, 4, 50)}},
			wantC: 0, wantX: 0, wantPeak: 50, wantTotalPeak: 150,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := run(tt.steps)
			c, x := find(t, rep, c1), find(t, rep, x1)
			if c.CPUSeconds != tt.wantC || x.CPUSeconds != tt.wantX {
				t.Errorf("cpu = %v, %v; want %v, %v", c.CPUSeconds, x.CPUSeconds, tt.wantC, tt.wantX)
			}
			if rep.Total.CPUSeconds != tt.wantC+tt.wantX {
				t.Errorf("total cpu = %v", rep.Total.CPUSeconds)
			}
			if c.PeakRSS != tt.wantPeak || rep.Total.PeakRSS != tt.wantTotalPeak {
				t.Errorf("peak = %d, total peak = %d", c.PeakRSS, rep.Total.PeakRSS)
			}
			if c.ActiveFor() != tt.wantActive || !rep.Since.Equal(t0) {
				t.Errorf("active for %v, since %v", c.ActiveFor(), rep.Since)
			}
			if tt.wantC != tt.wantX && rep.Agents[0].CPUSeconds < rep.Agents[1].CPUSeconds {
				t.Errorf("not sorted by cpu: %+v", rep.Agents)
			}
		})
	}
}

func TestLabelReuse(t *testing.T) {
	old, reused := key(10, 1), key(30, 50)
	rep := run([]step{
		{at: 0, snap: process.Snapshot{Agents: []process.AgentView{agent("claude-1", old, 0, 10)}}},
		{at: 10, snap: process.Snapshot{Agents: []process.AgentView{agent("claude-1", old, 1, 10)}}},
		{at: 20, snap: process.Snapshot{}}, // claude-1 exited
		{at: 30, snap: process.Snapshot{Agents: []process.AgentView{agent("claude-1", reused, 0, 99)}}},
		{at: 40, snap: process.Snapshot{Agents: []process.AgentView{agent("claude-1", reused, 2, 99)}}},
	})
	if len(rep.Agents) != 2 {
		t.Fatalf("rows = %+v", rep.Agents)
	}
	o, n := find(t, rep, old), find(t, rep, reused)
	if o.CPUSeconds != 10 || o.PeakRSS != 10 || !o.LastSeen.Equal(at(10)) {
		t.Errorf("old instance = %+v", o)
	}
	// The gap while no agent ran still counts for the snapshot after it.
	if n.CPUSeconds != 20 || n.PeakRSS != 99 || !n.FirstSeen.Equal(at(30)) || n.Agent != "claude-1" {
		t.Errorf("reused label = %+v", n)
	}
	if rep.Agents[0].Process != reused {
		t.Errorf("order = %+v", rep.Agents)
	}
}

func TestEvictionBound(t *testing.T) {
	a := New()
	// One long-lived agent plus a stream of short-lived ones.
	keep := key(1, 1)
	for i := range MaxAgents + 50 {
		k := key(1000+i, i)
		a.Observe(at(i), process.Snapshot{Agents: []process.AgentView{
			agent("claude-1", keep, 1, 1), agent(fmt.Sprintf("codex-%d", i), k, 1, 1),
		}}, nil)
		if len(a.agents) > MaxAgents {
			t.Fatalf("step %d: %d rows retained", i, len(a.agents))
		}
	}
	rep := a.Report()
	if len(rep.Agents) != MaxAgents {
		t.Fatalf("rows = %d", len(rep.Agents))
	}
	find(t, rep, keep)
	// The oldest short-lived instances were evicted, the newest kept.
	for _, r := range rep.Agents {
		if r.Process.PID != 1 && r.Process.PID < 1000+50+1 {
			t.Errorf("kept stale instance %+v", r)
		}
	}
	find(t, rep, key(1000+MaxAgents+49, MaxAgents+49))
	// Totals still include evicted instances: every step after the first
	// adds 1s from claude-1 and 1s from the newcomer.
	if want := float64(2 * (MaxAgents + 49)); rep.Total.CPUSeconds != want {
		t.Errorf("total cpu = %v, want %v", rep.Total.CPUSeconds, want)
	}
}

func TestEvictionWhenAllLive(t *testing.T) {
	a := New()
	var agents []process.AgentView
	for i := range MaxAgents {
		agents = append(agents, agent("a", key(100+i, 1), 1, 1))
	}
	a.Observe(at(0), process.Snapshot{Agents: agents}, nil)
	a.Observe(at(1), process.Snapshot{Agents: append(agents, agent("late", key(9999, 5), 1, 1))}, nil)
	rep := a.Report()
	if len(rep.Agents) != MaxAgents {
		t.Fatalf("rows = %d", len(rep.Agents))
	}
	for _, r := range rep.Agents {
		if r.Agent == "late" {
			t.Fatalf("newcomer displaced an instance with history")
		}
	}
}

func TestHeavyAndUnattributed(t *testing.T) {
	c1 := key(10, 1)
	ag := []process.AgentView{agent("claude-1", c1, 0, 0)}
	build, test, orphan := key(11, 2), key(12, 3), key(50, 4)
	rep := run([]step{
		{at: 0, snap: process.Snapshot{Agents: ag, Heavy: []process.WorkloadView{heavy(build, "claude-1", 10, 1, 100)}}},
		{at: 1, snap: process.Snapshot{Agents: ag, Heavy: []process.WorkloadView{
			heavy(build, "claude-1", 10, 1, 100), heavy(orphan, "", 0, 2, 300), heavy(key(51, 4), "", 0, 1, 200),
		}}},
		{at: 3, snap: process.Snapshot{Agents: ag, Heavy: []process.WorkloadView{heavy(test, "claude-1", 10, 1, 100), heavy(orphan, "", 0, 0.5, 50)}}},
		{at: 4, snap: process.Snapshot{Agents: ag}},
		// The same build PID with a new start is a new workload.
		{at: 5, snap: process.Snapshot{Agents: ag, Heavy: []process.WorkloadView{heavy(key(11, 9), "claude-1", 10, 1, 100)}}},
	})
	c := find(t, rep, c1)
	if c.HeavyStarted != 3 || rep.Total.HeavyStarted != 3 {
		t.Errorf("heavy started = %d, total %d", c.HeavyStarted, rep.Total.HeavyStarted)
	}
	u := rep.Unattributed
	// orphan: 2*1 + 0.5*2; pid 51: 1*1. Peak is the sum within one sample.
	if u.HeavyStarted != 2 || u.CPUSeconds != 4 || u.PeakRSS != 500 || !u.FirstSeen.Equal(at(1)) || !u.LastSeen.Equal(at(3)) {
		t.Errorf("unattributed = %+v", u)
	}
	if u.Agent != "" {
		t.Errorf("unattributed has label %q", u.Agent)
	}
}

func TestQueueTime(t *testing.T) {
	c1, x1 := key(10, 1), key(20, 1)
	ag := []process.AgentView{agent("claude-1", c1, 0, 0), agent("codex-1", x1, 0, 0)}
	b1, b2, orphan := key(11, 2), key(21, 2), key(50, 2)
	hv := []process.WorkloadView{heavy(b1, "claude-1", 10, 0, 0), heavy(b2, "codex-1", 20, 0, 0), heavy(orphan, "", 0, 0, 0)}
	tests := []struct {
		name         string
		steps        []step
		wantC, wantX float64
		wantTotal    float64
	}{
		{
			name: "charged for the interval after queueing",
			steps: []step{
				{at: 0, snap: process.Snapshot{Agents: ag, Heavy: hv}, queued: []domain.ProcessKey{b1}},
				{at: 4, snap: process.Snapshot{Agents: ag, Heavy: hv}, queued: []domain.ProcessKey{b1, b2}},
				{at: 6, snap: process.Snapshot{Agents: ag, Heavy: hv}},
				{at: 10, snap: process.Snapshot{Agents: ag, Heavy: hv}},
			},
			wantC: 6, wantX: 2, wantTotal: 8,
		},
		{
			name: "workload that exited while queued",
			steps: []step{
				{at: 0, snap: process.Snapshot{Agents: ag, Heavy: hv}, queued: []domain.ProcessKey{b1}},
				{at: 3, snap: process.Snapshot{Agents: ag}, queued: []domain.ProcessKey{b1}},
				{at: 9, snap: process.Snapshot{Agents: ag}},
			},
			wantC: 3, wantTotal: 3,
		},
		{
			name: "unknown key is ignored",
			steps: []step{
				{at: 0, snap: process.Snapshot{Agents: ag, Heavy: hv}, queued: []domain.ProcessKey{key(77, 7)}},
				{at: 5, snap: process.Snapshot{Agents: ag, Heavy: hv}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := run(tt.steps)
			c, x := find(t, rep, c1), find(t, rep, x1)
			if c.QueuedSeconds != tt.wantC || x.QueuedSeconds != tt.wantX || rep.Total.QueuedSeconds != tt.wantTotal {
				t.Errorf("queued = %v, %v, total %v", c.QueuedSeconds, x.QueuedSeconds, rep.Total.QueuedSeconds)
			}
			if rep.Unattributed.QueuedSeconds != 0 {
				t.Errorf("unattributed queued = %v", rep.Unattributed.QueuedSeconds)
			}
		})
	}
}

func TestReportIsACopy(t *testing.T) {
	a := New()
	a.Observe(at(0), process.Snapshot{Agents: []process.AgentView{agent("claude-1", key(10, 1), 1, 1)}}, nil)
	rep := a.Report()
	rep.Agents[0].CPUSeconds = 99
	a.Observe(at(1), process.Snapshot{Agents: []process.AgentView{agent("claude-1", key(10, 1), 1, 1)}}, nil)
	if got := a.Report().Agents[0].CPUSeconds; got != 1 {
		t.Fatalf("cpu = %v", got)
	}
	if New().Report().Agents == nil {
		t.Fatal("empty report has nil agents")
	}
}
