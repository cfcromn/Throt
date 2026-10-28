package stats

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// TestEmptyWindows covers a fresh accountant and observations with nothing
// to account: the report must be empty but well formed, and idle
// observations must not move the window start or open the total's range.
func TestEmptyWindows(t *testing.T) {
	rep := New().Report()
	if !rep.Since.IsZero() || rep.Agents == nil || len(rep.Agents) != 0 || rep.Total != (Row{}) || rep.Unattributed != (Row{}) {
		t.Errorf("fresh report = %+v", rep)
	}

	c1 := key(10, 1)
	one := process.Snapshot{Agents: []process.AgentView{agent("claude-1", c1, 1, 100)}}
	rep = run([]step{
		{at: 0},
		{at: 5},
		{at: 10, snap: one},
		{at: 12, snap: one},
		{at: 20},
	})
	r := find(t, rep, c1)
	// A rate covers the interval since the previous snapshot, idle or not:
	// 1 core over 5s, then over 2s.
	if !rep.Since.Equal(at(0)) || r.CPUSeconds != 7 || r.ActiveFor() != 2*time.Second {
		t.Errorf("since %v, row %+v; want since 0s, 7 CPU seconds, active 2s", rep.Since, r)
	}
	if !rep.Total.FirstSeen.Equal(at(10)) || !rep.Total.LastSeen.Equal(at(12)) || rep.Unattributed != (Row{}) {
		t.Errorf("total %+v, unattributed %+v", rep.Total, rep.Unattributed)
	}
}

// TestClockStepsBack checks that a wall clock stepped backwards between
// observations charges nothing for that interval, rather than negative
// time, and does not rewind LastSeen.
func TestClockStepsBack(t *testing.T) {
	c1, b1 := key(10, 1), key(11, 2)
	snap := process.Snapshot{
		Agents: []process.AgentView{agent("claude-1", c1, 2, 100)},
		Heavy:  []process.WorkloadView{heavy(b1, "claude-1", 10, 1, 50)},
	}
	rep := run([]step{
		{at: 100, snap: snap, queued: []domain.ProcessKey{b1}},
		{at: 40, snap: snap, queued: []domain.ProcessKey{b1}},
		{at: 41, snap: snap},
	})
	r := find(t, rep, c1)
	if r.CPUSeconds != 2 || r.QueuedSeconds != 1 || rep.Total.CPUSeconds != 2 || rep.Total.QueuedSeconds != 1 {
		t.Errorf("row %+v, total %+v; want only the last 1s charged", r, rep.Total)
	}
	if !r.FirstSeen.Equal(at(100)) || !r.LastSeen.Equal(at(100)) {
		t.Errorf("seen %v..%v", r.FirstSeen, r.LastSeen)
	}
}

// TestUnattributedQueueTime checks that queue time of a workload no agent
// owns goes to the unattributed bucket, not to the agents' total.
func TestUnattributedQueueTime(t *testing.T) {
	c1, orphan := key(10, 1), key(50, 2)
	snap := process.Snapshot{
		Agents: []process.AgentView{agent("claude-1", c1, 0, 0)},
		Heavy:  []process.WorkloadView{heavy(orphan, "", 0, 0, 0)},
	}
	rep := run([]step{
		{at: 0, snap: snap, queued: []domain.ProcessKey{orphan}},
		{at: 7, snap: snap},
	})
	if rep.Unattributed.QueuedSeconds != 7 || rep.Total.QueuedSeconds != 0 || find(t, rep, c1).QueuedSeconds != 0 {
		t.Errorf("unattributed %v, total %v", rep.Unattributed.QueuedSeconds, rep.Total.QueuedSeconds)
	}
}
