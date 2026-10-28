package platform

import (
	"math"
	"path"
	"strings"
	"testing"
	"time"
)

// Fuzz targets for the cgroup v2 parsers. A container's cgroup files and
// mountinfo come from the kernel, but paths in them are named by whoever
// created the cgroup or the mount, so they are treated as untrusted.

func FuzzParseProcSelfCgroup(f *testing.F) {
	for _, s := range []string{
		"0::/user.slice/user-1000.slice/session-2.scope\n",
		"12:memory:/docker/abc\n0::/\n", "1:name=systemd:/\n", "0::\n", "0::relative\n", "", "0::/a\x00b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, ok, err := parseProcSelfCgroup(strings.NewReader(s))
		if ok && (err != nil || !strings.HasPrefix(p, "/")) {
			t.Fatalf("%q: path %q, ok, err %v", s, p, err)
		}
		if !ok && p != "" {
			t.Fatalf("%q: path %q without ok", s, p)
		}
	})
}

func FuzzCgroupMountAndDirs(f *testing.F) {
	f.Add("29 23 0:26 / /sys/fs/cgroup rw,nosuid shared:4 - cgroup2 cgroup2 rw\n", "/user.slice/a.scope")
	f.Add("29 23 0:26 /docker/abc /sys/fs/cgroup rw - cgroup2 cgroup2 rw\n", "/docker/abc/x")
	f.Add("29 23 0:26 /a\\040b /mnt\\134x rw - cgroup2 none rw\n", "/a b")
	f.Add("29 23 0:26 / /sys/fs/cgroup rw - cgroup cgroup rw\n", "/")
	f.Add("1 2 3 /x /y - cgroup2\n", "/../..")
	f.Add("1 2 3 /x/ /y - cgroup2\n", "/xx")
	f.Add(" - cgroup2", "")
	f.Fuzz(func(t *testing.T, mountinfo, cgPath string) {
		mp, root, ok, err := parseCgroup2Mount(strings.NewReader(mountinfo))
		if !ok {
			if mp != "" || root != "" {
				t.Fatalf("%q: mount %q root %q without ok (err %v)", mountinfo, mp, root, err)
			}
			mp, root = "/sys/fs/cgroup", "/"
		}
		dirs, ok := cgroupDirs(mp, root, cgPath)
		if !ok {
			if dirs != nil {
				t.Fatalf("cgroupDirs(%q, %q, %q) = %q without ok", mp, root, cgPath, dirs)
			}
			return
		}
		if len(dirs) == 0 || len(dirs) > strings.Count(cgPath, "/")+1 {
			t.Fatalf("cgroupDirs(%q, %q, %q): %d dirs %q", mp, root, cgPath, len(dirs), dirs)
		}
		top := path.Clean(mp)
		if dirs[len(dirs)-1] != top {
			t.Fatalf("cgroupDirs(%q, %q, %q): last dir %q, want the mount point %q", mp, root, cgPath, dirs[len(dirs)-1], top)
		}
		for _, d := range dirs {
			if d != top && !strings.HasPrefix(d, strings.TrimSuffix(top, "/")+"/") {
				t.Fatalf("cgroupDirs(%q, %q, %q): %q is outside the mount point", mp, root, cgPath, d)
			}
		}
	})
}

func FuzzUnescapeMountinfo(f *testing.F) {
	for _, s := range []string{`/a\040b`, `\134`, `\`, `\0`, `\777`, `\400`, `x\011y\012`, `\\040`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if u := unescapeMountinfo(s); len(u) > len(s) {
			t.Fatalf("unescapeMountinfo(%q) = %q grew", s, u)
		}
	})
}

func FuzzParseCgroupFiles(f *testing.F) {
	for _, s := range []string{
		"max\n", "1073741824\n", "0", "-1", "18446744073709551616", " 42 ",
		"max 100000\n", "50000 100000\n", "1 18446744073709551615", "0 100000", "100000 0", "200000", "a b c",
		"usage_usec 100\nnr_periods 3\nnr_throttled 1\n", "usage_usec x\n", "nr_periods 1\n",
		"anon 1\ninactive_file 4096\n", "inactive_file -1\n", "inactive_file \n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if v, limited, err := parseCgroupLimit(s); err != nil && (v != 0 || limited) {
			t.Fatalf("parseCgroupLimit(%q) = %d, %v with error %v", s, v, limited, err)
		}
		parseCgroupValue(s)
		cpus, limited, err := parseCPUMax(s)
		if limited && (err != nil || !(cpus > 0) || math.IsInf(cpus, 0)) {
			t.Fatalf("parseCPUMax(%q) = %v, limited, err %v", s, cpus, err)
		}
		if !limited && cpus != 0 {
			t.Fatalf("parseCPUMax(%q) = %v without a limit", s, cpus)
		}
		parseMemoryStatInactiveFile(strings.NewReader(s))
		prev, err := parseCPUStat(strings.NewReader(s))
		if err != nil {
			return
		}
		cur := cgroupCPU{Usage: prev.Usage + 1e6, Periods: prev.Periods + 10, Throttled: prev.Throttled + 1}
		for _, c := range []float64{cpus, 1, 1e-300, 1e300} {
			if busy, ok := cgroupCPUBusy(prev, cur, time.Second, c); ok && !(busy >= 0 && busy <= 1) {
				t.Fatalf("cgroupCPUBusy(%+v, %+v, 1s, %v) = %v", prev, cur, c, busy)
			}
		}
	})
}
