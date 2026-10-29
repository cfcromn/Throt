//go:build darwin

package platform

// The in-process process table: sysctl(kern.proc.all) for the process
// set and identities, proc_info(PROC_PIDTASKALLINFO) for CPU time and
// RSS. No cgo: proc_info is a raw system call.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/cfcromn/Throt/internal/domain"
)

// From <sys/proc_info.h>.
const (
	procInfoCallPIDInfo = 2 // PROC_INFO_CALL_PIDINFO
	procPIDTaskAllInfo  = 2 // PROC_PIDTASKALLINFO
	// struct proc_taskallinfo is a struct proc_bsdinfo (136 bytes) followed
	// by a struct proc_taskinfo (96 bytes).
	sizeofProcBSDInfo     = 136
	sizeofProcTaskAllInfo = sizeofProcBSDInfo + 96
)

// procTask is the part of PROC_PIDTASKALLINFO the sampler uses.
type procTask struct {
	pid      int
	uid      uint32
	startSec int64
	cpu      time.Duration
	rss      uint64
	// running is true when at least one thread is running, which is when
	// ps prints R.
	running bool
}

// readProcTask reads pid with proc_info. hz is the Mach timebase
// frequency. Another user's process fails with EPERM (proc_info checks
// the effective uid, unlike setuid /bin/ps), a PID that names no process
// or a zombie with ESRCH.
func readProcTask(pid int, hz uint64, buf *[sizeofProcTaskAllInfo]byte) (procTask, error) {
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDTaskAllInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), sizeofProcTaskAllInfo)
	if errno != 0 {
		return procTask{}, errno
	}
	if n != sizeofProcTaskAllInfo {
		return procTask{}, fmt.Errorf("proc_info of pid %d: %d bytes, want %d", pid, n, sizeofProcTaskAllInfo)
	}
	ne := binary.NativeEndian
	// proc_bsdinfo: pbi_pid, pbi_uid (the effective uid), pbi_start_tvsec.
	// proc_taskinfo: pti_resident_size (bytes), pti_total_user,
	// pti_total_system, pti_numrunning.
	bsd, task := buf[:sizeofProcBSDInfo], buf[sizeofProcBSDInfo:]
	return procTask{
		pid:      int(ne.Uint32(bsd[12:])),
		uid:      ne.Uint32(bsd[20:]),
		startSec: int64(ne.Uint64(bsd[120:])),
		rss:      ne.Uint64(task[8:]),
		cpu:      machDuration(ne.Uint64(task[16:]), hz) + machDuration(ne.Uint64(task[24:]), hz),
		running:  int32(ne.Uint32(task[88:])) > 0,
	}, nil
}

// machTimebase returns the Mach timebase frequency: proc_taskinfo CPU
// times are in Mach absolute time units, 1 ns on Intel and 1/24 µs on
// Apple silicon. mach_timebase_info needs cgo; hw.tbfrequency is the same
// value as a sysctl.
var machTimebase = sync.OnceValues(func() (uint64, error) {
	hz, err := sysctlUint("hw.tbfrequency")
	if err == nil && hz == 0 {
		err = errors.New("sysctl hw.tbfrequency: zero")
	}
	return hz, err
})

// machDuration converts ticks of a hz timebase to a duration, saturating.
func machDuration(ticks, hz uint64) time.Duration {
	hi, lo := bits.Mul64(ticks, uint64(time.Second))
	if hi >= hz {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, hz)
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(q)
}

// checkSelfCPU checks proc_info against getrusage on this process: its
// CPU time must lie between two getrusage readings taken around it, give
// or take rounding. It catches a proc_info that cannot be called, a
// struct layout that moved and a wrong timebase.
func checkSelfCPU(hz uint64, buf *[sizeofProcTaskAllInfo]byte) error {
	before, err := selfCPU()
	if err != nil {
		return err
	}
	self := os.Getpid()
	t, err := readProcTask(self, hz, buf)
	if err != nil {
		return fmt.Errorf("proc_info of self: %w", err)
	}
	after, err := selfCPU()
	if err != nil {
		return err
	}
	slack := 5*time.Millisecond + after/20
	if t.pid != self || t.uid != uint32(os.Geteuid()) || t.rss == 0 || t.cpu+slack < before || t.cpu > after+slack {
		return fmt.Errorf("proc_info of self: pid %d, uid %d, rss %d, cpu %v; getrusage %v..%v", t.pid, t.uid, t.rss, t.cpu, before, after)
	}
	return nil
}

