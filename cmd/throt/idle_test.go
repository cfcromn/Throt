package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

func TestIdleSessions(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	start := now.Add(-26 * time.Hour)
	key := func(pid int) domain.ProcessKey { return domain.ProcessKey{PID: pid, Start: start} }
	ag := func(pid int, cpu float64, rss uint64) process.AgentView {
		return process.AgentView{Key: key(pid), Agent: "claude-" + string(rune('0'+pid%10)), CPU: cpu, RSS: rss}
	}
	heavy := func(owner int) process.WorkloadView {
		return process.WorkloadView{Key: domain.ProcessKey{PID: 9000 + owner}, Name: "go", OwnerPID: owner}
	}
	type want struct {
		pid  int
		idle bool
	}
	tests := []struct {
		name    string
		samples []app.StatusResult
		want    []want
	}{
		{name: "no samples"},
		{
			name:    "quiet in every sample is idle",
			samples: []app.StatusResult{{Agents: []process.AgentView{ag(1, 0, 100)}}, {Agents: []process.AgentView{ag(1, 0.005, 100)}}},
			want:    []want{{1, true}},
		},
		{
			name:    "one busy sample is not idle",
			samples: []app.StatusResult{{Agents: []process.AgentView{ag(1, 0.3, 100)}}, {Agents: []process.AgentView{ag(1, 0, 100)}}},
			want:    []want{{1, false}},
		},
		{
			name:    "at the threshold is not idle",
			samples: []app.StatusResult{{Agents: []process.AgentView{ag(1, idleCPU, 100)}}},
			want:    []want{{1, false}},
		},
		{
			name: "an owned heavy workload in any sample is not idle",
			samples: []app.StatusResult{
				{Agents: []process.AgentView{ag(1, 0, 100), ag(2, 0, 100)}, Heavy: []process.WorkloadView{heavy(1)}},
				{Agents: []process.AgentView{ag(1, 0, 100), ag(2, 0, 100)}},
			},
			want: []want{{1, false}, {2, true}},
		},
		{
			name: "a PID reused by a new process does not inherit CPU",
			samples: []app.StatusResult{
				{Agents: []process.AgentView{{Key: domain.ProcessKey{PID: 1, Start: start.Add(-time.Hour)}, CPU: 2}}},
				{Agents: []process.AgentView{ag(1, 0, 100)}},
			},
			want: []want{{1, true}},
		},
		{
			name: "sorted by memory descending, then PID; exited sessions dropped",
			samples: []app.StatusResult{
				{Agents: []process.AgentView{ag(7, 0, 1)}},
				{Agents: []process.AgentView{ag(3, 0, 200), ag(1, 0.5, 900), ag(2, 0, 200)}},
			},
			want: []want{{1, false}, {2, true}, {3, true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := idleSessions(tt.samples, now)
			if len(rows) != len(tt.want) {
				t.Fatalf("got %d rows %+v, want %d", len(rows), rows, len(tt.want))
			}
			for i, w := range tt.want {
				if rows[i].PID != w.pid || rows[i].Idle != w.idle {
					t.Errorf("row %d = pid %d idle %v, want pid %d idle %v", i, rows[i].PID, rows[i].Idle, w.pid, w.idle)
				}
				if rows[i].Age != 26*time.Hour {
					t.Errorf("row %d age %v, want 26h", i, rows[i].Age)
				}
			}
		})
	}
}

func TestPrintIdleTotals(t *testing.T) {
	var b bytes.Buffer
	printIdle(&b, []idleRow{
		{Agent: "claude-1", PID: 1, Age: 26 * time.Hour, RSS: 2 << 30, Idle: true},
		{Agent: "codex-1", PID: 2, Age: 5 * time.Minute, RSS: 512 << 20, CPU: 0.4},
	}, 4*time.Second)
	out := b.String()
	for _, s := range []string{"claude-1  1    1d2h", "codex-1   2    5m", "TOTAL     2 sessions, 2.5 GiB; 1 idle, 2.0 GiB"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestAgeText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                           "-",
		90 * time.Second:            "1m",
		3*time.Hour + 5*time.Minute: "3h5m",
		50 * time.Hour:              "2d2h",
	} {
		if got := ageText(d); got != want {
			t.Errorf("ageText(%v) = %q, want %q", d, got, want)
		}
	}
}
