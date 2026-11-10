//go:build darwin

package platform

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// The tests in this file measure what throtd's sampling costs the rest of
// the machine without starting throtd. They are slow and only run when
// THROT_OVERHEAD is set to a run length, e.g. THROT_OVERHEAD=60s; set
// THROT_OVERHEAD_LOAD to a number of CPU-bound child processes to measure
// under contention. Results go to the test log.

func overheadRun(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv("THROT_OVERHEAD")
	if v == "" {
		t.Skip("set THROT_OVERHEAD=<duration> to measure sampling overhead")
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("THROT_OVERHEAD: %v", err)
	}
	return d
}

// rusageCPU returns the user+system CPU time of who.
func rusageCPU(t *testing.T, who int) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(who, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// startChildren starts n copies of name with args and stops them at the end
// of the test.
func startChildren(t *testing.T, n int, name string, args ...string) {
	t.Helper()
	var cmds []*exec.Cmd
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	})
	for range n {
		c := exec.Command(name, args...)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
}

// percentile returns the p-th percentile (0..100) of sorted durations.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(int(float64(len(sorted))*p/100), len(sorted)-1)]
}

// TestOverheadSampling runs a 10 ms sleep probe, as internal/bench does,
// alone and next to the real samplers ticking at 500 ms and 2 s, once with
// each process collector (kern.proc.all and proc_info, as shipped, and ps
// alone, as before), and logs the probe's lateness, the sampling's CPU
// share, and per process sample its wall time percentiles, allocations
// and how it was read.
func TestOverheadSampling(t *testing.T) {
	run := overheadRun(t)
	if n, _ := strconv.Atoi(os.Getenv("THROT_OVERHEAD_LOAD")); n > 0 {
		startChildren(t, n, "/usr/bin/yes")
	}
	alone := overheadOnce(t, run, 0, false)
	slices.Sort(alone.late)
	t.Logf("%-30s probe n=%d p50=%v p95=%v p99=%v max=%v | probe cpu=%.2f%% of one CPU",
		"probe alone", len(alone.late), percentile(alone.late, 50), percentile(alone.late, 95), percentile(alone.late, 99),
		alone.late[len(alone.late)-1], 100*float64(alone.cpu)/float64(run))
	for _, interval := range []time.Duration{500 * time.Millisecond, 2 * time.Second} {
		for _, psOnly := range []bool{false, true} {
			r := overheadOnce(t, run, interval, psOnly)
			slices.Sort(r.late)
			slices.Sort(r.sample)
			collector := "kernel"
			if psOnly {
				collector = "ps"
			}
			t.Logf("%-30s probe n=%d p50=%v p95=%v p99=%v max=%v | sampler cpu=%.2f%% of one CPU, %d ticks, %.1f ms cpu/tick, %.1f ms wall/tick",
				"every "+interval.String()+", "+collector, len(r.late), percentile(r.late, 50), percentile(r.late, 95), percentile(r.late, 99),
				r.late[len(r.late)-1], 100*float64(r.cpu-alone.cpu)/float64(run), r.ticks, msPer(r.cpu-alone.cpu, r.ticks), msPer(r.wall, r.ticks))
			t.Logf("%-30s process sample wall p50=%v p95=%v p99=%v | %.0f allocs/sample | paths %v",
				"", percentile(r.sample, 50), percentile(r.sample, 95), percentile(r.sample, 99),
				float64(r.allocs)/float64(max(r.ticks, 1)), r.paths)
		}
	}
}

// overheadResult is one overheadOnce run.
type overheadResult struct {
	// late is the probe's lateness; cpu the CPU time of this process and
	// its ps children, probe included.
	late []time.Duration
	cpu  time.Duration
	// ticks and wall count the daemon-like ticks and their total time.
	ticks int
	wall  time.Duration
	// sample is each process sample's wall time and allocs their total
	// allocations; paths counts how they were read.
	sample []time.Duration
	allocs uint64
	paths  map[string]int
}

func msPer(d time.Duration, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond) / float64(n)
}

