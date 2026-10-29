package platform

// Carried stats for processes the in-process macOS collector cannot
// measure. It is a pure function of the samples it is given and carries no
// build constraint so it is tested on every OS.

import (
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// othersRefresh is how many samples other users' CPU time and RSS may be
// carried before a ps pass measures them again.
const othersRefresh = 15

// otherStats reports CPU time and RSS for processes whose stats only a ps
// pass can read (on macOS, other users' processes: proc_info refuses them
// and /bin/ps is setuid root). Between ps passes a process keeps its last
// RSS and CPU time, so its CPU rate reads 0 rather than a guess, and the
// CPU time it reports grows only by what two consecutive ps passes
// measured: CPU used while it was not measured is never credited to one
// interval, which would read as a burst. A process first seen in a ps pass
// reports the cumulative CPU time ps prints, as before. It is not safe for
// concurrent use.
type otherStats struct {
	last    map[domain.ProcessKey]otherStat
	started bool
	// sincePS counts samples since the last ps pass.
	sincePS int
}

type otherStat struct {
	// cpu and rss are what was reported; raw is the CPU time ps printed.
	cpu, raw time.Duration
	rss      uint64
	// measured is true when the previous sample's ps pass read it.
	measured bool
}

// due reports whether the next sample should measure the processes with
// ps: on the first sample and once every othersRefresh samples.
func (o *otherStats) due() bool {
	return !o.started || o.sincePS+1 >= othersRefresh
}

// observe sets CPUTime and RSS of every process in procs that other
// reports true for. measured says that this sample's values came from ps;
// otherwise they are ignored and the carried ones reported. Processes not
// in procs are forgotten.
func (o *otherStats) observe(procs []domain.ProcessSample, other func(domain.ProcessSample) bool, measured bool) {
	next := make(map[domain.ProcessKey]otherStat)
	for i := range procs {
		p := &procs[i]
		if !other(*p) {
			continue
		}
		prev, seen := o.last[p.Key()]
		var st otherStat
		switch {
		case measured && !seen:
			st = otherStat{cpu: p.CPUTime, raw: p.CPUTime, rss: p.RSS, measured: true}
		case measured:
			st = otherStat{cpu: prev.cpu, raw: p.CPUTime, rss: p.RSS, measured: true}
			if prev.measured && p.CPUTime >= prev.raw {
				st.cpu += p.CPUTime - prev.raw
			}
		default:
			st = otherStat{cpu: prev.cpu, raw: prev.raw, rss: prev.rss}
		}
		p.CPUTime, p.RSS = st.cpu, st.rss
		next[p.Key()] = st
	}
	o.last, o.started = next, true
	if measured {
		o.sincePS = 0
	} else {
		o.sincePS++
	}
}
