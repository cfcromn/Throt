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
// alone and next to the real samplers ticking at 500 ms and 2 s, and logs
// the probe's lateness and the sampling's CPU share.
func TestOverheadSampling(t *testing.T) {
	run := overheadRun(t)
	if n, _ := strconv.Atoi(os.Getenv("THROT_OVERHEAD_LOAD")); n > 0 {
		startChildren(t, n, "/usr/bin/yes")
	}
	for _, interval := range []time.Duration{0, 500 * time.Millisecond, 2 * time.Second} {
		late, cpu, ticks, perTick := overheadOnce(t, run, interval)
		slices.Sort(late)
		name := "probe alone"
		if interval > 0 {
			name = "sampling every " + interval.String()
		}
		t.Logf("%-24s probe n=%d p50=%v p95=%v p99=%v max=%v | sampler cpu=%.2f%% of one CPU, %d ticks, %.1f ms cpu/tick, %.1f ms wall/tick",
			name, len(late), percentile(late, 50), percentile(late, 95), percentile(late, 99), late[len(late)-1],
			100*float64(cpu)/float64(run), ticks, msPer(cpu, ticks), msPer(perTick, ticks))
	}
}

func msPer(d time.Duration, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond) / float64(n)
}

// overheadOnce runs the probe for run, with a daemon-like tick every
// interval (none when 0). It returns probe lateness samples, the CPU time of
// this process and its ps children, the tick count and their total wall
// time.
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
	// The probe's own CPU is subtracted by measuring it alone; a tick's
	// CPU is what is added on top.
	cpu0 := rusageCPU(t, syscall.RUSAGE_SELF) + rusageCPU(t, syscall.RUSAGE_CHILDREN)
	if interval > 0 {
		host, procs := NewHostSampler(), NewProcessSampler()
		defer func() { _ = CloseHostSampler(host) }()
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
			wall += time.Since(start)
			ticks++
		}
	}
	late = <-probeDone
	cpu = rusageCPU(t, syscall.RUSAGE_SELF) + rusageCPU(t, syscall.RUSAGE_CHILDREN) - cpu0
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

// TestOverheadPSScaling measures one ps stats pass, one full names pass, one
// ps identity lookup and one cold sample as the process table grows, by
// starting idle sleep processes. ps costs the same whatever columns or
// processes it is asked for: it reads every process either way.
func TestOverheadPSScaling(t *testing.T) {
	overheadRun(t)
	ctx := context.Background()
	added := 0
	for _, extra := range []int{0, 500, 1000, 2000} {
		startChildren(t, extra-added, "/bin/sleep", "600")
		added = extra
		const reps = 20
		for _, m := range []struct {
			name string
			fn   func() error
		}{
			{"ps stats pass", func() error { _, err := runPS(ctx, psStatColumns); return err }},
			{"ps names pass", func() error { _, err := runPS(ctx, psNameColumns); return err }},
			{"ps -p identity", func() error { _, err := execPS(ctx, "-o", psIdentityColumns, "-p", "1"); return err }},
			{"sampler (cold)", func() error { _, err := NewProcessSampler().SampleProcesses(ctx); return err }},
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
