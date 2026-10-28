package platform

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestParseProcSelfCgroup(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"v2 namespaced", "0::/\n", "/", true},
		{"v2 nested", "0::/system.slice/throtd.service\n", "/system.slice/throtd.service", true},
		{"v1 only", "12:cpu,cpuacct:/docker/abc\n11:memory:/docker/abc\n1:name=systemd:/docker/abc\n", "", false},
		{
			"hybrid",
			"12:cpu,cpuacct:/user.slice\n1:name=systemd:/user.slice/session-1.scope\n0::/user.slice/session-1.scope\n",
			"/user.slice/session-1.scope", true,
		},
		{"outside namespace", "0::/../../system.slice\n", "/../../system.slice", true},
		{"empty", "", "", false},
		{"v2 without path", "0::\n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := parseProcSelfCgroup(strings.NewReader(tt.in))
			if err != nil || got != tt.want || ok != tt.ok {
				t.Errorf("got %q, %v, %v; want %q, %v", got, ok, err, tt.want, tt.ok)
			}
		})
	}
}

func TestParseCgroup2Mount(t *testing.T) {
	tests := []struct {
		name             string
		in               string
		mountPoint, root string
		ok               bool
	}{
		{
			"docker",
			"1205 1204 0:28 / / rw,relatime master:1 - overlay overlay rw\n" +
				"1210 1205 0:30 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw,nsdelegate\n",
			"/sys/fs/cgroup", "/", true,
		},
		{
			"bind-mounted subtree",
			"30 25 0:26 /kubepods/pod1 /sys/fs/cgroup rw,nosuid shared:4 master:2 - cgroup2 cgroup2 rw\n",
			"/sys/fs/cgroup", "/kubepods/pod1", true,
		},
		{
			"hybrid unified",
			"31 25 0:27 / /sys/fs/cgroup/systemd rw - cgroup cgroup rw,name=systemd\n" +
				"32 25 0:28 / /sys/fs/cgroup/unified rw shared:5 - cgroup2 cgroup2 rw\n",
			"/sys/fs/cgroup/unified", "/", true,
		},
		{
			"escaped space",
			"32 25 0:28 / /mnt/cg\\040two rw - cgroup2 none rw\n",
			"/mnt/cg two", "/", true,
		},
		{
			"v1 only",
			"31 25 0:27 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n",
			"", "", false,
		},
		{"empty", "", "", "", false},
		{"no separator", "32 25 0:28 / /sys/fs/cgroup rw cgroup2 none rw\n", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp, root, ok, err := parseCgroup2Mount(strings.NewReader(tt.in))
			if err != nil || mp != tt.mountPoint || root != tt.root || ok != tt.ok {
				t.Errorf("got %q, %q, %v, %v; want %q, %q, %v", mp, root, ok, err, tt.mountPoint, tt.root, tt.ok)
			}
		})
	}
}

