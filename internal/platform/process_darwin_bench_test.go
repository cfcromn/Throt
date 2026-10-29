//go:build darwin

package platform

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// cpuTime returns user+system CPU time of this process and its reaped
// children, so a sample's cost includes the ps processes it runs.
func cpuTime(b *testing.B) time.Duration {
	var total time.Duration
	for _, who := range []int{syscall.RUSAGE_SELF, syscall.RUSAGE_CHILDREN} {
		var ru syscall.Rusage
		if err := syscall.Getrusage(who, &ru); err != nil {
			b.Fatal(err)
		}
		total += time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	return total
}

// BenchmarkSampleProcessesReal runs the real sampler against this host's
// process table, as one daemon tick does; it does not start throtd.
// cpu-ms/op is the CPU time one sample costs, ps included; divided by the
// sample interval it is the steady-state CPU share. A warm-up sample runs
// first, as in a daemon that has been up for more than one tick. Samples
// run back to back, so fewer processes are new in each than at a 2 s
// interval, where every new one costs one more ps run. kernel is the
// shipped collector with a fresh host CPU reading (ps only for its
// periodic refresh of other users' processes), ps the collector before it.
func BenchmarkSampleProcessesReal(b *testing.B) {
	saved := hostCPUAt.Load()
	b.Cleanup(func() { hostCPUAt.Store(saved) })
	for _, c := range []struct {
		name   string
		psOnly bool
	}{{"kernel", false}, {"ps", true}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			s := &darwinProcessSampler{psOnly: c.psOnly}
			ctx := context.Background()
			if _, err := s.SampleProcesses(ctx); err != nil {
				b.Fatal(err)
			}
			var n int
			start := cpuTime(b)
			for b.Loop() {
				noteHostCPU(time.Now(), true)
				procs, err := s.SampleProcesses(ctx)
				if err != nil {
					b.Fatal(err)
				}
				n = len(procs)
			}
			b.ReportMetric(float64(cpuTime(b)-start)/float64(time.Millisecond)/float64(b.N), "cpu-ms/op")
			b.ReportMetric(float64(n), "procs")
		})
	}
}
