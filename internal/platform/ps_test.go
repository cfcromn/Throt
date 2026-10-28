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
	in := strings.Join([]string{
		"    1     0     0   7584  90:36.83 Sun Oct 18 11:06:02 2026     /sbin/launchd",
		"  412     1   501  20480   0:01.50 Mon Oct  5 09:00:00 2026     /System/Library/Input Methods/KoreanIM.app/Contents/MacOS/Korean IM",
		"  413     1   501      0   0:00.00 Mon Oct  5 09:00:00 2026     Core Audio Driver (Parrot.driver)",
		"",
		"garbage",
		"  414     1   501  100 0:00.00 Mon Oct  5 09:00:00 2026", // no comm
		"  415     x   501  100 0:00.00 Mon Oct  5 09:00:00 2026 bad-ppid",
		"  416     1   501  -1 0:00.00 Mon Oct  5 09:00:00 2026 bad-rss",
		"  417     1   501  100 nope Mon Oct  5 09:00:00 2026 bad-time",
		"  418     1   501  100 0:00.00 Mon Oct 45 09:00:00 2026 bad-date",
		"  419     1  4294967296  100 0:00.00 Mon Oct  5 09:00:00 2026 bad-uid",
		"  420     1   501  100 0:00.00 lun. 5 oct. 09:00:00 2026 localized",
		"\t421\t1\t501\t8\t1-02:03:04\tTue Oct  6 10:00:00 2026\tgo",
	}, "\n")
	got, err := parsePS(strings.NewReader(in), loc)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ProcessSample{
		{PID: 1, PPID: 0, UID: 0, Start: time.Date(2026, 10, 18, 11, 6, 2, 0, loc), Name: "launchd",
			CPUTime: 90*time.Minute + 36830*time.Millisecond, RSS: 7584 * 1024},
		{PID: 412, PPID: 1, UID: 501, Start: time.Date(2026, 10, 5, 9, 0, 0, 0, loc), Name: "Korean IM",
			CPUTime: 1500 * time.Millisecond, RSS: 20480 * 1024},
		{PID: 413, PPID: 1, UID: 501, Start: time.Date(2026, 10, 5, 9, 0, 0, 0, loc), Name: "Core Audio Driver (Parrot.driver)"},
		{PID: 421, PPID: 1, UID: 501, Start: time.Date(2026, 10, 6, 10, 0, 0, 0, loc), Name: "go",
			CPUTime: 26*time.Hour + 3*time.Minute + 4*time.Second, RSS: 8 * 1024},
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

func TestParsePSDropsForgedRecords(t *testing.T) {
	// "/tmp/evil\n1 0 0 ..." is one executable path containing a newline that
	// forges a record for PID 1.
	in := "  500   1 501 100 0:00.00 Mon Oct  5 09:00:00 2026 /tmp/evil\n" +
		"    1   0   0 100 0:00.00 Mon Oct  5 09:00:00 2026 fake\n" +
		"    1   0   0 100 0:00.00 Sun Oct 18 11:06:02 2026 /sbin/launchd\n"
	got, err := parsePS(strings.NewReader(in), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != 500 {
		t.Fatalf("got %+v, want only pid 500", got)
	}
}

func TestParsePSReadError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := parsePS(iotest.ErrReader(boom), time.UTC); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}
