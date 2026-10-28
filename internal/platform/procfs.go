package platform

// Parsers for Linux procfs files. They are pure functions over io.Reader and
// carry no build constraint so they are tested on every OS.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// cpuTimes holds aggregate CPU time counters from /proc/stat, in clock ticks.
type cpuTimes struct {
	Total uint64
	Idle  uint64
}

// busySince returns the busy fraction between prev and t, and false when the
// counters did not advance (or went backwards) so no fraction is defined.
func (t cpuTimes) busySince(prev cpuTimes) (float64, bool) {
	if t.Total <= prev.Total || t.Idle < prev.Idle {
		return 0, false
	}
	total := t.Total - prev.Total
	idle := t.Idle - prev.Idle
	if idle > total {
		return 0, false
	}
	return float64(total-idle) / float64(total), true
}

// parseProcStat reads the aggregate "cpu" line of /proc/stat and counts the
// per-CPU "cpuN" lines. Idle includes iowait. Guest time is already counted
// in user time, so only the first eight fields are summed.
func parseProcStat(r io.Reader) (cpuTimes, int, error) {
	var (
		t     cpuTimes
		found bool
		ncpu  int
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			ncpu++
			continue
		}
		if found {
			return cpuTimes{}, 0, errors.New("parse /proc/stat: duplicate cpu line")
		}
		if len(fields) < 5 {
			return cpuTimes{}, 0, fmt.Errorf("parse /proc/stat: short cpu line %q", sc.Text())
		}
		vals := fields[1:]
		if len(vals) > 8 {
			vals = vals[:8]
		}
		for i, f := range vals {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, 0, fmt.Errorf("parse /proc/stat field %d: %w", i+1, err)
			}
			t.Total += v
			if i == 3 || i == 4 { // idle, iowait
				t.Idle += v
			}
		}
		found = true
	}
	if err := sc.Err(); err != nil {
		return cpuTimes{}, 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	if !found {
		return cpuTimes{}, 0, errors.New("parse /proc/stat: no aggregate cpu line")
	}
	return t, ncpu, nil
}

// parseLoadavg returns the 1-minute load average from /proc/loadavg.
func parseLoadavg(r io.Reader) (float64, error) {
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return 0, fmt.Errorf("read /proc/loadavg: %w", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, errors.New("parse /proc/loadavg: empty")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/loadavg: %w", err)
	}
	if v < 0 {
		return 0, fmt.Errorf("parse /proc/loadavg: negative load %v", v)
	}
	return v, nil
}

// memInfo holds the /proc/meminfo values Throt uses, in bytes.
type memInfo struct {
	MemTotal     uint64
	MemAvailable uint64
	SwapTotal    uint64
	SwapFree     uint64
	HasMem       bool
	HasSwap      bool
}

// parseMeminfo reads /proc/meminfo. Memory is present when MemTotal and
// MemAvailable are; on kernels without MemAvailable (before 3.14) it falls
// back to MemFree + Buffers + Cached. Swap is present when SwapTotal and
// SwapFree are. It fails only when neither memory nor swap is present.
func parseMeminfo(r io.Reader) (memInfo, error) {
	vals := make(map[string]uint64, 7)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch key {
		case "MemTotal", "MemAvailable", "MemFree", "Buffers", "Cached", "SwapTotal", "SwapFree":
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return memInfo{}, fmt.Errorf("parse /proc/meminfo %s: missing value", key)
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return memInfo{}, fmt.Errorf("parse /proc/meminfo %s: %w", key, err)
		}
		if len(fields) > 1 && fields[1] == "kB" {
			v *= 1024
		}
		vals[key] = v
	}
	if err := sc.Err(); err != nil {
		return memInfo{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}

	var m memInfo
	if total, ok := vals["MemTotal"]; ok && total > 0 {
		avail, ok := vals["MemAvailable"]
		if !ok {
			var free uint64
			free, ok = vals["MemFree"]
			avail = free + vals["Buffers"] + vals["Cached"]
		}
		if ok {
			m.MemTotal, m.MemAvailable, m.HasMem = total, min(avail, total), true
		}
	}
	if st, ok := vals["SwapTotal"]; ok {
		if sf, ok := vals["SwapFree"]; ok {
			m.SwapTotal, m.SwapFree, m.HasSwap = st, min(sf, st), true
		}
	}
	if !m.HasMem && !m.HasSwap {
		return memInfo{}, errors.New("parse /proc/meminfo: no memory or swap fields")
	}
	return m, nil
}
