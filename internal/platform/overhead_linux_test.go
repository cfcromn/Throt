//go:build linux

package platform

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests in this file measure what throtd's sampling costs the rest of
// the machine without starting throtd. They are slow and only run when
// THROT_OVERHEAD is set to a run length, e.g. THROT_OVERHEAD=60s. They are
// meant for a container (docker run --init, so the sleepers are reaped):
// THROT_OVERHEAD_PROCS lists the process-table sizes to measure at
// (default 1000,3000), reached by starting idle sleep processes. Results go
// to the test log.

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

// overheadProcs returns the process-table sizes from THROT_OVERHEAD_PROCS.
func overheadProcs(t *testing.T) []int {
	t.Helper()
	v := os.Getenv("THROT_OVERHEAD_PROCS")
	if v == "" {
		v = "1000,3000"
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 0 {
			t.Fatalf("THROT_OVERHEAD_PROCS: bad size %q", f)
		}
		out = append(out, n)
	}
	return out
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

// countProcs returns the number of process directories in /proc.
func countProcs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if _, ok := parseProcPID(e.Name()); ok && e.IsDir() {
			n++
		}
	}
	return n
}

// growProcs starts sleep processes until /proc lists about n processes and
// stops them at the end of the test.
func growProcs(t *testing.T, n int) {
	t.Helper()
	var cmds []*exec.Cmd
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	})
	for range n - countProcs(t) {
		c := exec.Command("sleep", "3600")
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

func msPer(d time.Duration, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond) / float64(n)
}

// TestOverheadSampling runs a 10 ms sleep probe, as internal/bench does,
// alone and next to the real samplers ticking at 500 ms and 2 s, at each
// process-table size, and logs the probe's lateness and the sampling's CPU
// share. The probe-alone line logs the probe's own CPU, which the sampling
// lines subtract: in a container it can cost as much as sampling.
func TestOverheadSampling(t *testing.T) {
	run := overheadRun(t)
	for _, procs := range overheadProcs(t) {
		growProcs(t, procs)
		var probeCPU time.Duration // the probe's own CPU, measured alone
		for _, interval := range []time.Duration{0, 500 * time.Millisecond, 2 * time.Second} {
			late, cpu, ticks, wall := overheadOnce(t, run, interval)
			slices.Sort(late)
			name := "probe alone"
			if interval > 0 {
				name = "sampling every " + interval.String()
				cpu = max(cpu-probeCPU, 0)
			} else {
				probeCPU = cpu
			}
			t.Logf("procs=%5d %-24s probe n=%d p50=%v p95=%v p99=%v max=%v | cpu=%.2f%% of one CPU, %d ticks, %.2f ms cpu/tick, %.2f ms wall/tick",
				countProcs(t), name, len(late), percentile(late, 50), percentile(late, 95), percentile(late, 99), late[len(late)-1],
				100*float64(cpu)/float64(run), ticks, msPer(cpu, ticks), msPer(wall, ticks))
		}
	}
}

// overheadOnce runs the probe for run, with a daemon-like tick every
// interval (none when 0). It returns probe lateness samples, the CPU time of
// this process, the tick count and their total wall time.
func overheadOnce(t *testing.T, run, interval time.Duration) (late []time.Duration, cpu time.Duration, ticks int, wall time.Duration) {
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
	cpu0 := rusageCPU(t, syscall.RUSAGE_SELF)
	if interval > 0 {
		host, procs := NewHostSampler(), NewProcessSampler()
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
			if _, err := procs.SampleProcesses(tctx); err != nil {
				t.Error(err)
			}
			tcancel()
			wall += time.Since(start) - 10*time.Millisecond // less the probe's sleep
			ticks++
		}
	}
	late = <-probeDone
	cpu = rusageCPU(t, syscall.RUSAGE_SELF) - cpu0
	return late, cpu, ticks, wall
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

// BenchmarkSampleProcessesLinux runs the real samplers against this host's
// /proc, as one daemon tick does, after a warm-up sample; it does not start
// throtd. Run it with -benchmem: allocs/op and B/op are per sample.
// cpu-ms/op divided by the sample interval is the steady-state CPU share.
func BenchmarkSampleProcessesLinux(b *testing.B) {
	s := NewProcessSampler()
	ctx := context.Background()
	if _, err := s.SampleProcesses(ctx); err != nil {
		b.Fatal(err)
	}
	var n int
	cpu := func() time.Duration {
		var ru syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
			b.Fatal(err)
		}
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	start := cpu()
	for b.Loop() {
		procs, err := s.SampleProcesses(ctx)
		if err != nil {
			b.Fatal(err)
		}
		n = len(procs)
	}
	b.ReportMetric(float64(cpu()-start)/float64(time.Millisecond)/float64(b.N), "cpu-ms/op")
	b.ReportMetric(float64(n), "procs")
}

// BenchmarkSampleHostLinux runs the real host sampler, for comparison.
func BenchmarkSampleHostLinux(b *testing.B) {
	s := NewHostSampler()
	ctx := context.Background()
	for b.Loop() {
		_, _ = s.SampleHost(ctx)
	}
}
