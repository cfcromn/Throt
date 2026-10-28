package platform

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestParsePSTime(t *testing.T) {
	tests := map[string]time.Duration{
		"0:00.00":     0,
		"0:01.50":     1500 * time.Millisecond,
		"2:46.63":     2*time.Minute + 46630*time.Millisecond,
		"2221:05.48":  2221*time.Minute + 5480*time.Millisecond, // macOS: unbounded minutes
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"3-04:05:06":  3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second,
		"1-00:00:00":  24 * time.Hour,
		"12":          12 * time.Second,
		"0:00.123456": 123456 * time.Microsecond,
	}
	for in, want := range tests {
		got, err := parsePSTime(in)
		if err != nil || got != want {
			t.Errorf("parsePSTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "x:00", "1:2:3:4", ":05", "1:-5", "1:05.x", "a-01:00:00", "1:00.1234567890"} {
		if got, err := parsePSTime(in); err == nil {
			t.Errorf("parsePSTime(%q) = %v, want error", in, got)
		}
	}
}

func TestParsePS(t *testing.T) {
	loc := time.FixedZone("test", 9*3600)
	stats := strings.Join([]string{
		"    1     0     0   7584  90:36.83 Sun Oct 18 11:06:02 2026",
		"  412     1   501  20480   0:01.50 Mon Oct  5 09:00:00 2026",
		"  413     1   501      0   0:00.00 Mon Oct  5 09:00:00 2026",
		"",
		"garbage",
		"  414     1   501  100 0:00.00 Mon Oct  5 09:00:00 2026 extra", // wrong field count
		"  415     x   501  100 0:00.00 Mon Oct  5 09:00:00 2026",
		"  416     1   501  -1 0:00.00 Mon Oct  5 09:00:00 2026",
		"  417     1   501  100 nope Mon Oct  5 09:00:00 2026",
		"  418     1   501  100 0:00.00 Mon Oct 45 09:00:00 2026",
		"  419     1  4294967296  100 0:00.00 Mon Oct  5 09:00:00 2026",
		"  420     1   501  100 0:00.00 lun. 5 oct. 09:00:00 2026",
		"\t421\t1\t501\t8\t1-02:03:04\tTue Oct  6 10:00:00 2026",
		"  422     1   501  8 0:00.00 Tue Oct  6 10:00:00 2026", // exited before the names pass
	}, "\n")
	names := strings.Join([]string{
		"    1 /sbin/launchd",
		"  412 /System/Library/Input Methods/KoreanIM.app/Contents/MacOS/Korean IM",
		"  413 Core Audio Driver (Parrot.driver)",
		"  421 /usr/local/go/bin/go",
		"  999 /bin/new-after-stats-pass",
		"bad line",
	}, "\n")
	got, err := parsePS(strings.NewReader(stats), strings.NewReader(names), loc)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ProcessSample{
		{PID: 1, PPID: 0, UID: 0, Start: time.Date(2026, 10, 18, 11, 6, 2, 0, loc), Name: "launchd",
			CPUTime: 90*time.Minute + 36830*time.Millisecond, RSS: 7584 * 1024},
		{PID: 412, PPID: 1, UID: 501, Start: time.Date(2026, 10, 5, 9, 0, 0, 0, loc), Name: "Korean",
			CPUTime: 1500 * time.Millisecond, RSS: 20480 * 1024},
		{PID: 413, PPID: 1, UID: 501, Start: time.Date(2026, 10, 5, 9, 0, 0, 0, loc), Name: "Core"},
		{PID: 421, PPID: 1, UID: 501, Start: time.Date(2026, 10, 6, 10, 0, 0, 0, loc), Name: "go",
			CPUTime: 26*time.Hour + 3*time.Minute + 4*time.Second, RSS: 8 * 1024},
		{PID: 422, PPID: 1, UID: 501, Start: time.Date(2026, 10, 6, 10, 0, 0, 0, loc), RSS: 8 * 1024},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) {
			t.Errorf("[%d] Start = %v, want %v", i, got[i].Start, want[i].Start)
		}
		got[i].Start, want[i].Start = time.Time{}, time.Time{}
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPSName(t *testing.T) {
	for in, want := range map[string]string{
		"claude":                                         "claude",
		"claude --api-key=sk-SECRET":                     "claude",
		"npm run build":                                  "npm",
		"npm exec @scope/server --stdio":                 "npm",
		"/usr/local/go/bin/go":                           "go",
		"/usr/bin/x --tok=a/b":                           "x",
		"/usr/bin/x secret":                              "x",
		"/Applications/My App.app/Contents/MacOS/My App": "My",
	} {
		if got := psName(in); got != want {
			t.Errorf("psName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePSForgedNameLine(t *testing.T) {
	// The executable "/tmp/evil\n    1 claude" forges a name line for PID 1.
	// Both processes must survive; PID 1 merely loses its (ambiguous) name.
	stats := "  500   1 501 100 0:00.00 Mon Oct  5 09:00:00 2026\n" +
		"    1   0   0 100 0:00.00 Sun Oct 18 11:06:02 2026\n"
	names := "  500 /tmp/evil\n    1 claude\n    1 /sbin/launchd\n"
	got, err := parsePS(strings.NewReader(stats), strings.NewReader(names), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PID != 500 || got[0].Name != "evil" || got[1].PID != 1 || got[1].Name != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestParsePSReadError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := parsePS(iotest.ErrReader(boom), strings.NewReader(""), time.UTC); !errors.Is(err, boom) {
		t.Fatalf("stats err = %v, want boom", err)
	}
	if _, err := parsePS(strings.NewReader(""), iotest.ErrReader(boom), time.UTC); !errors.Is(err, boom) {
		t.Fatalf("names err = %v, want boom", err)
	}
}

func TestParsePSIdentity(t *testing.T) {
	loc := time.FixedZone("test", 9*3600)
	start, uid, err := parsePSIdentity([]byte("  501 Sun Oct  4 11:06:02 2026    \n"), loc)
	want := time.Date(2026, 10, 4, 11, 6, 2, 0, loc)
	if err != nil || uid != 501 || !start.Equal(want) {
		t.Fatalf("got %v uid %d err %v, want %v uid 501", start, uid, err, want)
	}
	// Identity must match what the sampler parses for the same lstart.
	p, ok := parsePSStatLine("7 1 501 10 0:00.01 Sun Oct  4 11:06:02 2026", loc)
	if !ok || !p.Start.Equal(start) {
		t.Fatalf("sampler start %v, identity start %v", p.Start, start)
	}
	for _, bad := range []string{
		"",
		"501",
		"501 Sun Oct 4 11:06:02",
		"x Sun Oct 4 11:06:02 2026",
		"501 Sun Oct 4 11:06:02 2026 extra",
		"501 Sun Oct 4 11:06:02 2026\n501 Sun Oct 4 11:06:02 2026",
		"501 Xyz Oct 4 11:06:02 2026",
	} {
		if _, _, err := parsePSIdentity([]byte(bad), loc); err == nil {
			t.Errorf("parsePSIdentity(%q) succeeded", bad)
		}
	}
}
