package platform

import (
	"strings"
	"testing"
)

const procStatFixture = `cpu  10132153 290696 3084719 46828483 16683 0 25195 0 175628 0
cpu0 1393280 32966 572056 13343292 6130 0 17875 0 23933 0
cpu1 1335085 32848 526738 13319025 4306 0 2310 0 22545 0
intr 1462898 1 0 0
ctxt 2541447
btime 1700000000
processes 26442
procs_running 1
procs_blocked 0
`

func TestParseProcStat(t *testing.T) {
	got, ncpu, err := parseProcStat(strings.NewReader(procStatFixture))
	if err != nil {
		t.Fatal(err)
	}
	// user+nice+system+idle+iowait+irq+softirq+steal; guest is excluded.
	wantTotal := uint64(10132153 + 290696 + 3084719 + 46828483 + 16683 + 0 + 25195 + 0)
	wantIdle := uint64(46828483 + 16683)
	if got.Total != wantTotal || got.Idle != wantIdle {
		t.Errorf("got %+v, want Total=%d Idle=%d", got, wantTotal, wantIdle)
	}
	if ncpu != 2 {
		t.Errorf("ncpu = %d, want 2", ncpu)
	}
}

func TestParseProcStatHugeIntrLine(t *testing.T) {
	in := "cpu 1 0 1 8 0 0 0 0\ncpu0 1 0 1 8 0 0 0 0\nintr" + strings.Repeat(" 0", 100_000) + "\n"
	got, ncpu, err := parseProcStat(strings.NewReader(in))
	if err != nil || got.Total != 10 || ncpu != 1 {
		t.Fatalf("got %+v, ncpu=%d, err=%v", got, ncpu, err)
	}
}

func TestParseProcStatOldKernel(t *testing.T) {
	// Kernels before 2.6 report only user, nice, system, idle.
	got, ncpu, err := parseProcStat(strings.NewReader("cpu 1 2 3 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 10 || got.Idle != 4 || ncpu != 0 {
		t.Errorf("got %+v ncpu=%d", got, ncpu)
	}
}

func TestParseProcStatErrors(t *testing.T) {
	tests := map[string]string{
		"empty":     "",
		"no cpu":    "intr 1 2 3\nctxt 4\n",
		"short":     "cpu 1 2 3\n",
		"non-num":   "cpu 1 2 x 4 5\n",
		"negative":  "cpu 1 2 -3 4 5\n",
		"duplicate": "cpu 1 2 3 4\ncpu 1 2 3 4\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseProcStat(strings.NewReader(in)); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestBusySince(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur cpuTimes
		want      float64
		ok        bool
	}{
		{"half busy", cpuTimes{100, 50}, cpuTimes{200, 100}, 0.5, true},
		{"idle", cpuTimes{100, 50}, cpuTimes{200, 150}, 0, true},
		{"fully busy", cpuTimes{100, 50}, cpuTimes{200, 50}, 1, true},
		{"no progress", cpuTimes{100, 50}, cpuTimes{100, 50}, 0, false},
		{"total went back", cpuTimes{200, 50}, cpuTimes{100, 50}, 0, false},
		{"idle went back", cpuTimes{100, 50}, cpuTimes{200, 40}, 0, false},
		{"idle exceeds total", cpuTimes{100, 50}, cpuTimes{110, 100}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.cur.busySince(tt.prev)
			if got != tt.want || ok != tt.ok {
				t.Errorf("busySince = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseLoadavg(t *testing.T) {
	got, err := parseLoadavg(strings.NewReader("1.25 0.80 0.50 2/345 6789\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 1.25 {
		t.Errorf("load1 = %v, want 1.25", got)
	}
	for _, bad := range []string{"", "  \n", "abc 1 2", "-1 0 0"} {
		if _, err := parseLoadavg(strings.NewReader(bad)); err == nil {
			t.Errorf("parseLoadavg(%q): want error", bad)
		}
	}
}

const meminfoFixture = `MemTotal:       16303428 kB
MemFree:         1234567 kB
MemAvailable:    8000000 kB
Buffers:          200000 kB
Cached:          5000000 kB
SwapCached:            0 kB
Active:          6000000 kB
SwapTotal:       2097148 kB
SwapFree:        1048574 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
`

func TestParseMeminfo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want memInfo
	}{
		{
			name: "full",
			in:   meminfoFixture,
			want: memInfo{
				MemTotal: 16303428 * 1024, MemAvailable: 8000000 * 1024,
				SwapTotal: 2097148 * 1024, SwapFree: 1048574 * 1024,
				HasMem: true, HasSwap: true,
			},
		},
		{
			name: "no MemAvailable falls back",
			in:   "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 20 kB\nCached: 300 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
			want: memInfo{MemTotal: 1000 * 1024, MemAvailable: 420 * 1024, HasMem: true, HasSwap: true},
		},
		{
			name: "available clamped to total",
			in:   "MemTotal: 100 kB\nMemAvailable: 200 kB\n",
			want: memInfo{MemTotal: 100 * 1024, MemAvailable: 100 * 1024, HasMem: true},
		},
		{
			name: "swap only",
			in:   "SwapTotal: 10 kB\nSwapFree: 4 kB\n",
			want: memInfo{SwapTotal: 10 * 1024, SwapFree: 4 * 1024, HasSwap: true},
		},
		{
			name: "total without available or free",
			in:   "MemTotal: 100 kB\nSwapTotal: 10 kB\nSwapFree: 10 kB\n",
			want: memInfo{SwapTotal: 10 * 1024, SwapFree: 10 * 1024, HasSwap: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMeminfo(strings.NewReader(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseMeminfoErrors(t *testing.T) {
	tests := map[string]string{
		"empty":       "",
		"unrelated":   "Active: 10 kB\n",
		"bad number":  "MemTotal: lots kB\n",
		"no value":    "MemTotal:\n",
		"zero total":  "MemTotal: 0 kB\nMemAvailable: 0 kB\n",
		"swap halves": "SwapTotal: 10 kB\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMeminfo(strings.NewReader(in)); err == nil {
				t.Error("want error")
			}
		})
	}
}