func selfCPU() (time.Duration, error) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0, fmt.Errorf("getrusage: %w", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), nil
}

// kinfoState maps struct extern_proc's p_stat. macOS keeps a live process
// at SRUN whether or not its threads run; the sampler refines running
// against sleeping where it can read the threads.
func kinfoState(stat int8) domain.ProcessState {
	switch stat {
	case sRun:
		return domain.StateRunning
	case sSleep:
		return domain.StateSleeping
	case sStop:
		return domain.StateStopped
	case sZomb:
		return domain.StateZombie
	}
	return domain.StateOther
}

// kinfoProcs lists every process from sysctl(kern.proc.all), the table ps
// itself reads, with PID, parent, effective uid (what ps uid prints),
// start (lstartTime, exactly as the ps path parses it) and state. PID 0,
// which ps does not list, is left out. secs holds each start's Unix
// second.
func kinfoProcs(loc *time.Location) (procs []domain.ProcessSample, secs []int64, err error) {
	kinfo, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, nil, fmt.Errorf("sysctl kern.proc.all: %w", err)
	}
	procs = make([]domain.ProcessSample, 0, len(kinfo))
	secs = make([]int64, 0, len(kinfo))
	for i := range kinfo {
		k := &kinfo[i]
		pid, ppid := int(k.Proc.P_pid), int(k.Eproc.Ppid)
		if pid <= 0 || ppid < 0 {
			continue
		}
		procs = append(procs, domain.ProcessSample{
			PID:   pid,
			PPID:  ppid,
			UID:   k.Eproc.Ucred.Uid,
			Start: lstartTime(k.Proc.P_starttime.Sec, loc),
			State: kinfoState(k.Proc.P_stat),
		})
		secs = append(secs, k.Proc.P_starttime.Sec)
	}
	if len(procs) == 0 {
		return nil, nil, errors.New("sysctl kern.proc.all: no processes")
	}
	return procs, secs, nil
}

// readOwnStats sets CPU time, RSS and running against sleeping on every
// process in procs that own reports true for, from proc_info. A process
// that exited since kern.proc.all, or whose PID now names another
// process, is marked in drop; one that is exiting keeps no stats, as a
// zombie. A live process is never dropped: a process missing from one
// sample reads as exited (a lease ends). Any other failure is returned,
// after which none of the values set may be used: the caller then reads
// all of them from ps.
func readOwnStats(procs []domain.ProcessSample, secs []int64, own func(uint32) bool, drop []bool) error {
	hz, err := machTimebase()
	if err != nil {
		return err
	}
	var buf [sizeofProcTaskAllInfo]byte
	if err := checkSelfCPU(hz, &buf); err != nil {
		return err
	}
	for i := range procs {
		p := &procs[i]
		if !own(p.UID) || p.State == domain.StateZombie {
			continue // a zombie has no task; ps shows it without stats too
		}
		t, err := readProcTask(p.PID, hz, &buf)
		switch {
		case errors.Is(err, unix.ESRCH):
			drop[i] = pidExited(p.PID)
			continue
		case err != nil:
			return fmt.Errorf("proc_info of pid %d: %w", p.PID, err)
		case t.pid != p.PID || t.startSec != secs[i]:
			drop[i] = true // the PID was reused since kern.proc.all
			continue
		}
		p.CPUTime, p.RSS = t.cpu, t.rss
		if p.State == domain.StateRunning && !t.running {
			p.State = domain.StateSleeping
		}
	}
	return nil
}

// hostCPUAt is when a host sampler last read host CPU from iostat, in Unix
// nanoseconds, or 0 when its last sample had none. Without a fresh
// reading the daemon estimates host CPU from per-process CPU deltas, which
// need every user's processes measured, so the process sampler then
// measures other users' processes with ps on every sample.
var hostCPUAt atomic.Int64

// hostCPUFreshFor bounds how old a host CPU reading may be, longer than
// the longest sample interval.
const hostCPUFreshFor = 15 * time.Second

func noteHostCPU(now time.Time, ok bool) {
	if ok {
		hostCPUAt.Store(now.UnixNano())
	} else {
		hostCPUAt.Store(0)
	}
}

func hostCPUFresh(now time.Time) bool {
	at := hostCPUAt.Load()
	return at != 0 && now.Sub(time.Unix(0, at)) < hostCPUFreshFor
}
