package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// cmdStatus is 'throt status [--json | --idle]'.
func cmdStatus(ctx context.Context, e env, args []string) error {
	asJSON, idle := false, false
	switch {
	case len(args) == 0:
	case len(args) == 1 && (args[0] == "--json" || args[0] == "-json"):
		asJSON = true
	case len(args) == 1 && (args[0] == "--idle" || args[0] == "-idle"):
		idle = true
	default:
		return usageLine("usage: throt status [--json | --idle]")
	}
	cfg := e.clientConfig()
	if idle {
		gap := time.Duration(cfg.SampleInterval)
		samples, err := observeStatus(ctx, idleSamples, gap, func(ctx context.Context, st *app.StatusResult) error {
			ctx, cancel := context.WithTimeout(ctx, callTimeout)
			defer cancel()
			return transport.Call(ctx, cfg.SocketPath, "status", nil, st)
		})
		if err != nil {
			return err
		}
		printIdle(e.stdout, idleSessions(samples, time.Now()), (idleSamples-1)*gap)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var st app.StatusResult
	if err := transport.Call(ctx, cfg.SocketPath, "status", nil, &st); err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(e.stdout, st)
	return nil
}

func printStatus(w io.Writer, st app.StatusResult) {
	fmt.Fprintf(w, "PRESSURE  %s", strings.ToUpper(st.Pressure.String()))
	if st.Reason != "" {
		fmt.Fprintf(w, " (%s)", st.Reason)
	}
	fmt.Fprintln(w)
	if st.Degraded != "" {
		fmt.Fprintln(w, "DEGRADED  "+st.Degraded)
	}
	if n := st.OrphanCount; n > 0 {
		noun := "processes"
		if n == 1 {
			noun = "process"
		}
		fmt.Fprintf(w, "STOPPED   %d %s not paused by throtd (see throt doctor)\n", n, noun)
	}
	h := st.Host
	if h == nil {
		fmt.Fprintln(w, "no host sample yet")
		return
	}
	na := func(m domain.Metric, v string) string {
		if !h.Has(m) {
			return "n/a"
		}
		return v
	}
	fmt.Fprintf(w, "CPU       %s\n", na(domain.MetricCPU, fmt.Sprintf("%.0f%%", h.CPUUsage*100)))
	fmt.Fprintf(w, "LOAD      %s\n", na(domain.MetricLoad, fmt.Sprintf("%.2f per CPU", h.Load1)))
	mem := na(domain.MetricMemory, fmt.Sprintf("%s used of %s", gib(h.MemTotal-min(h.MemAvailable, h.MemTotal)), gib(h.MemTotal)))
	if h.HasMemoryPressure() {
		mem += fmt.Sprintf(" (kernel: %s)", h.MemoryPressure)
	}
	fmt.Fprintf(w, "MEM       %s\n", mem)
	fmt.Fprintf(w, "LATENCY   %s\n", na(domain.MetricLatency, h.Latency.Round(time.Millisecond).String()))
	fmt.Fprintf(w, "SWAP      %s\n", na(domain.MetricSwap, fmt.Sprintf("%s used of %s", gib(h.SwapUsed), gib(h.SwapTotal))))
	if psi := psiDetail(*h); psi != "" {
		fmt.Fprintf(w, "PSI       %s\n", psi)
	}

	// The scheduler lines belong with the summary above, so they are
	// written before the tables, which tw buffers until Flush.
	fmt.Fprintf(w, "SCHEDULER %s, %s, %d queued, %d reserved\n", modeName(st.Scheduler.Mode), limitText(st.Scheduler), len(st.Scheduler.Queued), len(st.Scheduler.Leases))
	const maxLeases = 5
	for i, l := range st.Scheduler.Leases {
		if i == maxLeases {
			fmt.Fprintf(w, "RESERVED  ... and %d more\n", len(st.Scheduler.Leases)-maxLeases)
			break
		}
		agent := l.Agent
		if agent == "" {
			agent = "-"
		}
		fmt.Fprintf(w, "RESERVED  pid %d (%s) since %s\n", l.Process.PID, agent, l.Granted.Format(time.TimeOnly))
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nAGENTS\nAGENT\tPID\tCPU\tMEM\tCHILDREN")
	if len(st.Agents) == 0 {
		fmt.Fprintln(tw, "(none detected)")
	}
	const maxAgents = 15
	for i, a := range st.Agents {
		if i == maxAgents {
			fmt.Fprintf(tw, "... and %d more\n", len(st.Agents)-maxAgents)
			break
		}
		fmt.Fprintf(tw, "%s\t%d\t%.0f%%\t%s\t%d\n", a.Agent, a.Key.PID, a.CPU*100, gib(a.RSS), a.Children)
	}
	paused := make(map[int]bool, len(st.Paused))
	for _, p := range st.Paused {
		paused[p.Key.PID] = true
	}
	queued := make(map[int]bool, len(st.Scheduler.Queued))
	for _, k := range st.Scheduler.Queued {
		queued[k.PID] = true
	}
	fmt.Fprintln(tw, "\nHEAVY WORKLOADS\nOWNER\tPID\tPROCESS\tCLASS\tCPU\tMEM\tSTATE")
	if len(st.Heavy) == 0 {
		fmt.Fprintln(tw, "(none)")
	}
	for _, h := range st.Heavy {
		owner := h.Owner
		if owner == "" {
			owner = "-"
		}
		state := "running"
		switch {
		case queued[h.Key.PID]:
			state = "QUEUED"
		case paused[h.Key.PID]:
			state = "PAUSED"
		case slices.ContainsFunc(st.Scheduler.WouldQueue, func(k domain.ProcessKey) bool { return k.PID == h.Key.PID }):
			state = "running (would queue)"
		case h.Background:
			state = "running (background)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.0f%%\t%s\t%s\n", owner, h.Key.PID, h.Name, h.Class, h.CPU*100, gib(h.RSS), state)
	}
	tw.Flush()
}

// limitText is the heavy limit as status and explain show it: "limit 3
// (adaptive, start 2, max 10)" for an adaptive scheduler, whose limit
// moves, else "limit 2".
func limitText(sc app.SchedulerStatus) string {
	s := fmt.Sprintf("limit %d", sc.HeavyLimit)
	if sc.Adaptive {
		s += fmt.Sprintf(" (adaptive, start %d, max %d)", sc.StartLimit, sc.MaxLimit)
	}
	return s
}

// psiDetail renders the Linux PSI readings the sample carries, as
// "mem 3% (full 1%)  io 0% (full 0%)  cpu 12%", or "" when it has none.
// The pressure policy uses some and full for memory, full for I/O and some
// for CPU.
func psiDetail(h domain.HostSample) string {
	var parts []string
	pct := func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
	if st, ok := h.StallFor(domain.MetricMemoryStall); ok {
		parts = append(parts, "mem "+pct(st.Some)+" (full "+pct(st.Full)+")")
	}
	if st, ok := h.StallFor(domain.MetricIOStall); ok {
		parts = append(parts, "io "+pct(st.Some)+" (full "+pct(st.Full)+")")
	}
	if st, ok := h.StallFor(domain.MetricCPUStall); ok {
		parts = append(parts, "cpu "+pct(st.Some))
	}
	return strings.Join(parts, "  ")
}

func gib(b uint64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%d MiB", b>>20)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}
