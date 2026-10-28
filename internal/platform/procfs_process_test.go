package platform

import (
	"strings"
	"testing"
	"time"
)

// statLine builds a /proc/<pid>/stat line with the given comm and the
// fields Throt reads: ppid=1, utime=250, stime=50, starttime=12345, rss=300.
func statLine(comm string) string {
	return "42 (" + comm + ") S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 12345 1000000 300 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n"
}

func TestParsePIDStat(t *testing.T) {
	for _, comm := range []string{"bash", "my prog", "a) b (c", ")", "", "x) S 9 9 9"} {
		got, err := parsePIDStat([]byte(statLine(comm)))
		if err != nil {
			t.Errorf("comm %q: %v", comm, err)
			continue
		}
		want := pidStat{PID: 42, Comm: comm, PPID: 1, CPUTicks: 300, StartTime: 12345, RSSPages: 300}
		if got != want {
			t.Errorf("comm %q: got %+v, want %+v", comm, got, want)
		}
	}
}

func TestParsePIDStatControlCharsInComm(t *testing.T) {
	// A process can put a newline in its own comm (found by FuzzParsePIDStat).
	got, err := parsePIDStat([]byte(statLine("a\nb\x7fc\td")))
	if err != nil || got.Comm != "a?b?c?d" {
		t.Fatalf("got %+v, %v; want comm %q", got, err, "a?b?c?d")
	}
}

func TestParsePIDStatErrors(t *testing.T) {
	tests := map[string]string{
		"empty":        "",
		"no parens":    "42 bash S 1 2 3",
		"bad pid":      "x (bash) S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 12345 1000000 300",
		"short":        "42 (bash) S 1 42 42",
		"bad ppid":     strings.Replace(statLine("bash"), "S 1 ", "S x ", 1),
		"negative rss": strings.Replace(statLine("bash"), " 300 ", " -1 ", 1),
		"only close":   "42 bash) S 1",
		// Found by FuzzParsePIDStat.
		"pid 0":          strings.Replace(statLine("bash"), "42 ", "0 ", 1),
		"negative pid":   strings.Replace(statLine("bash"), "42 ", "-42 ", 1),
		"huge ppid":      strings.Replace(statLine("bash"), "S 1 ", "S 18446744073709551615 ", 1),
		"huge utime":     strings.Replace(statLine("bash"), " 250 50 ", " 92233720368547759 50 ", 1),
		"cpu overflow":   strings.Replace(statLine("bash"), " 250 50 ", " 922337203685 50 ", 1),
		"huge starttime": strings.Replace(statLine("bash"), " 12345 ", " 18446744073709551615 ", 1),
	}
	for name, in := range tests {
		if got, err := parsePIDStat([]byte(in)); err == nil {
			t.Errorf("%s: got %+v, want error", name, got)
		}
	}
}

func TestParseStatusUID(t *testing.T) {
	in := "Name:\tbash\nUmask:\t0022\nState:\tS (sleeping)\nPid:\t42\nPPid:\t1\nUid:\t1000\t0\t0\t0\nGid:\t1000\t1000\t1000\t1000\n"
	uid, err := parseStatusUID(strings.NewReader(in))
	if err != nil || uid != 1000 {
		t.Fatalf("uid = %d, err = %v; want real uid 1000", uid, err)
	}
	for name, in := range map[string]string{
		"missing": "Name:\tbash\n",
		"empty":   "Uid:\n",
		"bad":     "Uid:\tx 0 0 0\n",
	} {
		if _, err := parseStatusUID(strings.NewReader(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestParseBootTime(t *testing.T) {
	got, err := parseBootTime(strings.NewReader(procStatFixture))
	if err != nil || !got.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("got %v, %v", got, err)
	}
	for name, in := range map[string]string{
		"missing":  "cpu 1 2 3 4\n",
		"bad":      "btime x\n",
		"zero":     "btime 0\n",
		"negative": "btime -5\n",
	} {
		if _, err := parseBootTime(strings.NewReader(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestTicksToDuration(t *testing.T) {
	if got := ticksToDuration(250); got != 2500*time.Millisecond {
		t.Errorf("ticksToDuration(250) = %v", got)
	}
}
