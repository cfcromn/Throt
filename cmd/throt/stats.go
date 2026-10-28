package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/stats"
	"github.com/cfcromn/Throt/internal/transport"
)

// cmdStats is 'throt stats [--json]'.
func cmdStats(ctx context.Context, e env, args []string) error {
	asJSON, err := parseStatsArgs(args, e.stdout, e.stderr)
	if err != nil {
		return err
	}
	cfg := e.clientConfig()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var r app.StatsResult
	if err := transport.Call(ctx, cfg.SocketPath, "stats", nil, &r); err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	printStats(e.stdout, r, time.Now())
	return nil
}

// parseStatsArgs parses the arguments after 'stats'.
func parseStatsArgs(args []string, stdout, stderr io.Writer) (asJSON bool, err error) {
	sfs := flag.NewFlagSet("throt stats", flag.ContinueOnError)
	sfs.SetOutput(stderr)
	sfs.Usage = func() { fmt.Fprintln(sfs.Output(), "usage: throt stats [--json]"); sfs.PrintDefaults() }
	sfs.BoolVar(&asJSON, "json", false, "print the stats as JSON")
	if err := parseFlags(sfs, args, stdout); err != nil {
		return false, err
	}
	if sfs.NArg() > 0 {
		return false, usageLine("usage: throt stats [--json]")
	}
	return asJSON, nil
}

// printStats prints per-agent usage, most CPU time first (the daemon sorts
// the rows), followed by the all-agents and unattributed totals.
func printStats(w io.Writer, r app.StatsResult, now time.Time) {
	if r.Since.IsZero() {
		fmt.Fprintln(w, "no samples yet")
		return
	}
	fmt.Fprintf(w, "SINCE %s (%s ago, in memory; resets when throtd restarts)\n\n",
		r.Since.Local().Format("2006-01-02 15:04:05"), now.Sub(r.Since).Round(time.Second))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tPID\tCPU-TIME\tPEAK MEM\tHEAVY JOBS\tTIME QUEUED\tACTIVE FOR")
	if len(r.Agents) == 0 {
		fmt.Fprintln(tw, "(no agents seen)")
	}
	for _, a := range r.Agents {
		statsRow(tw, a.Agent, strconv.Itoa(a.Process.PID), a)
	}
	statsRow(tw, "all agents", "-", r.Total)
	statsRow(tw, "unattributed", "-", r.Unattributed)
	tw.Flush()
}

func statsRow(w io.Writer, name, pid string, r stats.Row) {
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", name, pid, seconds(r.CPUSeconds), gib(r.PeakRSS),
		r.HeavyStarted, seconds(r.QueuedSeconds), r.ActiveFor().Round(time.Second))
}

func seconds(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
}
