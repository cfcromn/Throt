//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// NewHostSampler returns a sampler for the current OS. On Linux it reads
// /proc/stat, /proc/loadavg and /proc/meminfo. The first sample reports CPU
// as missing because the busy fraction needs two counter readings.
//
// When the daemon runs in a cgroup v2 hierarchy whose own cgroup or an
// ancestor sets a limit (cpu.max, memory.max, memory.swap.max), each metric
// is measured against the tightest limit instead of the whole machine. The
// cgroup is located once, on the first sample; the limit files are re-read
// on every sample because limits can change at runtime. Any cgroup read
// error falls back to the host figure for that metric.
func NewHostSampler() HostSampler { return &linuxSampler{} }

// linuxSampler keeps the previous CPU counters to compute deltas, so it is
// not safe for concurrent use.
type linuxSampler struct {
	prev    cpuTimes
	hasPrev bool

	located bool
	cgDirs  []string // own cgroup v2 directory, then its ancestors; nil without cgroup v2
	cgCPU   cgroupCPUTracker
}

func (s *linuxSampler) SampleHost(ctx context.Context) (domain.HostSample, error) {
	if err := ctx.Err(); err != nil {
		return domain.HostSample{}, fmt.Errorf("sample host: %w", err)
	}
	if !s.located {
		s.cgDirs, s.located = locateCgroup(), true
	}
	out := domain.HostSample{Time: time.Now()}
	var fails sampleErrors

	ncpu := 0
	var cur cpuTimes
	hostCPU, hostCPUOK := 0.0, false
	cpuErr := withFile("/proc/stat", func(r io.Reader) (err error) {
		cur, ncpu, err = parseProcStat(r)
		return err
	})
	if cpuErr != nil {
		s.hasPrev = false
	} else {
		usage, ok := cur.busySince(s.prev)
		hostCPU, hostCPUOK = usage, s.hasPrev && ok
		s.prev, s.hasPrev = cur, true
	}
	if ncpu <= 0 {
		ncpu = runtime.NumCPU()
	}

	// With a binding CPU quota, busy time is measured against the quota's
	// effective CPUs and load is reported missing (see loadPerCPU).
	cgCPU, cgCPUOK := 0.0, false
	dir, quota, quotaBinds := cgroupCPULimit(s.cgDirs, ncpu)
	if quotaBinds {
		cgCPU, cgCPUOK, _ = s.cgCPU.sample(dir, quota, out.Time)
	}
	switch {
	case cgCPUOK:
		out.CPUUsage = cgCPU
	case hostCPUOK:
		out.CPUUsage = hostCPU
	default:
		fails.fail(domain.MetricCPU, cpuErr)
	}

	var load float64
	if err := withFile("/proc/loadavg", func(r io.Reader) (err error) {
		load, err = parseLoadavg(r)
		return err
	}); err != nil {
		fails.fail(domain.MetricLoad, err)
	} else if perCPU, ok := loadPerCPU(load, ncpu, quotaBinds); ok {
		out.Load1 = perCPU
	} else {
		fails.fail(domain.MetricLoad, nil)
	}

	var mem memInfo
	if err := withFile("/proc/meminfo", func(r io.Reader) (err error) {
		mem, err = parseMeminfo(r)
		return err
	}); err != nil {
		fails.fail(domain.MetricMemory, err)
		fails.fail(domain.MetricSwap, err)
	} else {
		if mem.HasMem {
			out.MemTotal, out.MemAvailable = mem.MemTotal, mem.MemAvailable
			applyCgroupMemory(&out, s.cgDirs)
		} else {
			fails.fail(domain.MetricMemory, errors.New("/proc/meminfo lacks memory fields"))
		}
		if mem.HasSwap {
			out.SwapTotal, out.SwapUsed = mem.SwapTotal, mem.SwapTotal-mem.SwapFree
			applyCgroupSwap(&out, s.cgDirs)
		} else {
			fails.fail(domain.MetricSwap, errors.New("/proc/meminfo lacks swap fields"))
		}
	}

	return out, fails.finish(&out)
}

// locateCgroup returns the daemon's cgroup v2 directory and its ancestors
// up to the cgroup2 mount root, or nil when there is no cgroup v2 or the
// path cannot be resolved.
func locateCgroup() []string {
	var (
		cg   string
		isV2 bool
	)
	if err := withFile("/proc/self/cgroup", func(r io.Reader) (err error) {
		cg, isV2, err = parseProcSelfCgroup(r)
		return err
	}); err != nil || !isV2 {
		return nil
	}
	var (
		mountPoint, root string
		mounted          bool
	)
	if err := withFile("/proc/self/mountinfo", func(r io.Reader) (err error) {
		mountPoint, root, mounted, err = parseCgroup2Mount(r)
		return err
	}); err != nil || !mounted {
		return nil
	}
	dirs, ok := cgroupDirs(mountPoint, root, cg)
	if !ok {
		return nil
	}
	return dirs
}

// withFile opens path and passes it to parse.
func withFile(path string, parse func(io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return parse(f)
}
