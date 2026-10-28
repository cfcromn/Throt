package platform

// Parsers and rules for Linux cgroup v2 files. Like procfs.go they are pure
// functions and carry no build constraint so they are tested on every OS.

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// parseProcSelfCgroup returns the cgroup v2 path from /proc/self/cgroup: the
// "0::/path" line. ok is false on a cgroup v1-only system. On a hybrid
// system the v2 line is returned too; its hierarchy usually has no
// controllers, so the limit files are absent and the sampler falls back.
func parseProcSelfCgroup(r io.Reader) (p string, ok bool, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, found := strings.CutPrefix(sc.Text(), "0::")
		if found && strings.HasPrefix(rest, "/") {
			return rest, true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	return "", false, nil
}

// parseCgroup2Mount returns the mount point and mounted root of the first
// cgroup2 filesystem in /proc/self/mountinfo. ok is false when there is none.
func parseCgroup2Mount(r io.Reader) (mountPoint, root string, ok bool, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// ID parentID major:minor root mountpoint options [optional...] - fstype source superoptions
		pre, post, found := strings.Cut(sc.Text(), " - ")
		if !found {
			continue
		}
		fields, fs := strings.Fields(pre), strings.Fields(post)
		if len(fields) < 5 || len(fs) == 0 || fs[0] != "cgroup2" {
			continue
		}
		return unescapeMountinfo(fields[4]), unescapeMountinfo(fields[3]), true, nil
	}
	if err := sc.Err(); err != nil {
		return "", "", false, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	return "", "", false, nil
}

// unescapeMountinfo decodes the octal escapes (\040 for space, \011, \012,
// \134) the kernel uses for whitespace and backslashes in mountinfo paths.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// cgroupDirs returns the directories of the cgroup at cgPath and of each of
// its ancestors up to the mount root, nearest first. A limit on an ancestor
// (a systemd slice, a pod) binds the daemon just as one on its own cgroup
// does. ok is false when cgPath lies outside the mounted root, as it does
// for a process outside its cgroup namespace ("/../..").
func cgroupDirs(mountPoint, mountRoot, cgPath string) ([]string, bool) {
	if !path.IsAbs(cgPath) || !path.IsAbs(mountRoot) || slices.Contains(strings.Split(cgPath, "/"), "..") {
		return nil, false
	}
	cgPath, mountRoot = path.Clean(cgPath), path.Clean(mountRoot)
	rel := cgPath
	if mountRoot != "/" {
		r, found := strings.CutPrefix(cgPath, mountRoot)
		if !found || (r != "" && !strings.HasPrefix(r, "/")) {
			return nil, false
		}
		rel = r
	}
	if rel == "" {
		rel = "/"
	}
	var dirs []string
	for {
		dirs = append(dirs, path.Join(mountPoint, rel))
		if rel == "/" {
			return dirs, true
		}
		rel = path.Dir(rel)
	}
}

// parseCgroupLimit parses a single-value limit file such as memory.max or
// memory.swap.max. limited is false for "max".
func parseCgroupLimit(s string) (v uint64, limited bool, err error) {
	s = strings.TrimSpace(s)
	if s == "max" {
		return 0, false, nil
	}
	v, err = strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse cgroup limit: %w", err)
	}
	return v, true, nil
}

// parseCgroupValue parses a single-value counter such as memory.current.
func parseCgroupValue(s string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cgroup value: %w", err)
	}
	return v, nil
}

// parseCPUMax parses cpu.max ("$QUOTA $PERIOD", microseconds) into effective
// CPUs. limited is false when the quota is "max".
func parseCPUMax(s string) (cpus float64, limited bool, err error) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 2 {
		return 0, false, fmt.Errorf("parse cpu.max: malformed %q", s)
	}
	if fields[0] == "max" {
		return 0, false, nil
	}
	if len(fields) != 2 {
		return 0, false, fmt.Errorf("parse cpu.max: quota without period %q", s)
	}
	quota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse cpu.max quota: %w", err)
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse cpu.max period: %w", err)
	}
	if quota == 0 || period == 0 {
		return 0, false, fmt.Errorf("parse cpu.max: zero in %q", s)
	}
	return float64(quota) / float64(period), true, nil
}

