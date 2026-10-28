package platform

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
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
		want := pidStat{PID: 42, Comm: comm, State: domain.StateSleeping, PPID: 1, CPUTicks: 300, StartTime: 12345, RSSPages: 300}
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

func TestParsePIDStatState(t *testing.T) {
	for st, want := range map[string]domain.ProcessState{
		"R": domain.StateRunning, "S": domain.StateSleeping, "D": domain.StateSleeping, "I": domain.StateSleeping,
		"T": domain.StateStopped, "t": domain.StateOther, "Z": domain.StateZombie, "X": domain.StateOther, "W": domain.StateOther,
	} {
		in := strings.Replace(statLine("my prog"), ") S ", ") "+st+" ", 1)
		got, err := parsePIDStat([]byte(in))
		if err != nil || got.State != want {
			t.Errorf("state %q: got %q, %v; want %q", st, got.State, err, want)
		}
	}
	// A comm that looks like a state cannot change it: comm ends at the last ')'.
	if got, err := parsePIDStat([]byte(statLine("x) T 9"))); err != nil || got.State != domain.StateSleeping {
		t.Errorf("forged comm: got %+v, %v; want sleeping", got, err)
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
		"long state":   strings.Replace(statLine("bash"), ") S ", ") TS ", 1),
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

// The reference parsers below are the strings.Fields and bufio.Scanner
// versions that parsePIDStat and statusUID replaced. The parsers must agree
// with them on every input, errors included.

func referenceParsePIDStat(b []byte) (pidStat, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return pidStat{}, errors.New("parse stat: no comm")
	}
	var s pidStat
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b[:open])))
	if err != nil {
		return pidStat{}, fmt.Errorf("parse stat pid: %w", err)
	}
	if pid <= 0 {
		return pidStat{}, fmt.Errorf("parse stat: invalid pid %d", pid)
	}
	comm := bytes.Clone(b[open+1 : end])
	for i, c := range comm {
		if c < 0x20 || c == 0x7f {
			comm[i] = '?'
		}
	}
	s.PID, s.Comm = pid, string(comm)
	rest := strings.Fields(string(b[end+1:]))
	if len(rest) < 22 {
		return pidStat{}, fmt.Errorf("parse stat: %d fields after comm, want >= 22", len(rest))
	}
	if s.State, err = statState([]byte(rest[0])); err != nil {
		return pidStat{}, err
	}
	field := func(n int) (uint64, error) {
		v, err := strconv.ParseUint(rest[n-3], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse stat field %d: %w", n, err)
		}
		return v, nil
	}
	ppid, err := field(4)
	if err != nil {
		return pidStat{}, err
	}
	if ppid > math.MaxInt32 {
		return pidStat{}, fmt.Errorf("parse stat: invalid ppid %d", ppid)
	}
	utime, err := field(14)
	if err != nil {
		return pidStat{}, err
	}
	stime, err := field(15)
	if err != nil {
		return pidStat{}, err
	}
	if s.StartTime, err = field(22); err != nil {
		return pidStat{}, err
	}
	if utime > maxTicks || stime > maxTicks-utime || s.StartTime > maxTicks {
		return pidStat{}, errors.New("parse stat: times out of range")
	}
	if s.RSSPages, err = field(24); err != nil {
		return pidStat{}, err
	}
	s.PPID, s.CPUTicks = int(ppid), utime+stime
	return s, nil
}

func referenceStatusUID(r io.Reader) (uint32, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, errors.New("parse status: empty Uid line")
		}
		v, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse status Uid: %w", err)
		}
		return uint32(v), nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read status: %w", err)
	}
	return 0, errors.New("parse status: no Uid line")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// pidStatSeeds are stat lines for the equivalence fuzz target: the shapes