func TestCgroupDirs(t *testing.T) {
	tests := []struct {
		name              string
		mount, root, path string
		want              []string
	}{
		{"namespaced root", "/sys/fs/cgroup", "/", "/", []string{"/sys/fs/cgroup"}},
		{
			"nested", "/sys/fs/cgroup", "/", "/system.slice/throtd.service",
			[]string{"/sys/fs/cgroup/system.slice/throtd.service", "/sys/fs/cgroup/system.slice", "/sys/fs/cgroup"},
		},
		{
			"mounted subtree", "/sys/fs/cgroup", "/kubepods/pod1", "/kubepods/pod1/ctr",
			[]string{"/sys/fs/cgroup/ctr", "/sys/fs/cgroup"},
		},
		{"subtree root", "/sys/fs/cgroup", "/kubepods/pod1", "/kubepods/pod1", []string{"/sys/fs/cgroup"}},
		{"outside subtree", "/sys/fs/cgroup", "/kubepods/pod1", "/kubepods/pod10", nil},
		{"outside namespace", "/sys/fs/cgroup", "/", "/../../system.slice", nil},
		{"relative", "/sys/fs/cgroup", "/", "system.slice", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cgroupDirs(tt.mount, tt.root, tt.path)
			if ok != (tt.want != nil) || !slices.Equal(got, tt.want) {
				t.Errorf("got %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}

func TestParseCPUMax(t *testing.T) {
	tests := []struct {
		in      string
		cpus    float64
		limited bool
		err     bool
	}{
		{"max 100000\n", 0, false, false},
		{"max\n", 0, false, false},
		{"200000 100000\n", 2, true, false},
		{"50000 100000", 0.5, true, false},
		{"150000 100000", 1.5, true, false},
		{"", 0, false, true},
		{"200000\n", 0, false, true},
		{"x 100000", 0, false, true},
		{"200000 y", 0, false, true},
		{"0 100000", 0, false, true},
		{"200000 0", 0, false, true},
		{"1 2 3", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			cpus, limited, err := parseCPUMax(tt.in)
			if (err != nil) != tt.err || cpus != tt.cpus || limited != tt.limited {
				t.Errorf("got %v, %v, %v; want %v, %v, err=%v", cpus, limited, err, tt.cpus, tt.limited, tt.err)
			}
		})
	}
}

func TestParseCPUStat(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want cgroupCPU
		err  bool
	}{
		{
			"full",
			"usage_usec 8001234\nuser_usec 7000000\nsystem_usec 1001234\nnr_periods 120\nnr_throttled 30\nthrottled_usec 450000\nnr_bursts 0\nburst_usec 0\n",
			cgroupCPU{Usage: 8001234, Periods: 120, Throttled: 30}, false,
		},
		{"no cpu controller", "usage_usec 42\nuser_usec 40\nsystem_usec 2\n", cgroupCPU{Usage: 42}, false},
		{"no usage", "nr_periods 1\nnr_throttled 1\n", cgroupCPU{}, true},
		{"bad usage", "usage_usec many\n", cgroupCPU{}, true},
		{"bad periods", "usage_usec 1\nnr_periods x\n", cgroupCPU{}, true},
		{"bad throttled", "usage_usec 1\nnr_throttled -1\n", cgroupCPU{}, true},
		{"empty", "", cgroupCPU{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCPUStat(strings.NewReader(tt.in))
			if (err != nil) != tt.err || got != tt.want {
				t.Errorf("got %+v, %v; want %+v, err=%v", got, err, tt.want, tt.err)
			}
		})
	}
}

func TestCgroupCPUBusy(t *testing.T) {
	sec := time.Second
	tests := []struct {
		name      string
		prev, cur cgroupCPU
		wall      time.Duration
		cpus      float64
		want      float64
		ok        bool
	}{
		// cgroupCPU{Usage µs, Periods, Throttled periods}; 1s is 10 periods of 100ms.
		{"idle", cgroupCPU{1000, 0, 0}, cgroupCPU{1000, 10, 0}, sec, 2, 0, true},
		{"half of quota", cgroupCPU{0, 0, 0}, cgroupCPU{1_000_000, 10, 0}, sec, 2, 0.5, true},
		{"full quota", cgroupCPU{0, 0, 0}, cgroupCPU{2_000_000, 10, 10}, sec, 2, 1, true},
		{"over quota clamps", cgroupCPU{0, 0, 0}, cgroupCPU{2_100_000, 10, 0}, sec, 2, 1, true},
		{"fractional quota", cgroupCPU{0, 0, 0}, cgroupCPU{250_000, 10, 0}, sec, 0.5, 0.5, true},
		{"throttled every period, bursty usage", cgroupCPU{0, 0, 0}, cgroupCPU{400_000, 10, 10}, sec, 2, 1, true},
		{"throttled most periods", cgroupCPU{0, 0, 0}, cgroupCPU{400_000, 10, 9}, sec, 2, 0.9, true},
		{"throttled once, usage wins", cgroupCPU{0, 0, 0}, cgroupCPU{1_000_000, 10, 1}, sec, 2, 0.5, true},
		{"throttled once, low usage", cgroupCPU{0, 0, 0}, cgroupCPU{200_000, 10, 1}, sec, 2, 0.1, true},
		{"throttled earlier only", cgroupCPU{0, 100, 50}, cgroupCPU{400_000, 110, 50}, sec, 2, 0.2, true},
		{"no periods counted", cgroupCPU{0, 0, 0}, cgroupCPU{400_000, 0, 0}, sec, 2, 0.2, true},
		{"usage went backwards", cgroupCPU{5000, 0, 0}, cgroupCPU{4000, 10, 0}, sec, 2, 0, false},
		{"periods went backwards", cgroupCPU{0, 10, 0}, cgroupCPU{1000, 5, 0}, sec, 2, 0, false},
		{"throttled went backwards", cgroupCPU{0, 0, 10}, cgroupCPU{1000, 20, 5}, sec, 2, 0, false},
		{"no wall time", cgroupCPU{0, 0, 0}, cgroupCPU{1000, 0, 0}, 0, 2, 0, false},
		{"sub-microsecond wall", cgroupCPU{0, 0, 0}, cgroupCPU{1000, 0, 0}, time.Nanosecond, 2, 0, false},
		{"no cpus", cgroupCPU{0, 0, 0}, cgroupCPU{1000, 0, 0}, sec, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cgroupCPUBusy(tt.prev, tt.cur, tt.wall, tt.cpus)
			if got != tt.want || ok != tt.ok {
				t.Errorf("got %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestLoadPerCPU(t *testing.T) {
	tests := []struct {
		name       string
		load       float64
		ncpu       int
		quotaBinds bool
		want       float64
		ok         bool
	}{
		{"no quota", 5, 10, false, 0.5, true},
		{"idle", 0, 4, false, 0, true},
		{"quota binds", 5, 10, true, 0, false},
		{"quota binds while idle", 0, 10, true, 0, false},
		{"no cpu count", 5, 0, false, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := loadPerCPU(tt.load, tt.ncpu, tt.quotaBinds)
			if got != tt.want || ok != tt.ok {
				t.Errorf("got %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseCgroupLimit(t *testing.T) {
	tests := []struct {
		in      string
		v       uint64
		limited bool
		err     bool
	}{
		{"max\n", 0, false, false},
		{"1073741824\n", 1 << 30, true, false},
		{"0", 0, true, false},
		{"", 0, false, true},
		{"-1", 0, false, true},
		{"lots", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			v, limited, err := parseCgroupLimit(tt.in)
			if (err != nil) != tt.err || v != tt.v || limited != tt.limited {
				t.Errorf("got %v, %v, %v", v, limited, err)
			}
		})
	}
}

func TestParseMemoryStatInactiveFile(t *testing.T) {
	const fixture = "anon 104857600\nfile 52428800\nkernel 1048576\nactive_anon 0\ninactive_anon 104857600\nactive_file 20971520\ninactive_file 31457280\nunevictable 0\n"
	got, err := parseMemoryStatInactiveFile(strings.NewReader(fixture))
	if err != nil || got != 31457280 {
		t.Errorf("got %d, %v; want 31457280", got, err)
	}
	for name, in := range map[string]string{
		"missing": "anon 1\nactive_file 2\n",
		"bad":     "inactive_file x\n",
		"empty":   "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMemoryStatInactiveFile(strings.NewReader(in)); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestCgroupLimitsApply(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/", true},                             // root of a private cgroup namespace: containerized
		{"/system.slice/throtd.service", false}, // systemd system unit (CPUQuota=, MemoryMax=)
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/throtd.service", false}, // systemd user unit
		{"/docker/0123abcd", false},   // container without its own cgroup namespace
		{"/kubepods/pod1/ctr", false}, // likewise
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := cgroupLimitsApply(tt.path); got != tt.want {
				t.Errorf("cgroupLimitsApply(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestLeastHeadroom(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name   string
		levels []cgroupLevel
		want   cgroupLevel
		ok     bool
	}{
		{"none", nil, cgroupLevel{}, false},
		{"one", []cgroupLevel{{Limit: gib, Used: gib / 4}}, cgroupLevel{Limit: gib, Used: gib / 4}, true},
		{"smaller limit, more headroom", []cgroupLevel{{Limit: 4 * gib, Used: gib}, {Limit: 8 * gib, Used: 7 * gib}}, cgroupLevel{Limit: 8 * gib, Used: 7 * gib}, true},
		{"smaller limit, less headroom", []cgroupLevel{{Limit: gib, Used: gib / 2}, {Limit: 8 * gib, Used: gib}}, cgroupLevel{Limit: gib, Used: gib / 2}, true},
		{"over limit is zero headroom", []cgroupLevel{{Limit: 8 * gib, Used: gib}, {Limit: gib, Used: 2 * gib}}, cgroupLevel{Limit: gib, Used: 2 * gib}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := leastHeadroom(tt.levels)
			if got != tt.want || ok != tt.ok {
				t.Errorf("got %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCgroupMemory(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name               string
		lvl                cgroupLevel
		hostTotal, hostAvl uint64
		total, avail       uint64
	}{
		{"limit below host", cgroupLevel{Limit: gib, Used: gib / 4}, 8 * gib, 6 * gib, gib, 3 * gib / 4},
		{"limit above host", cgroupLevel{Limit: 16 * gib, Used: gib}, 8 * gib, 6 * gib, 8 * gib, 6 * gib},
		{"headroom above host total", cgroupLevel{Limit: 16 * gib, Used: 0}, 8 * gib, 8 * gib, 8 * gib, 8 * gib},
		{"over limit floors at zero", cgroupLevel{Limit: gib, Used: 2 * gib}, 8 * gib, 6 * gib, gib, 0},
		{"host has less available", cgroupLevel{Limit: gib, Used: 0}, 8 * gib, gib / 8, gib, gib / 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, avail := cgroupMemory(tt.lvl, tt.hostTotal, tt.hostAvl)
			if total != tt.total || avail != tt.avail {
				t.Errorf("got %d, %d; want %d, %d", total, avail, tt.total, tt.avail)
			}
		})
	}
}

func TestCgroupSwap(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name        string
		lvl         cgroupLevel
		hostTotal   uint64
		total, used uint64
	}{
		{"limit below host", cgroupLevel{Limit: gib, Used: gib / 4}, 4 * gib, gib, gib / 4},
		{"limit above host", cgroupLevel{Limit: 8 * gib, Used: gib}, 4 * gib, 4 * gib, gib},
		{"over limit", cgroupLevel{Limit: gib, Used: 2 * gib}, 4 * gib, gib, gib},
		{"no host swap", cgroupLevel{Limit: gib, Used: 0}, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, used := cgroupSwap(tt.lvl, tt.hostTotal)
			if total != tt.total || used != tt.used {
				t.Errorf("got %d, %d; want %d, %d", total, used, tt.total, tt.used)
			}
		})
	}
}

// writeCgroup creates dir with the given files.
func writeCgroup(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCgroupCPULimit(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	writeCgroup(t, root, map[string]string{"cpu.max": "200000 100000\n"})
	writeCgroup(t, child, map[string]string{"cpu.max": "max 100000\n"})
	dirs := []string{child, root}

	if dir, cpus, ok := cgroupCPULimit(dirs, 10); !ok || dir != root || cpus != 2 {
		t.Errorf("ancestor limit: got %q, %v, %v", dir, cpus, ok)
	}
	if _, _, ok := cgroupCPULimit(dirs, 2); ok {
		t.Error("quota equal to host CPUs should not bind")
	}
	writeCgroup(t, child, map[string]string{"cpu.max": "50000 100000\n"})
	if dir, cpus, ok := cgroupCPULimit(dirs, 10); !ok || dir != child || cpus != 0.5 {
		t.Errorf("tighter child: got %q, %v, %v", dir, cpus, ok)
	}
	if _, _, ok := cgroupCPULimit(nil, 10); ok {
		t.Error("no cgroup should not bind")
	}
	if _, _, ok := cgroupCPULimit([]string{filepath.Join(root, "missing")}, 10); ok {
		t.Error("missing cpu.max should not bind")
	}
}

func TestCgroupCPUTracker(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	writeCgroup(t, a, map[string]string{"cpu.stat": "usage_usec 0\nnr_periods 0\nnr_throttled 0\n"})
	writeCgroup(t, b, map[string]string{"cpu.stat": "usage_usec 0\nnr_periods 0\nnr_throttled 0\n"})
	var tr cgroupCPUTracker
	t0 := time.Unix(1000, 0)

	if _, ok, err := tr.sample(a, 2, t0); ok || err != nil {
		t.Fatalf("first reading: ok=%v err=%v", ok, err)
	}
	writeCgroup(t, a, map[string]string{"cpu.stat": "usage_usec 1000000\nnr_periods 10\nnr_throttled 0\n"})
	if busy, ok, err := tr.sample(a, 2, t0.Add(time.Second)); !ok || err != nil || busy != 0.5 {
		t.Fatalf("second reading: %v, %v, %v", busy, ok, err)
	}
	writeCgroup(t, a, map[string]string{"cpu.stat": "usage_usec 1100000\nnr_periods 20\nnr_throttled 8\n"})
	if busy, ok, _ := tr.sample(a, 2, t0.Add(2*time.Second)); !ok || busy != 0.8 {
		t.Fatalf("throttled: %v, %v", busy, ok)
	}
	if _, ok, _ := tr.sample(b, 2, t0.Add(3*time.Second)); ok {
		t.Fatal("changed cgroup should restart the delta")
	}
	if _, ok, err := tr.sample(filepath.Join(a, "missing"), 2, t0.Add(4*time.Second)); ok || err == nil {
		t.Fatalf("missing cpu.stat: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := tr.sample(b, 2, t0.Add(5*time.Second)); ok {
		t.Fatal("a read error should restart the delta")
	}
}

func TestApplyCgroupMemory(t *testing.T) {
	const gib = 1 << 30
	host := domain.HostSample{MemTotal: 8 * gib, MemAvailable: 6 * gib, SwapTotal: 2 * gib, SwapUsed: gib}
	limited := map[string]string{
		"memory.max":          "1073741824\n",
		"memory.current":      "536870912\n",
		"memory.stat":         "anon 268435456\ninactive_file 268435456\n",
		"memory.swap.max":     "1073741824\n",
		"memory.swap.current": "104857600\n",
	}
	tests := []struct {
		name  string
		files map[string]string
		want  domain.HostSample
	}{
		{"limited", limited, domain.HostSample{MemTotal: gib, MemAvailable: 3 * gib / 4, SwapTotal: gib, SwapUsed: 100 << 20}},
		{"unlimited", map[string]string{"memory.max": "max\n", "memory.swap.max": "max\n"}, host},
		{"no files", nil, host},
		{"unreadable usage", map[string]string{"memory.max": "1073741824\n", "memory.swap.max": "1073741824\n"}, host},
		{
			"bad stat",
			map[string]string{"memory.max": "1073741824\n", "memory.current": "1\n", "memory.stat": "anon 1\n"},
			host,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCgroup(t, dir, tt.files)
			got := host
			applyCgroupMemory(&got, []string{dir})
			applyCgroupSwap(&got, []string{dir})
			if got.MemTotal != tt.want.MemTotal || got.MemAvailable != tt.want.MemAvailable ||
				got.SwapTotal != tt.want.SwapTotal || got.SwapUsed != tt.want.SwapUsed {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestApplyCgroupHeadroom is the review scenario: a leaf with a 4G limit and
// 1G used sits in a pod with an 8G limit and 7.9G used. The pod binds, so
// about 100M is available, not the leaf's 3G.
func TestApplyCgroupHeadroom(t *testing.T) {
	const (
		mib = 1 << 20
		gib = 1 << 30
	)
	pod := t.TempDir()
	leaf := filepath.Join(pod, "leaf")
	num := func(v uint64) string { return strconv.FormatUint(v, 10) + "\n" }
	writeCgroup(t, pod, map[string]string{
		"memory.max": num(8 * gib), "memory.current": num(8*gib - 100*mib), "memory.stat": "inactive_file 0\n",
		"memory.swap.max": num(2 * gib), "memory.swap.current": num(2*gib - 50*mib),
	})
	writeCgroup(t, leaf, map[string]string{
		"memory.max": num(4 * gib), "memory.current": num(gib), "memory.stat": "inactive_file 0\n",
		"memory.swap.max": num(gib), "memory.swap.current": num(0),
	})
	got := domain.HostSample{MemTotal: 16 * gib, MemAvailable: 12 * gib, SwapTotal: 4 * gib}
	applyCgroupMemory(&got, []string{leaf, pod})
	applyCgroupSwap(&got, []string{leaf, pod})
	if got.MemTotal != 8*gib || got.MemAvailable != 100*mib {
		t.Errorf("memory: total %d, available %d; want %d, %d", got.MemTotal, got.MemAvailable, uint64(8*gib), uint64(100*mib))
	}
	if got.SwapTotal != 2*gib || got.SwapUsed != 2*gib-50*mib {
		t.Errorf("swap: total %d, used %d; want the pod's 2G with 50M headroom", got.SwapTotal, got.SwapUsed)
	}

	// A limited level whose usage cannot be read keeps the host figures.
	if err := os.Remove(filepath.Join(pod, "memory.current")); err != nil {
		t.Fatal(err)
	}
	host := domain.HostSample{MemTotal: 16 * gib, MemAvailable: 12 * gib}
	got = host
	applyCgroupMemory(&got, []string{leaf, pod})
	if got.MemTotal != host.MemTotal || got.MemAvailable != host.MemAvailable {
		t.Errorf("unreadable pod usage: got %+v, want host figures", got)
	}
}

func TestMemoryPressureDirFollowsLeastHeadroom(t *testing.T) {
	// A pod nearly at its 8G limit holds a leaf with a calm 4G limit: the
	// pod's memory.pressure is the one that reflects the squeeze.
	const gib = 1 << 30
	mk := func(limit, current uint64) string {
		d := t.TempDir()
		for name, v := range map[string]string{
			"memory.max":     strconv.FormatUint(limit, 10),
			"memory.current": strconv.FormatUint(current, 10),
			"memory.stat":    "anon 1\ninactive_file 0\n",
		} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(v+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	leaf, pod := mk(4*gib, gib), mk(8*gib, 8*gib-100<<20)
	if got := memoryPressureDir([]string{leaf, pod}); got != pod {
		t.Errorf("memoryPressureDir = %s, want the pod %s", got, pod)
	}
	if got := memoryPressureDir(nil); got != "" {
		t.Errorf("no cgroup: %q, want host", got)
	}
}