// cgroupCPU holds the cpu.stat counters Throt uses, in microseconds.
type cgroupCPU struct {
	Usage     uint64
	Throttled uint64
}

// parseCPUStat reads usage_usec and throttled_usec from cpu.stat.
// throttled_usec is absent when the cpu controller is not enabled for the
// cgroup and then reads as zero; usage_usec is required.
func parseCPUStat(r io.Reader) (cgroupCPU, error) {
	var (
		c        cgroupCPU
		hasUsage bool
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || (fields[0] != "usage_usec" && fields[0] != "throttled_usec") {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return cgroupCPU{}, fmt.Errorf("parse cpu.stat %s: %w", fields[0], err)
		}
		if fields[0] == "usage_usec" {
			c.Usage, hasUsage = v, true
		} else {
			c.Throttled = v
		}
	}
	if err := sc.Err(); err != nil {
		return cgroupCPU{}, fmt.Errorf("read cpu.stat: %w", err)
	}
	if !hasUsage {
		return cgroupCPU{}, errors.New("parse cpu.stat: no usage_usec")
	}
	return c, nil
}

// cgroupCPUBusy returns the busy fraction of a cgroup limited to cpus
// effective CPUs between two cpu.stat readings taken wall apart, and false
// when it is undefined (no time passed, or the counters went backwards
// because the cgroup was recreated).
//
// Rule: any throttling in the interval reports 1.0. Being throttled means the
// cgroup spent its whole quota in at least one period and runnable work then
// waited for the next, which is CPU saturation as the workloads experience
// it, even when usage averaged over the interval reads lower (bursty work
// spends the quota early in each period). Otherwise the fraction is usage
// over the quota's capacity, clamped to 0..1.
func cgroupCPUBusy(prev, cur cgroupCPU, wall time.Duration, cpus float64) (float64, bool) {
	if wall <= 0 || cpus <= 0 || cur.Usage < prev.Usage || cur.Throttled < prev.Throttled {
		return 0, false
	}
	if cur.Throttled > prev.Throttled {
		return 1, true
	}
	capacity := float64(wall.Microseconds()) * cpus
	if capacity <= 0 {
		return 0, false
	}
	return min(float64(cur.Usage-prev.Usage)/capacity, 1), true
}

// parseMemoryStatInactiveFile returns inactive_file from memory.stat, in
// bytes: page cache the kernel can reclaim without swapping.
func parseMemoryStatInactiveFile(r io.Reader) (uint64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		v, found := strings.CutPrefix(sc.Text(), "inactive_file ")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse memory.stat inactive_file: %w", err)
		}
		return n, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read memory.stat: %w", err)
	}
	return 0, errors.New("parse memory.stat: no inactive_file")
}

// cgroupMemory returns MemTotal and MemAvailable for a cgroup with a memory
// limit. Total is the smaller of the limit and the host total. Available is
// total minus the cgroup's non-reclaimable usage (current - inactive_file),
// floored at 0, and never more than the host has available: a container
// cannot use memory the whole machine lacks.
func cgroupMemory(limit, current, inactiveFile, hostTotal, hostAvail uint64) (total, avail uint64) {
	total = min(limit, hostTotal)
	used := current - min(inactiveFile, current)
	if used < total {
		avail = total - used
	}
	return total, min(avail, hostAvail)
}

// The functions below read cgroup files from the directories cgroupDirs
// returns. They hold no OS-specific calls, so tests run them against a
// temporary directory tree on every OS.

// readCgroupFile returns the contents of name in dir.
func readCgroupFile(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	return string(b), err
}

// tightestLimit returns the directory among dirs whose limit file name holds
// the smallest limit, parsed by parse. Directories whose file is missing,
// unreadable or unlimited impose no limit; ok is false when none does.
func tightestLimit[T cmp.Ordered](dirs []string, name string, parse func(string) (T, bool, error)) (dir string, limit T, ok bool) {
	for _, d := range dirs {
		s, err := readCgroupFile(d, name)
		if err != nil {
			continue
		}
		v, limited, err := parse(s)
		if err != nil || !limited {
			continue
		}
		if !ok || v < limit {
			dir, limit, ok = d, v, true
		}
	}
	return dir, limit, ok
}