// the tests above use, plus Unicode and odd spacing, which take
// splitFields' bytes.Fields path or its ASCII edge cases.
func pidStatSeeds() []string {
	seeds := []string{
		"", "42 bash S 1 2 3", "42 bash) S 1", "42 (bash) S 1 42 42",
		"x (bash) S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 12345 1000000 300",
	}
	for _, comm := range []string{"bash", "my prog", "a) b (c", ")", "", "x) S 9 9 9", "a\nb", "a\x7fb\tc", "é"} {
		seeds = append(seeds, statLine(comm))
	}
	line := statLine("bash")
	for _, r := range []struct{ old, new string }{
		{") S ", ") TS "}, {") S ", ")  S "}, {" 250 ", "\u0085250 "}, {" 250 ", " \xff250 "},
		{" 250 50 ", "\t250\v50\f"}, {" 300 ", " 300\r\n"}, {"\n", ""}, {" 0 0 0 0 0\n", " 　"},
		{"S 1 ", "S x "}, {" 300 ", " -1 "}, {"42 ", "0 "}, {" 12345 ", " 18446744073709551615 "},
		{" 250 50 ", " 922337203685 50 "}, {"S 1 ", "S 18446744073709551615 "},
	} {
		seeds = append(seeds, strings.Replace(line, r.old, r.new, 1))
	}
	// Exactly 22 fields after comm, and 21.
	f := strings.Fields(line[strings.LastIndexByte(line, ')')+1:])
	return append(seeds, "42 (x) "+strings.Join(f[:22], " "), "42 (x) "+strings.Join(f[:21], " "))
}

func FuzzParsePIDStatMatchesReference(f *testing.F) {
	for _, s := range pidStatSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got, gotErr := parsePIDStat(b)
		want, wantErr := referenceParsePIDStat(b)
		if got != want || errString(gotErr) != errString(wantErr) {
			t.Errorf("parsePIDStat(%q) = %+v, %v; reference %+v, %v", b, got, gotErr, want, wantErr)
		}
		raw, comm, err := parsePIDStatRaw(b)
		if err == nil && !commEqual(comm, got.Comm) {
			t.Errorf("parsePIDStatRaw(%q): comm %q does not match %q", b, comm, got.Comm)
		}
		if raw.Comm != "" {
			t.Errorf("parsePIDStatRaw(%q) set Comm %q", b, raw.Comm)
		}
	})
}

func FuzzStatusUIDMatchesReference(f *testing.F) {
	for _, s := range []string{
		"Name:\tbash\nUmask:\t0022\nState:\tS (sleeping)\nPid:\t42\nPPid:\t1\nUid:\t1000\t0\t0\t0\nGid:\t1000\t1000\t1000\t1000\n",
		"Name:\tbash\n", "Uid:\n", "Uid:\tx 0 0 0\n", "Uid:\t7\r\n", "Uid:\t7\r\r\n", "Uid: 7 0\n", "Uid:\t 7\n",
		"Name:\tUid:\nUid:\t3\n", "\n\nUid: 4294967295\n", "Uid: 4294967296\n", "Uid:\t5", "uid:\t5\n", " Uid:\t5\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 32<<10 {
			return // the reference scanner rejects lines over 64 KiB; status lines are short
		}
		got, gotErr := statusUID([]byte(s))
		want, wantErr := referenceStatusUID(strings.NewReader(s))
		if got != want || errString(gotErr) != errString(wantErr) {
			t.Errorf("statusUID(%q) = %d, %v; reference %d, %v", s, got, gotErr, want, wantErr)
		}
	})
}

func TestCommEqual(t *testing.T) {
	for _, tc := range []struct {
		b, s string
		want bool
	}{
		{"bash", "bash", true}, {"bash", "bas", false}, {"a\nb", "a?b", true}, {"a?b", "a?b", true},
		{"a\nb", "a\nb", false}, {"", "", true}, {"x\x7f", "x?", true}, {"é", "é", true},
	} {
		if got := commEqual([]byte(tc.b), tc.s); got != tc.want {
			t.Errorf("commEqual(%q, %q) = %v, want %v", tc.b, tc.s, got, tc.want)
		}
	}
}
