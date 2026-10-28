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
func NewHostSampler() HostSampler { return &linuxSampler{} }

// linuxSampler keeps the previous CPU counters to compute deltas, so it is
// not safe for concurrent use.
type linuxSampler struct {
	prev    cpuTimes
	hasPrev bool
}

func (s *linuxSampler) SampleHost(ctx context.Context) (domain.HostSample, error) {
	if err := ctx.Err(); err != nil {
		return domain.HostSample{}, fmt.Errorf("sample host: %w", err)
	}
	out := domain.HostSample{Time: time.Now()}
	var fails sampleErrors

	ncpu := 0
	var cur cpuTimes
	err := withFile("/proc/stat", func(r io.Reader) (err error) {
		cur, ncpu, err = parseProcStat(r)
		return err
	})
	switch {
	case err != nil:
		s.hasPrev = false
		fails.fail(domain.MetricCPU, err)
	default:
		usage, ok := cur.busySince(s.prev)
		if s.hasPrev && ok {
			out.CPUUsage = usage
		} else {
			fails.fail(domain.MetricCPU, nil)
		}
		s.prev, s.hasPrev = cur, true
	}
	if ncpu <= 0 {
		ncpu = runtime.NumCPU()
	}

	var load float64
	if err := withFile("/proc/loadavg", func(r io.Reader) (err error) {
		load, err = parseLoadavg(r)
		return err
	}); err != nil {
		fails.fail(domain.MetricLoad, err)
	} else {
		out.Load1 = load / float64(ncpu)
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
		} else {
			fails.fail(domain.MetricMemory, errors.New("/proc/meminfo lacks memory fields"))
		}
		if mem.HasSwap {
			out.SwapTotal, out.SwapUsed = mem.SwapTotal, mem.SwapTotal-mem.SwapFree
		} else {
			fails.fail(domain.MetricSwap, errors.New("/proc/meminfo lacks swap fields"))
		}
	}

	return out, fails.finish(&out)
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
