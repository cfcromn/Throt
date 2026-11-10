package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cfcromn/Throt/internal/usage"
)

// 'throt usage' reads the agents' session logs itself: it needs no throtd,
// writes nothing, and reads counts and timestamps only (see internal/usage).

const usageUsage = `usage: throt usage [--by session|project|hour] [--since DURATION|DATE] [--json]

Tokens and active time of Claude Code and Codex sessions, read from their
session logs ($CLAUDE_CONFIG_DIR or ~/.claude, $CODEX_HOME or ~/.codex).
Counts and timestamps only, never conversation content; read-only.
Active time is the sum of the gaps under 5 minutes between consecutive
log entries of a session. Tokens are as the agent logged them (input
excludes cached tokens); there is no cost estimate.`

// usageArgs are the arguments after 'usage'.
type usageArgs struct {
	by     usage.By
	since  time.Time
	asJSON bool
}

// cmdUsage is 'throt usage'.
func cmdUsage(ctx context.Context, e env, args []string) error {
	a, err := parseUsageArgs(args, time.Now(), e.stdout, e.stderr)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("usage: home directory: %w", err)
	}
	sources := usage.Sources(os.Getenv, home)
	c := usage.NewCollector(a.since)
	if err := usage.Scan(ctx, sources, c); err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	rep := c.Report(a.by, time.Local)
	if a.asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printUsage(e.stdout, rep, sources)
	return nil
}

// parseUsageArgs parses the arguments after 'usage'.
func parseUsageArgs(args []string, now time.Time, stdout, stderr io.Writer) (usageArgs, error) {
	var a usageArgs
	by, since := "", ""
	fs := flag.NewFlagSet("throt usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), usageUsage); fs.PrintDefaults() }
	fs.StringVar(&by, "by", "session", "group the rows by `KEY`: session, project or hour")
	fs.StringVar(&since, "since", "24h", "count entries from `WHEN`: a duration ago (90m, 24h, 7d) or a date (2006-01-02, or RFC 3339)")
	fs.BoolVar(&a.asJSON, "json", false, "print the report as JSON")
	if err := parseFlags(fs, args, stdout); err != nil {
		return a, err
	}
	if fs.NArg() > 0 {
		return a, usageError("throt usage", fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	var err error
	if a.by, err = usage.ParseBy(by); err != nil {
		return a, usageError("throt usage", "--by: "+err.Error())
	}
	if a.since, err = parseSince(since, now); err != nil {
		return a, usageError("throt usage", "--since: "+err.Error())
	}
	return a, nil
}

// parseSince reads a time as a duration before now ("90m", "24h", "7d"),
// a local date ("2006-01-02") or an RFC 3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 && n <= 36500 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("%q is not a positive duration", s)
		}
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, now.Location()); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a duration (24h, 7d) or a date (2006-01-02)", s)
}

const usageTime = "2006-01-02 15:04"

// printUsage prints the per-agent totals, then the grouped rows.
func printUsage(w io.Writer, r usage.Report, sources []usage.Source) {
	fmt.Fprintf(w, "SINCE %s (active: gaps under %dm between a session's log entries, summed over sessions; tokens as logged, no cost estimate)\n\n",
		r.Since.Local().Format(usageTime), r.ActiveGapSeconds/60)
	if len(r.Rows) == 0 {
		fmt.Fprintln(w, "no agent sessions found in:")
		for _, s := range sources {
			fmt.Fprintf(w, "  %s\n", s.Dir)
		}
		return
	}
	const counts = "INPUT\tOUTPUT\tCACHE WRITE\tCACHE READ\tMESSAGES\tACTIVE\tFIRST\tLAST"
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSESSIONS\t"+counts)
	for _, a := range r.Agents {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", a.Agent, a.Sessions, usageCounts(a))
	}
	fmt.Fprintf(tw, "total\t%d\t%s\n", r.Total.Sessions, usageCounts(r.Total))
	tw.Flush()
	fmt.Fprintln(w)

	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	switch r.By {
	case usage.ByProject:
		fmt.Fprintln(tw, "PROJECT\tSESSIONS\t"+counts)
	case usage.ByHour:
		fmt.Fprintln(tw, "HOUR\tSESSIONS\t"+counts)
	default:
		fmt.Fprintln(tw, "SESSION\tAGENT\tPROJECT\t"+counts)
	}
	for _, row := range r.Rows {
		switch r.By {
		case usage.ByProject:
			fmt.Fprintf(tw, "%s\t%d\t", orDash(row.Project), row.Sessions)
		case usage.ByHour:
			fmt.Fprintf(tw, "%s\t%d\t", row.Hour.Local().Format(usageTime), row.Sessions)
		default:
			fmt.Fprintf(tw, "%s\t%s\t%s\t", shortID(row.Session), row.Agent, orDash(row.Project))
		}
		fmt.Fprintln(tw, usageCounts(row))
	}
	tw.Flush()
	if r.SkippedLines > 0 {
		fmt.Fprintf(w, "\n%d malformed or overlong log line(s) skipped\n", r.SkippedLines)
	}
}

// usageCounts renders the count columns of row.
func usageCounts(row usage.Row) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s", tokenText(row.Input), tokenText(row.Output),
		tokenText(row.CacheWrite), tokenText(row.CacheRead), row.Messages,
		activeText(time.Duration(row.ActiveSeconds)*time.Second),
		row.First.Local().Format(usageTime), row.Last.Local().Format(usageTime))
}

// tokenText renders a token count: exact below 10,000, then "12.3k",
// "4.5M", "1.2G".
func tokenText(n int64) string {
	switch {
	case n < 10_000:
		return strconv.FormatInt(n, 10)
	case n < 999_950:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	case n < 999_950_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.1fG", float64(n)/1e9)
	}
}

// activeText renders active time: "45s", "12m", "5h12m".
func activeText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return ageText(d)
}

// shortID is the first 8 characters of a session id, enough to tell
// sessions apart in a table; --json has the whole id.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
