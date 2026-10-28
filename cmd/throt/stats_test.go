package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/stats"
)

func TestPrintStats(t *testing.T) {
	since := time.Date(2026, 10, 28, 9, 0, 0, 0, time.Local)
	row := func(label string, pid int, cpu float64, rss uint64, heavy int, queued float64, active time.Duration) stats.Row {
		return stats.Row{Agent: label, Process: domain.ProcessKey{PID: pid, Start: since}, CPUSeconds: cpu, PeakRSS: rss,
			HeavyStarted: heavy, QueuedSeconds: queued, FirstSeen: since, LastSeen: since.Add(active)}
	}
	r := app.StatsResult{
		Since: since,
		Agents: []stats.Row{
			row("codex-1", 4400, 3725.4, 3<<30, 9, 190, 2*time.Hour),
			row("claude-1", 4312, 61, 900<<20, 2, 0, 30*time.Minute),
		},
		Total:        row("", 0, 3786.4, 3<<30+900<<20, 11, 190, 2*time.Hour),
		Unattributed: row("", 0, 12, 200<<20, 1, 0, time.Minute),
	}
	var b strings.Builder
	printStats(&b, r, since.Add(3*time.Hour))
	got := b.String()
	for _, want := range []string{
		"SINCE 2026-10-28 09:00:00 (3h0m0s ago",
		"AGENT         PID   CPU-TIME  PEAK MEM  HEAVY JOBS  TIME QUEUED  ACTIVE FOR",
		"codex-1       4400  1h2m5s    3.0 GiB   9           3m10s        2h0m0s",
		"claude-1      4312  1m1s      900 MiB   2           0s           30m0s",
		"all agents    -     1h3m6s    3.9 GiB   11          3m10s        2h0m0s",
		"unattributed  -     12s       200 MiB   1           0s           1m0s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "codex-1") > strings.Index(got, "claude-1") {
		t.Errorf("rows reordered:\n%s", got)
	}

	b.Reset()
	printStats(&b, app.StatsResult{}, since)
	if b.String() != "no samples yet\n" {
		t.Errorf("empty = %q", b.String())
	}
}
