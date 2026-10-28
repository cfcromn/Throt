package platform

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// Fuzz targets for the ps and procfs parsers. Their input comes from other
// processes (comm, process titles) or from the kernel, so it is untrusted.
// Seeds are the unit-test fixtures.

func FuzzParsePS(f *testing.F) {
	f.Add("    1     0     0   7584 Ss   90:36.83 Sun Oct 18 11:06:02 2026\n"+
		"  412     1   501  20480 R+    0:01.50 Mon Oct  5 09:00:00 2026\n"+
		"\t421\t1\t501\t8\tT\t1-02:03:04\tTue Oct  6 10:00:00 2026\n",
		"    1 /sbin/launchd\n"+
			"  412 /System/Library/Input Methods/KoreanIM.app/Contents/MacOS/Korean IM\n"+
			"  421 /usr/local/go/bin/go\n")
	f.Add("  500   1 501 100 S 0:00.00 Mon Oct  5 09:00:00 2026\n",
		"  500 /tmp/evil\n    1 claude\n    1 /sbin/launchd\n")
	f.Add("  413     1   501      0 T    0:00.00 Mon Oct  5 09:00:00 2026\n", "  413 Core Audio Driver (Parrot.driver)\nbad line\n")
	f.Add("    0     0     0   1 Ss  0:00.00 Sun Oct 18 11:06:02 2026\n", "    0 kernel_task\n")
	f.Add("  424     1   501  8 0:00.00 Tue Oct  6 10:00:00 2026\n  425 1 501 8 s 0:00.00 Tue Oct  6 10:00:00 2026\n", "  424 x\n")
	f.Fuzz(func(t *testing.T, stats, names string) {
		got, err := parsePS(strings.NewReader(stats), strings.NewReader(names), time.UTC)
		if err != nil {
			t.Fatalf("parsePS: %v", err)
		}
		seen := make(map[int]bool)
		for _, p := range got {
			if p.PID <= 0 || p.PPID < 0 {
				t.Errorf("bad pid/ppid: %+v", p)
			}
			if seen[p.PID] {
				t.Errorf("duplicate pid %d", p.PID)
			}
			seen[p.PID] = true
			if strings.ContainsAny(p.Name, "\n ") {
				t.Errorf("name %q contains a newline or space", p.Name)
			}
			if p.CPUTime < 0 {
				t.Errorf("negative cpu time %v", p.CPUTime)
			}
			switch p.State {
			case domain.StateRunning, domain.StateSleeping, domain.StateStopped, domain.StateZombie, domain.StateOther:
			default:
				t.Errorf("pid %d: state %q", p.PID, p.State)
			}
		}
	})
}

func FuzzPSName(f *testing.F) {
	for _, s := range []string{
		"claude", "claude --api-key=sk-SECRET", "npm run build", "npm exec @scope/server --stdio",
		"/usr/local/go/bin/go", "/usr/bin/x --tok=a/b", "/usr/bin/x secret",
		"/Applications/My App.app/Contents/MacOS/My App", "Core Audio Driver (Parrot.driver)", "/", "a/ /",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, comm string) {
		got := psName(comm)
		if strings.Contains(got, " ") {
			t.Errorf("psName(%q) = %q contains a space", comm, got)
		}
		if !strings.Contains(comm, "\n") && strings.Contains(got, "\n") {
			t.Errorf("psName(%q) = %q contains a newline", comm, got)
		}
	})
}

func FuzzParsePSTime(f *testing.F) {
	for _, s := range []string{
		"0:00.00", "0:01.50", "2:46.63", "2221:05.48", "01:02:03", "3-04:05:06", "1-00:00:00",
		"12", "0:00.123456", "", "-", "1:2:3:4", "1:05.x", "1:00.1234567890",
		"4294967295-4294967295:4294967295:4294967295.999999999",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if d, err := parsePSTime(s); err == nil && d < 0 {
			t.Errorf("parsePSTime(%q) = %v, want >= 0", s, d)
		}
	})
}

func FuzzParsePSIdentity(f *testing.F) {
	for _, s := range []string{
		"  501 Sun Oct  4 11:06:02 2026    \n", "", "501", "501 Sun Oct 4 11:06:02",
		"501 Sun Oct 4 11:06:02 2026 extra", "501 Sun Oct 4 11:06:02 2026\n501 Sun Oct 4 11:06:02 2026",
		"501 Xyz Oct 4 11:06:02 2026",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		start, _, err := parsePSIdentity(b, time.UTC)
		if err == nil && start.IsZero() {
			t.Errorf("parsePSIdentity(%q): zero start without error", b)
		}
	})
}

