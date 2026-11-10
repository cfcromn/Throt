package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/usage"
)

func TestParseSince(t *testing.T) {
	loc := time.FixedZone("KST", 9*3600)
	now := time.Date(2026, 10, 28, 14, 30, 0, 0, loc)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"24h", now.Add(-24 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"1h30m", now.Add(-90 * time.Minute)},
		{"7d", now.AddDate(0, 0, -7)},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, loc)},
		{"2026-10-27T08:00:00Z", time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		got, err := parseSince(tt.in, now)
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("%q: %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"", "0", "0h", "-1h", "0d", "-3d", "yesterday", "2026-13-01", "10"} {
		if got, err := parseSince(in, now); err == nil {
			t.Errorf("%q: %v, want an error", in, got)
		}
	}
}

func TestParseUsageArgs(t *testing.T) {
	now := time.Date(2026, 10, 28, 14, 30, 0, 0, time.UTC)
	a, err := parseUsageArgs(nil, now, io.Discard, io.Discard)
	if err != nil || a.by != usage.BySession || !a.since.Equal(now.Add(-24*time.Hour)) || a.asJSON {
		t.Errorf("defaults: %+v, %v", a, err)
	}
	a, err = parseUsageArgs([]string{"--by", "hour", "--since=2h", "--json"}, now, io.Discard, io.Discard)
	if err != nil || a.by != usage.ByHour || !a.since.Equal(now.Add(-2*time.Hour)) || !a.asJSON {
		t.Errorf("flags: %+v, %v", a, err)
	}
	for _, args := range [][]string{{"x"}, {"--by", "day"}, {"--since", "never"}, {"--bogus"}} {
		_, err := parseUsageArgs(args, now, io.Discard, io.Discard)
		var ee exitError
		if !errors.As(err, &ee) || ee.code != exitUsage || strings.Contains(ee.msg, "\n") {
			t.Errorf("%q: err %#v, want a one-line usage error", args, err)
		}
	}
}

func TestUsageText(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 9999: "9999", 10_000: "10.0k", 12_345: "12.3k", 999_949: "999.9k", 999_950: "1.0M",
		4_500_000: "4.5M", 999_949_999: "999.9M", 999_950_000: "1.0G", 11_262_582_580: "11.3G",
	} {
		if got := tokenText(n); got != want {
			t.Errorf("tokenText(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		0: "0s", 45 * time.Second: "45s", 12*time.Minute + 30*time.Second: "12m", 5*time.Hour + 12*time.Minute: "5h12m", 28 * time.Hour: "1d4h",
	} {
		if got := activeText(d); got != want {
			t.Errorf("activeText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPrintUsageEmpty(t *testing.T) {
	var b strings.Builder
	since := time.Date(2026, 10, 28, 9, 0, 0, 0, time.Local)
	rep := usage.NewCollector(since).Report(usage.BySession, time.Local)
	printUsage(&b, rep, []usage.Source{{Agent: usage.ClaudeCode, Dir: "/h/.claude/projects"}, {Agent: usage.Codex, Dir: "/h/.codex/sessions"}})
	want := `SINCE 2026-10-28 09:00 (active: gaps under 5m between a session's log entries, summed over sessions; tokens as logged, no cost estimate)

no agent sessions found in:
  /h/.claude/projects
  /h/.codex/sessions
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}
