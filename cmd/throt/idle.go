package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/cfcromn/Throt/internal/app"
)

// 'throt status --idle' is report-only: it reads status and prints. It
// never signals, pauses, or ends a process.

const (
	// idleSamples is how many status samples --idle observes, one sample
	// interval apart, so one quiet sample cannot make a session look idle.
	idleSamples = 3
	// idleCPU is the most CPU, in cores, a session's subtree may use in
	// any observed sample and still count as idle.
	idleCPU = 0.01
	// An old session counts as idle up to oldIdleCPU: long-lived sessions
	// keep a few percent of background CPU even when no one uses them.
	oldIdleCPU = 0.05
	oldIdleAge = time.Hour
)

// idleRow is one agent session as 'status --idle' reports it.
type idleRow struct {
	Agent string
	PID   int
	Age   time.Duration // 0 when the start time is unknown
	RSS   uint64        // in the last sample
	CPU   float64       // the most cores used in any sample
	Idle  bool
}

// idleSessions summarizes the agents in the last of samples, which are in
// time order. A session is idle when every sample that saw it shows its subtree
// below idleCPU (oldIdleCPU once it is oldIdleAge old) and no heavy workload
// owned by it. Rows are sorted by RSS
// descending, then PID.
func idleSessions(samples []app.StatusResult, now time.Time) []idleRow {
	if len(samples) == 0 {
		return nil
	}
	last := samples[len(samples)-1]
	rows := make([]idleRow, 0, len(last.Agents))
	for _, a := range last.Agents {
		r := idleRow{Agent: a.Agent, PID: a.Key.PID, RSS: a.RSS, Idle: true}
		if !a.Key.Start.IsZero() && now.After(a.Key.Start) {
			r.Age = now.Sub(a.Key.Start)
		}
		for _, st := range samples {
			for _, b := range st.Agents {
				if b.Key == a.Key {
					r.CPU = max(r.CPU, b.CPU)
				}
			}
			for _, h := range st.Heavy {
				if h.OwnerPID == a.Key.PID {
					r.Idle = false
				}
			}
		}
		limit := idleCPU
		if r.Age >= oldIdleAge {
			limit = oldIdleCPU
		}
		if r.CPU >= limit {
			r.Idle = false
		}
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(x, y idleRow) int {
		return cmp.Or(cmp.Compare(y.RSS, x.RSS), cmp.Compare(x.PID, y.PID))
	})
	return rows
}

// observeStatus takes n status samples, gap apart.
func observeStatus(ctx context.Context, n int, gap time.Duration, call func(context.Context, *app.StatusResult) error) ([]app.StatusResult, error) {
	out := make([]app.StatusResult, 0, n)
	for i := range n {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(gap):
			}
		}
		var st app.StatusResult
		if err := call(ctx, &st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func printIdle(w io.Writer, rows []idleRow, window time.Duration) {
	fmt.Fprintf(w, "AGENT SESSIONS (idle: CPU under %.0f%%, or %.0f%% after %.0fh, and no heavy workload over %s; report only)\n",
		idleCPU*100, oldIdleCPU*100, oldIdleAge.Hours(), window)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tPID\tAGE\tMEM\tCPU\tIDLE")
	if len(rows) == 0 {
		fmt.Fprintln(tw, "(none detected)")
	}
	var total, idleRSS uint64
	idle := 0
	for _, r := range rows {
		verdict := "no"
		if r.Idle {
			verdict = "yes"
			idle++
			idleRSS += r.RSS
		}
		total += r.RSS
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.0f%%\t%s\n", r.Agent, r.PID, ageText(r.Age), gib(r.RSS), r.CPU*100, verdict)
	}
	tw.Flush()
	fmt.Fprintf(w, "TOTAL     %d sessions, %s; %d idle, %s\n", len(rows), gib(total), idle, gib(idleRSS))
}

// ageText renders d coarsely: "3d4h", "5h12m", "12m", or "-" when unknown.
func ageText(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", d/time.Hour, d%time.Hour/time.Minute)
	default:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
}