// overheadOnce runs the probe for run, with a daemon-like tick every
// interval (none when 0) whose process sampler uses ps alone when psOnly
// is set.
func overheadOnce(t *testing.T, run, interval time.Duration, psOnly bool) (r overheadResult) {
	ctx, cancel := context.WithTimeout(context.Background(), run)
	defer cancel()
	probeDone := make(chan []time.Duration)
	go func() {
		runtime.LockOSThread()
		var out []time.Duration
		for ctx.Err() == nil {
			start := time.Now()
			time.Sleep(10 * time.Millisecond)
			out = append(out, max(time.Since(start)-10*time.Millisecond, 0))
		}
		probeDone <- out
	}()
	// The probe's own CPU is subtracted by measuring it alone; a tick's
	// CPU is what is added on top.
	cpu0 := rusageCPU(t, syscall.RUSAGE_SELF) + rusageCPU(t, syscall.RUSAGE_CHILDREN)
	if interval > 0 {
		host, procs := NewHostSampler(), &darwinProcessSampler{psOnly: psOnly}
		defer func() { _ = CloseHostSampler(host) }()
		r.paths = map[string]int{}
		var m0, m1 runtime.MemStats
		tk := time.NewTicker(interval)
		defer tk.Stop()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-tk.C:
			}
			start := time.Now()
			tctx, tcancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = sampleTickProbe(tctx)
			_, _ = host.SampleHost(tctx)
			runtime.ReadMemStats(&m0)
			pstart := time.Now()
			if _, err := procs.SampleProcesses(tctx); err != nil {
				t.Error(err)
			}
			r.sample = append(r.sample, time.Since(pstart))
			runtime.ReadMemStats(&m1)
			r.allocs += m1.Mallocs - m0.Mallocs
			r.paths[procs.path]++
			tcancel()
			r.wall += time.Since(start)
			r.ticks++
		}
	}
	r.late = <-probeDone
	r.cpu = rusageCPU(t, syscall.RUSAGE_SELF) + rusageCPU(t, syscall.RUSAGE_CHILDREN) - cpu0
	return r
}

// sampleTickProbe is the daemon's own 10 ms latency probe.
func sampleTickProbe(ctx context.Context) (time.Duration, bool) {
	start := time.Now()
	tm := time.NewTimer(10 * time.Millisecond)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return 0, false
	case <-tm.C:
		return time.Since(start) - 10*time.Millisecond, true
	}
}

// TestOverheadPSScaling measures one ps stats pass, one full names pass, one
// ps identity lookup, one cold sample and one warm sample with each
// collector as the process table grows, by starting idle sleep processes.
// ps costs the same whatever columns or processes it is asked for: it
// reads every process either way.
func TestOverheadPSScaling(t *testing.T) {
	overheadRun(t)
	saved := hostCPUAt.Load()
	t.Cleanup(func() { hostCPUAt.Store(saved) })
	ctx := context.Background()
	added := 0
	for _, extra := range []int{0, 500, 1000} { // 2000 more hits the macOS runner's process limit
		startChildren(t, extra-added, "/bin/sleep", "600")
		added = extra
		const reps = 20
		// Warm samplers, as in a daemon that has been up for a while, with
		// a fresh host CPU reading: the kernel one runs ps only for its
		// periodic refresh of other users' processes.
		kernel, psOnly := &darwinProcessSampler{}, &darwinProcessSampler{psOnly: true}
		for _, s := range []*darwinProcessSampler{kernel, psOnly} {
			if _, err := s.SampleProcesses(ctx); err != nil {
				t.Fatal(err)
			}
		}
		warm := func(s *darwinProcessSampler) func() error {
			return func() error {
				noteHostCPU(time.Now(), true)
				_, err := s.SampleProcesses(ctx)
				return err
			}
		}
		for _, m := range []struct {
			name string
			fn   func() error
		}{
			{"ps stats pass", func() error { _, err := runPS(ctx, psStatColumns); return err }},
			{"ps names pass", func() error { _, err := runPS(ctx, psNameColumns); return err }},
			{"ps -p identity", func() error { _, err := execPS(ctx, "-o", psIdentityColumns, "-p", "1"); return err }},
			{"sampler (cold)", func() error { _, err := NewProcessSampler().SampleProcesses(ctx); return err }},
			{"warm, kernel", warm(kernel)},
			{"warm, ps", warm(psOnly)},
		} {
			self0, kids0 := rusageCPU(t, syscall.RUSAGE_SELF), rusageCPU(t, syscall.RUSAGE_CHILDREN)
			start := time.Now()
			for range reps {
				if err := m.fn(); err != nil {
					t.Fatal(err)
				}
			}
			wall := time.Since(start)
			self, kids := rusageCPU(t, syscall.RUSAGE_SELF)-self0, rusageCPU(t, syscall.RUSAGE_CHILDREN)-kids0
			procs, _ := NewProcessSampler().SampleProcesses(ctx)
			t.Logf("procs=%5d %-16s wall=%6.1f ms  self cpu=%5.1f ms  child cpu=%6.1f ms",
				len(procs), m.name, msPer(wall, reps), msPer(self, reps), msPer(kids, reps))
		}
	}
}
