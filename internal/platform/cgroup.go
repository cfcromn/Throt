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

// cgroupLimitsApply reports whether the limits of the daemon's cgroup, at
// cgPath from /proc/self/cgroup, describe the machine the agents share. That
// holds only at "/", the root of a private cgroup namespace, where the
// daemon runs containerized beside the agents. Elsewhere, for example in a
// systemd unit with CPUQuota= or MemoryMax=, the limits confine throtd
// alone, and the agents it watches run outside them, so host-wide figures
// apply. (On a host, "/" is the real root cgroup, which has no limit files,
// so the figures fall back to host-wide anyway.)
func cgroupLimitsApply(cgPath string) bool {
	return cgPath == "/"
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

// cgroupCPU holds the cpu.stat counters Throt uses: CPU time in
// microseconds, and the count of enforcement periods and of those in which
// the cgroup was throttled.
type cgroupCPU struct {
	Usage     uint64
	Periods   uint64
	Throttled uint64
}

// parseCPUStat reads usage_usec, nr_periods and nr_throttled from cpu.stat.
// The period counters are absent when the cpu controller is not enabled for
// the cgroup and then read as zero; usage_usec is required.
func parseCPUStat(r io.Reader) (cgroupCPU, error) {
	var (
		c        cgroupCPU
		hasUsage bool
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		var dst *uint64
		switch fields[0] {
		case "usage_usec":
			dst, hasUsage = &c.Usage, true
		case "nr_periods":
			dst = &c.Periods
		case "nr_throttled":
			dst = &c.Throttled
		default:
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return cgroupCPU{}, fmt.Errorf("parse cpu.stat %s: %w", fields[0], err)
		}
		*dst = v
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
// Rule: busy = max(usage over the quota's capacity, the fraction of
// enforcement periods that were throttled), clamped to 0..1. A throttled
// period is one in which runnable work waited for quota, so work throttled
// every period reads fully busy even when bursty usage averages lower, while
// a container throttled only now and then reads about its usage.
func cgroupCPUBusy(prev, cur cgroupCPU, wall time.Duration, cpus float64) (float64, bool) {
	if wall <= 0 || cpus <= 0 || cur.Usage < prev.Usage || cur.Periods < prev.Periods || cur.Throttled < prev.Throttled {
		return 0, false
	}
	capacity := float64(wall.Microseconds()) * cpus
	if capacity <= 0 {
		return 0, false
	}
	busy := float64(cur.Usage-prev.Usage) / capacity
	if periods := cur.Periods - prev.Periods; periods > 0 {
		busy = max(busy, float64(cur.Throttled-prev.Throttled)/float64(periods))
	}
	return min(busy, 1), true
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

// cgroupLevel is one limited level of the cgroup hierarchy: its limit and
// the usage counted against it, in bytes.
type cgroupLevel struct {
	Limit, Used uint64
	// Dir is the cgroup directory the level was read from.
	Dir string
}

// headroom is how much more the level can use, floored at 0.
func (l cgroupLevel) headroom() uint64 {
	if l.Used >= l.Limit {
		return 0
	}
	return l.Limit - l.Used
}

// leastHeadroom returns the level with the least headroom; ok is false for
// no levels. Headroom, not the smallest limit, decides: a nearly full pod
// binds before a roomy container inside it whose own limit is smaller.
func leastHeadroom(levels []cgroupLevel) (lvl cgroupLevel, ok bool) {
	for i, l := range levels {
		if i == 0 || l.headroom() < lvl.headroom() {
			lvl = l
		}
	}
	return lvl, len(levels) > 0
}

// cgroupMemory returns MemTotal and MemAvailable for the binding memory
// level. Total is the smaller of its limit and the host total. Available is
// its headroom, never more than total or than the host has available: a
// container cannot use memory the whole machine lacks.
func cgroupMemory(lvl cgroupLevel, hostTotal, hostAvail uint64) (total, avail uint64) {
	total = min(lvl.Limit, hostTotal)
	return total, min(lvl.headroom(), total, hostAvail)
}

// cgroupSwap returns SwapTotal and SwapUsed for the binding swap level:
// total is the smaller of its limit and the host swap total, and used is
// its usage, at most total.
func cgroupSwap(lvl cgroupLevel, hostTotal uint64) (total, used uint64) {
	total = min(lvl.Limit, hostTotal)
	return total, min(lvl.Used, total)
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

// readLevels returns every level among dirs whose limit file sets a limit,
// with the usage used reports for it. Directories whose limit file is
// missing, unreadable or "max" impose no limit. An error reading the usage
// of a limited level fails the whole read, so the caller keeps the host
// figure rather than ignore a level that may bind.
func readLevels(dirs []string, limitFile string, used func(dir string) (uint64, error)) ([]cgroupLevel, error) {
	var levels []cgroupLevel
	for _, d := range dirs {
		s, err := readCgroupFile(d, limitFile)
		if err != nil {
			continue
		}
		limit, limited, err := parseCgroupLimit(s)
		if err != nil || !limited {
			continue
		}
		u, err := used(d)
		if err != nil {
			return nil, err
		}
		levels = append(levels, cgroupLevel{Limit: limit, Used: u, Dir: d})
	}
	return levels, nil
}

// readValue reads a single-value counter file in dir.
func readValue(dir, name string) (uint64, error) {
	s, err := readCgroupFile(dir, name)
	if err != nil {
		return 0, err
	}
	return parseCgroupValue(s)
}

// memoryUsed is a cgroup's non-reclaimable memory: memory.current less the
// inactive_file page cache in memory.stat.
func memoryUsed(dir string) (uint64, error) {
	current, err := readValue(dir, "memory.current")
	if err != nil {
		return 0, err
	}
	stat, err := readCgroupFile(dir, "memory.stat")
	if err != nil {
		return 0, err
	}
	inactive, err := parseMemoryStatInactiveFile(strings.NewReader(stat))
	if err != nil {
		return 0, err
	}
	return current - min(inactive, current), nil
}

// applyCgroupMemory replaces the host memory figures in out with those of
// the memory.max level among dirs with the least headroom. It leaves out
// unchanged without a limit or when a limited level's usage cannot be read.
// memoryPressureDir is the level whose memory.pressure to read: the same
// least-headroom level the memory figures use (memory.pressure is
// hierarchical, so it includes that level's children), or "" for the host.
func memoryPressureDir(dirs []string) string {
	levels, err := readLevels(dirs, "memory.max", memoryUsed)
	if lvl, ok := leastHeadroom(levels); err == nil && ok {
		return lvl.Dir
	}
	return ""
}

func applyCgroupMemory(out *domain.HostSample, dirs []string) {
	levels, err := readLevels(dirs, "memory.max", memoryUsed)
	if lvl, ok := leastHeadroom(levels); err == nil && ok {
		out.MemTotal, out.MemAvailable = cgroupMemory(lvl, out.MemTotal, out.MemAvailable)
	}
}

// applyCgroupSwap replaces the host swap figures in out with those of the
// memory.swap.max level among dirs with the least headroom, its usage read
// from memory.swap.current. It leaves out unchanged without a limit or when
// a limited level's usage cannot be read.
func applyCgroupSwap(out *domain.HostSample, dirs []string) {
	levels, err := readLevels(dirs, "memory.swap.max", func(d string) (uint64, error) {
		return readValue(d, "memory.swap.current")
	})
	if lvl, ok := leastHeadroom(levels); err == nil && ok {
		out.SwapTotal, out.SwapUsed = cgroupSwap(lvl, out.SwapTotal)
	}
}