// cgroupCPULimit returns the cgroup with the tightest CPU quota among dirs
// and its effective CPUs, when that quota is below the ncpu CPUs the host
// has. A quota of ncpu or more never binds, so the host-wide figures stay.
func cgroupCPULimit(dirs []string, ncpu int) (dir string, cpus float64, ok bool) {
	dir, cpus, ok = tightestLimit(dirs, "cpu.max", parseCPUMax)
	if !ok || (ncpu > 0 && cpus >= float64(ncpu)) {
		return "", 0, false
	}
	return dir, cpus, true
}

// loadPerCPU returns the 1-minute load average per CPU, and false when a
// cgroup CPU quota binds. The load average is host-wide (Linux keeps no
// per-cgroup load), so dividing it by the quota's CPUs would count work
// outside the cgroup against the quota and inflate it; dividing it by the
// host CPUs would hide the quota. Neither is meaningful, so it is missing.
func loadPerCPU(load float64, ncpu int, quotaBinds bool) (float64, bool) {
	if quotaBinds || ncpu <= 0 {
		return 0, false
	}
	return load / float64(ncpu), true
}

// cgroupCPUTracker keeps the previous cpu.stat reading of one cgroup to
// compute a busy fraction from deltas.
type cgroupCPUTracker struct {
	dir  string
	prev cgroupCPU
	at   time.Time
	has  bool
}

// sample reads cpu.stat in dir at now and returns the busy fraction since
// the previous reading of the same dir. ok is false on the first reading,
// after the limiting cgroup changed, or when the delta is undefined; err is
// set when cpu.stat could not be read, and then the next reading starts over.
func (t *cgroupCPUTracker) sample(dir string, cpus float64, now time.Time) (busy float64, ok bool, err error) {
	s, err := readCgroupFile(dir, "cpu.stat")
	var cur cgroupCPU
	if err == nil {
		cur, err = parseCPUStat(strings.NewReader(s))
	}
	if err != nil {
		t.has = false
		return 0, false, err
	}
	if t.has && t.dir == dir {
		busy, ok = cgroupCPUBusy(t.prev, cur, now.Sub(t.at), cpus)
	}
	t.dir, t.prev, t.at, t.has = dir, cur, now, true
	return busy, ok, nil
}

// applyCgroupMemory replaces the host memory figures in out with those of
// the cgroup with the tightest memory.max among dirs. It leaves out unchanged
// without a limit or when the cgroup's usage cannot be read.
func applyCgroupMemory(out *domain.HostSample, dirs []string) {
	dir, limit, ok := tightestLimit(dirs, "memory.max", parseCgroupLimit)
	if !ok {
		return
	}
	s, err := readCgroupFile(dir, "memory.current")
	if err != nil {
		return
	}
	current, err := parseCgroupValue(s)
	if err != nil {
		return
	}
	stat, err := readCgroupFile(dir, "memory.stat")
	if err != nil {
		return
	}
	inactive, err := parseMemoryStatInactiveFile(strings.NewReader(stat))
	if err != nil {
		return
	}
	out.MemTotal, out.MemAvailable = cgroupMemory(limit, current, inactive, out.MemTotal, out.MemAvailable)
}

// applyCgroupSwap replaces the host swap figures in out with those of the
// cgroup with the tightest memory.swap.max among dirs: its memory.swap.current
// against the smaller of that limit and the host swap total. It leaves out
// unchanged without a limit or when the usage cannot be read.
func applyCgroupSwap(out *domain.HostSample, dirs []string) {
	dir, limit, ok := tightestLimit(dirs, "memory.swap.max", parseCgroupLimit)
	if !ok {
		return
	}
	s, err := readCgroupFile(dir, "memory.swap.current")
	if err != nil {
		return
	}
	used, err := parseCgroupValue(s)
	if err != nil {
		return
	}
	total := min(limit, out.SwapTotal)
	out.SwapTotal, out.SwapUsed = total, min(used, total)
}
