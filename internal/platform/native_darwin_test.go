//go:build darwin

package platform

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestMachDuration(t *testing.T) {
	for _, c := range []struct {
		ticks, hz uint64
		want      time.Duration
	}{
		{24_000_000, 24_000_000, time.Second}, // Apple silicon
		{3, 24_000_000, 125},                  // 125/3 ns per tick
		{1_000_000_000, 1e9, time.Second},     // Intel
		{math.MaxUint64, 1, math.MaxInt64},    // saturates
		{math.MaxUint64, 24_000_000, math.MaxInt64},
	} {
		if got := machDuration(c.ticks, c.hz); got != c.want {
			t.Errorf("machDuration(%d, %d) = %d, want %d", c.ticks, c.hz, got, c.want)
		}
	}
}

func TestReadProcTask(t *testing.T) {
	hz, err := machTimebase()
	if err != nil {
		t.Fatal(err)
	}
	var buf [sizeofProcTaskAllInfo]byte
	if err := checkSelfCPU(hz, &buf); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	k, err := unix.SysctlKinfoProc("kern.proc.pid", self)
	if err != nil {
		t.Fatal(err)
	}
	p, err := readProcTask(self, hz, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if p.pid != self || p.uid != uint32(os.Geteuid()) || p.startSec != k.Proc.P_starttime.Sec || p.rss == 0 || p.cpu <= 0 {
		t.Errorf("self = %+v, kinfo start %d", p, k.Proc.P_starttime.Sec)
	}
	if _, err := readProcTask(1<<30, hz, &buf); !errors.Is(err, unix.ESRCH) {
		t.Errorf("nonexistent pid: %v, want ESRCH", err)
	}
	if os.Geteuid() != 0 {
		// launchd is root's: proc_info refuses it, which is why other
		// users' processes need ps.
		if _, err := readProcTask(1, hz, &buf); !errors.Is(err, unix.EPERM) {
			t.Errorf("launchd: %v, want EPERM", err)
		}
	}
}

// TestKernelCollectorMatchesPS compares kern.proc.all and proc_info with
// two ps stats passes, one before and one after, on the live process
// table. A process alive in both passes, with the same start, must be
// listed with exactly that start: the identity a running daemon would
// otherwise see change. Parent, uid and stopped or zombie state must match
// when both passes agree; CPU time must lie between the two passes; RSS,
// which moves, must be close for nearly every process.
func TestKernelCollectorMatchesPS(t *testing.T) {
	ctx := context.Background()
	euid := uint32(os.Geteuid())
	own := func(uid uint32) bool { return euid == 0 || uid == euid }
	// A stopped child, so the stopped state is always compared.
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if err := child.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		k, err := unix.SysctlKinfoProc("kern.proc.pid", child.Process.Pid)
		if err == nil && k.Proc.P_stat == sStop {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child not stopped: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, err := psStatsPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	procs, secs, err := kinfoProcs(time.Local)
	if err != nil {
		t.Fatal(err)
	}
	drop := make([]bool, len(procs))
	if err := readOwnStats(procs, secs, own, drop); err != nil {
		t.Fatal(err)
	}
	after, err := psStatsPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	index := func(ps []domain.ProcessSample) map[int]domain.ProcessSample {
		m := make(map[int]domain.ProcessSample, len(ps))
		for _, p := range ps {
			m[p.PID] = p
		}
		return m
	}
	kernel := make(map[int]domain.ProcessSample, len(procs))
	for i, p := range procs {
		if !drop[i] {
			kernel[p.PID] = p
		}
	}
	b, a := index(before), index(after)
	// ps prints hundredths, and on Apple silicon its total runs 10–20 ms
	// ahead of proc_info for some processes (seen in CI). Rates are deltas,
	// so the offset only shows when a sample falls back to ps: a one-off
	// 20 ms over a 2 s interval, under 1% of a core.
	const cpuSlack = 50 * time.Millisecond
	var compared, measured, rssOff int
	for pid, pb := range b {
		pa, ok := a[pid]
		if !ok || pa.Start != pb.Start {
			continue // exited or reused between the passes
		}
		n, ok := kernel[pid]
		if !ok {
			t.Errorf("pid %d (started %v) is in both ps passes, not in kern.proc.all", pid, pb.Start)
			continue
		}
		compared++
		if n.Start != pb.Start {
			t.Errorf("pid %d: start %v, ps %v", pid, n.Start, pb.Start)
		}
		if pa.UID == pb.UID && n.UID != pb.UID {
			t.Errorf("pid %d: uid %d, ps %d", pid, n.UID, pb.UID)
		}
		if pa.PPID == pb.PPID && n.PPID != pb.PPID {
			t.Errorf("pid %d: ppid %d, ps %d", pid, n.PPID, pb.PPID)
		}
		for _, st := range []domain.ProcessState{domain.StateStopped, domain.StateZombie} {
			if (pa.State == st) == (pb.State == st) && (n.State == st) != (pb.State == st) {
				t.Errorf("pid %d: state %s, ps %s", pid, n.State, pb.State)
			}
		}
		if !own(n.UID) || n.State == domain.StateZombie {
			continue
		}
		measured++
		if n.CPUTime+cpuSlack < pb.CPUTime || n.CPUTime > pa.CPUTime+cpuSlack {
			t.Errorf("pid %d: cpu %v, ps %v then %v", pid, n.CPUTime, pb.CPUTime, pa.CPUTime)
		}
		lo, hi := min(pa.RSS, pb.RSS), max(pa.RSS, pb.RSS)
		if n.RSS+n.RSS/4+16<<20 < lo || n.RSS > hi+hi/4+16<<20 {
			rssOff++
			t.Logf("pid %d: rss %d, ps %d then %d", pid, n.RSS, pb.RSS, pa.RSS)
		}
	}
	if c := kernel[child.Process.Pid]; c.State != domain.StateStopped || b[child.Process.Pid].State != domain.StateStopped {
		t.Errorf("stopped child: state %q, ps %q", c.State, b[child.Process.Pid].State)
	}
	t.Logf("%d processes in both ps passes, %d measured with proc_info, %d with RSS off", compared, measured, rssOff)
	if compared < 20 || measured == 0 {
		t.Fatalf("compared %d processes, measured %d", compared, measured)
	}
	if rssOff > max(3, measured/20) {
		t.Errorf("RSS off for %d of %d processes", rssOff, measured)
	}
}

// TestSamplerPaths checks which collector each sample uses: ps for other
// users' processes on the first sample and while host CPU has no fresh
// reading, the kernel alone otherwise, and ps alone when asked.
func TestSamplerPaths(t *testing.T) {
	saved := hostCPUAt.Load()
	t.Cleanup(func() { hostCPUAt.Store(saved) })
	ctx := context.Background()
	self, want := os.Getpid(), filepath.Base(os.Args[0])
	check := func(s *darwinProcessSampler, path string) {
		t.Helper()
		procs, err := s.SampleProcesses(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s.path != path {
			t.Errorf("path = %q, want %q", s.path, path)
		}
		for _, p := range procs {
			if p.PID == self {
				if p.Name != want || p.CPUTime <= 0 || p.RSS == 0 || p.UID != uint32(os.Geteuid()) {
					t.Errorf("self = %+v", p)
				}
				return
			}
		}
		t.Errorf("self not listed")
	}
	withPS := pathKernelPS
	if os.Geteuid() == 0 {
		withPS = pathKernel // root reads every process with proc_info
	}
	s := &darwinProcessSampler{}
	noteHostCPU(time.Now(), true)
	check(s, withPS) // the first sample measures everyone
	noteHostCPU(time.Now(), true)
	check(s, pathKernel)
	noteHostCPU(time.Now(), false)
	check(s, withPS) // the daemon sums per-process CPU
	noteHostCPU(time.Now().Add(-hostCPUFreshFor), true)
	check(s, withPS)
	noteHostCPU(time.Now(), true)
	for range othersRefresh - 1 {
		check(s, pathKernel)
	}
	check(s, withPS) // the periodic refresh
	check(&darwinProcessSampler{psOnly: true}, pathPS)
}