func FuzzParseProcStat(f *testing.F) {
	f.Add(procStatFixture)
	f.Add("cpu 1 0 1 8 0 0 0 0\ncpu0 1 0 1 8 0 0 0 0\nintr 0 0 0\n")
	f.Add("cpu 1 2 3 4\n")
	f.Add("cpu 1 2 3 4\ncpu 1 2 3 4\n")
	f.Add("cpu 1 1 1 18446744073709551615 0 1\n")
	f.Fuzz(func(t *testing.T, s string) {
		got, ncpu, err := parseProcStat(strings.NewReader(s))
		if err != nil {
			return
		}
		if got.Idle > got.Total || ncpu < 0 {
			t.Errorf("parseProcStat(%q) = %+v, ncpu %d", s, got, ncpu)
		}
	})
}

func FuzzParseLoadavg(f *testing.F) {
	for _, s := range []string{"1.25 0.80 0.50 2/345 6789\n", "", "  \n", "abc 1 2", "-1 0 0", "NaN", "+Inf"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := parseLoadavg(strings.NewReader(s))
		if err == nil && (!(v >= 0) || math.IsInf(v, 0)) {
			t.Errorf("parseLoadavg(%q) = %v, want finite and >= 0", s, v)
		}
	})
}

func FuzzParseMeminfo(f *testing.F) {
	f.Add(meminfoFixture)
	f.Add("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 20 kB\nCached: 300 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	f.Add("MemTotal: 100 kB\nMemAvailable: 200 kB\n")
	f.Add("SwapTotal: 10 kB\nSwapFree: 4 kB\n")
	f.Add("MemTotal: 18014398509481985 kB\nMemAvailable: 1 kB\n")
	f.Fuzz(func(t *testing.T, s string) {
		m, err := parseMeminfo(strings.NewReader(s))
		if err != nil {
			return
		}
		if !m.HasMem && !m.HasSwap || m.MemAvailable > m.MemTotal || m.SwapFree > m.SwapTotal {
			t.Errorf("parseMeminfo(%q) = %+v", s, m)
		}
		if m.HasMem && m.MemTotal == 0 {
			t.Errorf("parseMeminfo(%q): zero MemTotal", s)
		}
		// A kB value is reported in bytes, so it must not wrap.
		for line := range strings.Lines(s) {
			key, rest, _ := strings.Cut(line, ":")
			if f := strings.Fields(rest); key == "MemTotal" && len(f) > 1 && f[1] == "kB" {
				if v, err := strconv.ParseUint(f[0], 10, 64); err == nil && v > math.MaxUint64/1024 {
					t.Errorf("parseMeminfo(%q) accepted %d kB, which overflows", s, v)
				}
			}
		}
	})
}

func FuzzParsePIDStat(f *testing.F) {
	for _, comm := range []string{"bash", "my prog", "a) b (c", ")", "", "x) S 9 9 9", "a\nb"} {
		f.Add([]byte(statLine(comm)))
	}
	f.Add([]byte("x (bash) S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 12345 1000000 300"))
	f.Add([]byte("42 bash) S 1"))
	for _, st := range []string{"T", "t", "Z", "D", "R", "X", "TT", "?"} {
		f.Add([]byte(strings.Replace(statLine("bash"), ") S ", ") "+st+" ", 1)))
	}
	f.Add([]byte("42 (x) S 18446744073709551615 42 42 0 -1 4194560 100 0 0 0 18446744073709551615 50 0 0 20 0 1 0 18446744073709551615 1000000 300"))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := parsePIDStat(b)
		if err != nil {
			return
		}
		if s.PID <= 0 || s.PPID < 0 {
			t.Errorf("parsePIDStat(%q): pid %d ppid %d", b, s.PID, s.PPID)
		}
		if strings.ContainsAny(s.Comm, "\n\r") {
			t.Errorf("parsePIDStat(%q): comm %q contains a newline", b, s.Comm)
		}
		if d := ticksToDuration(s.CPUTicks); d < 0 {
			t.Errorf("parsePIDStat(%q): cpu time %v", b, d)
		}
		if d := ticksToDuration(s.StartTime); d < 0 {
			t.Errorf("parsePIDStat(%q): start offset %v", b, d)
		}
		if s.State == "" {
			t.Errorf("parsePIDStat(%q): no state", b)
		}
	})
}

func FuzzParseStatusUID(f *testing.F) {
	for _, s := range []string{
		"Name:\tbash\nUmask:\t0022\nState:\tS (sleeping)\nPid:\t42\nPPid:\t1\nUid:\t1000\t0\t0\t0\nGid:\t1000\t1000\t1000\t1000\n",
		"Name:\tbash\n", "Uid:\n", "Uid:\tx 0 0 0\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		parseStatusUID(strings.NewReader(s))
	})
}

func FuzzParseBootTime(f *testing.F) {
	for _, s := range []string{procStatFixture, "cpu 1 2 3 4\n", "btime x\n", "btime 0\n", "btime -5\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if bt, err := parseBootTime(strings.NewReader(s)); err == nil && bt.Unix() <= 0 {
			t.Errorf("parseBootTime(%q) = %v", s, bt)
		}
	})
}
