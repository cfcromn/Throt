//go:build darwin

package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cfcromn/Throt/internal/domain"
)

// From <sys/resource.h>; x/sys/unix does not define them.
const (
	// prioDarwinProcess makes setpriority's second argument a PID.
	prioDarwinProcess = 4
	// prioDarwinBG puts the whole process (every thread) in the background
	// state, as `taskpolicy -b -p PID` does: lowest CPU priority, efficiency
	// cores on Apple silicon, throttled disk and network I/O. 0 clears it.
	prioDarwinBG = 0x1000
	// struct extern_proc's p_stat values (sys/proc.h): running,
	// sleeping, stopped, zombie.
	sRun   = 2
	sSleep = 3
	sStop  = 4
	sZomb  = 5
)

// Background puts key's process in the macOS background state if key still
// names that process owned by uid. The returned token is always 0: macOS
// has no way to read the state back without private APIs, so Foreground
// clears it.
//
// Identity is read with one sysctl(kern.proc.pid) instead of /bin/ps, which
// a workload's many short-lived members would otherwise fork for every
// tick: it reports the same start second (ps lstart prints p_starttime)
// and effective uid. Like signals on macOS, verifying and acting are two
// steps; the residual risk is the one described at NewProcessController.
// A process started after this one is lowered inherits the state.
func (c darwinProcessController) Background(ctx context.Context, key domain.ProcessKey, uid uint32) (uint64, error) {
	return 0, setDarwinBG(ctx, key, uid, prioDarwinBG)
}

// Foreground clears the background state of key's process if key still
// names that process owned by uid. token is ignored.
func (c darwinProcessController) Foreground(ctx context.Context, key domain.ProcessKey, uid uint32, _ uint64) error {
	return setDarwinBG(ctx, key, uid, 0)
}

func setDarwinBG(ctx context.Context, key domain.ProcessKey, uid uint32, prio int) error {
	if err := checkSignalTarget(key.PID); err != nil {
		return err
	}
	if err := verifyIdentity(ctx, kinfoIdentity, key, uid); err != nil {
		return fmt.Errorf("set priority of pid %d: %w", key.PID, err)
	}
	err := unix.Setpriority(prioDarwinProcess, key.PID, prio)
	switch {
	case errors.Is(err, unix.ESRCH):
		return fmt.Errorf("set priority of pid %d: %w", key.PID, domain.ErrProcessGone)
	case err != nil:
		return fmt.Errorf("set priority of pid %d: %w", key.PID, err)
	}
	return nil
}

// kinfoIdentity reads pid's start time (whole seconds, as ps lstart shows
// it) and effective uid with sysctl. A zombie counts as gone.
func kinfoIdentity(ctx context.Context, pid int) (time.Time, uint32, error) {
	if err := checkIdentityPID(pid); err != nil {
		return time.Time{}, 0, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	switch {
	case err != nil && pidExited(pid):
		// The sysctl returns no record, which x/sys reports as EIO.
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, domain.ErrProcessGone)
	case err != nil:
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	case int(k.Proc.P_pid) != pid:
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: sysctl reports pid %d", pid, k.Proc.P_pid)
	case k.Proc.P_stat == sZomb:
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: zombie: %w", pid, domain.ErrProcessGone)
	}
	return time.Unix(k.Proc.P_starttime.Sec, 0).In(time.Local), k.Eproc.Ucred.Uid, nil
}
