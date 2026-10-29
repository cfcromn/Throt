//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/cfcromn/Throt/internal/domain"
)

const (
	// backgroundNice is the nice value Background sets.
	backgroundNice = 10
	// I/O priorities, from <linux/ioprio.h>.
	ioprioWhoProcess = 1
	ioprioClassShift = 13
	ioprioClassNone  = 0
	ioprioClassBE    = 2
	// ioprioBackground is the best-effort class at its lowest level.
	ioprioBackground = ioprioClassBE<<ioprioClassShift | 7
	// tokenValid marks a token Background returned.
	tokenValid = 1 << 31
)

// Background lowers key's process to nice +10 and best-effort I/O level 7,
// if key still names that process owned by uid, and returns a token for
// Foreground holding the original values. Linux keeps both per thread, so
// every thread listed in /proc/<pid>/task is lowered; threads started later
// inherit from the thread that creates them, and so do child processes.
// A thread already at nice 10 or more, or in the idle or real-time I/O
// class, keeps its value.
//
// setpriority has no pidfd form, so identity is verified first and then
// the PIDs are acted on: the same short reuse window as kill(2) on macOS
// (see NewProcessController there). An error means the main thread was
// not lowered; failures on other threads (one exiting meanwhile) are
// skipped.
func (c linuxProcessController) Background(ctx context.Context, key domain.ProcessKey, uid uint32) (uint64, error) {
	if err := c.verifyPriorityTarget(ctx, key, uid); err != nil {
		return 0, err
	}
	nice, err := getNice(key.PID)
	if err != nil {
		return 0, priorityErr(key.PID, err)
	}
	io, err := ioprioGet(key.PID)
	if err != nil {
		io = ioprioClassNone << ioprioClassShift
	}
	if nice < backgroundNice {
		if err := unix.Setpriority(unix.PRIO_PROCESS, key.PID, backgroundNice); err != nil {
			return 0, priorityErr(key.PID, err)
		}
	}
	token := uint64(tokenValid) | uint64(io)<<8 | uint64(nice+20)
	for _, tid := range c.threads(key.PID) {
		if tid != key.PID {
			if n, err := getNice(tid); err == nil && n < backgroundNice {
				_ = unix.Setpriority(unix.PRIO_PROCESS, tid, backgroundNice) // best effort
			}
		}
		if cur, err := ioprioGet(tid); err == nil && lowerableIO(cur) {
			_ = ioprioSet(tid, ioprioBackground) // best effort
		}
	}
	return token, nil
}

// Foreground puts back the values token (from Background) holds, on the
// threads of key's process that still have the values Background set, if
// key still names that process owned by uid. An unprivileged user may not
// lower a nice value again unless RLIMIT_NICE allows it, so the kernel
// usually refuses the nice part (EACCES); that is not an error, and the
// process keeps nice +10 until it exits. The I/O priority is restored.
func (c linuxProcessController) Foreground(ctx context.Context, key domain.ProcessKey, uid uint32, token uint64) error {
	if token&tokenValid == 0 {
		return fmt.Errorf("set priority of pid %d: invalid token", key.PID)
	}
	if err := c.verifyPriorityTarget(ctx, key, uid); err != nil {
		return err
	}
	nice, io := int(token&0xff)-20, int(token>>8&0xffff)
	for _, tid := range c.threads(key.PID) {
		if n, err := getNice(tid); err == nil && n == backgroundNice && nice < backgroundNice {
			err := unix.Setpriority(unix.PRIO_PROCESS, tid, nice)
			if tid == key.PID && err != nil && !errors.Is(err, unix.EACCES) {
				return priorityErr(key.PID, err)
			}
		}
		if cur, err := ioprioGet(tid); err == nil && cur == ioprioBackground && io != ioprioBackground {
			_ = ioprioSet(tid, io) // best effort
		}
	}
	return nil
}

func (c linuxProcessController) verifyPriorityTarget(ctx context.Context, key domain.ProcessKey, uid uint32) error {
	if err := checkSignalTarget(key.PID); err != nil {
		return err
	}
	if err := verifyIdentity(ctx, c.identity, key, uid); err != nil {
		return fmt.Errorf("set priority of pid %d: %w", key.PID, err)
	}
	return nil
}

// threads lists pid's thread IDs, always including pid itself first.
func (c linuxProcessController) threads(pid int) []int {
	tids := []int{pid}
	f, err := os.Open(procRoot(c.root) + "/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		return tids
	}
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	for _, n := range names {
		if tid, ok := parseProcPID(n); ok && tid != pid {
			tids = append(tids, tid)
		}
	}
	return tids
}

func priorityErr(pid int, err error) error {
	if errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("set priority of pid %d: %w", pid, domain.ErrProcessGone)
	}
	return fmt.Errorf("set priority of pid %d: %w", pid, err)
}

// getNice returns tid's nice value. The raw syscall returns 20 - nice.
func getNice(tid int) (int, error) {
	p, err := unix.Getpriority(unix.PRIO_PROCESS, tid)
	if err != nil {
		return 0, err
	}
	return 20 - p, nil
}

// lowerableIO reports whether ioprio is the default or a best-effort level
// above the lowest; the idle and real-time classes are left alone.
func lowerableIO(ioprio int) bool {
	switch ioprio >> ioprioClassShift {
	case ioprioClassNone:
		return true
	case ioprioClassBE:
		return ioprio != ioprioBackground
	}
	return false
}

func ioprioGet(tid int) (int, error) {
	r, _, e := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

func ioprioSet(tid, ioprio int) error {
	if _, _, e := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(ioprio)); e != 0 {
		return e
	}
	return nil
}
