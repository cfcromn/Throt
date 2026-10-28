//go:build darwin || linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// identityFunc re-reads pid's start time and uid from the OS. The error
// wraps domain.ErrProcessGone when no such process exists.
type identityFunc func(ctx context.Context, pid int) (start time.Time, uid uint32, err error)

// verifyIdentity returns nil only if key's PID currently names a process
// with key's start time owned by uid. A mismatch wraps domain.ErrProcessGone.
func verifyIdentity(ctx context.Context, identity identityFunc, key domain.ProcessKey, uid uint32) error {
	start, got, err := identity(ctx, key.PID)
	switch {
	case err != nil:
		return err
	case !start.Equal(key.Start):
		return fmt.Errorf("pid %d: start time changed (pid reused): %w", key.PID, domain.ErrProcessGone)
	case got != uid:
		return fmt.Errorf("pid %d: owned by uid %d, not %d: %w", key.PID, got, uid, domain.ErrProcessGone)
	}
	return nil
}

// goneBy reports whether key's process no longer exists according to
// identity.
func goneBy(ctx context.Context, identity identityFunc, key domain.ProcessKey) (bool, error) {
	start, _, err := identity(ctx, key.PID)
	if errors.Is(err, domain.ErrProcessGone) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !start.Equal(key.Start), nil
}

// pidExited reports whether kill(pid, 0) confirms that no process has pid.
// Signal 0 checks existence and sends nothing. Any other result, EPERM
// included, means a process may have it.
func pidExited(pid int) bool {
	if pid <= 1 {
		return false // 0 and negative PIDs address groups
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// verifyThenKill verifies key's identity and then sends sig with kill(2).
// The PID could in principle be reused between the two steps; callers that
// can pin the process first (Linux pidfds) should do so.
func verifyThenKill(ctx context.Context, identity identityFunc, key domain.ProcessKey, uid uint32, sig syscall.Signal) error {
	if err := checkSignalTarget(key.PID); err != nil {
		return err
	}
	if err := verifyIdentity(ctx, identity, key, uid); err != nil {
		return fmt.Errorf("signal %v to pid %d: %w", sig, key.PID, err)
	}
	return signalPID(key.PID, sig)
}

// signalPID sends sig to the single process pid with kill(2). The caller
// has already applied checkSignalTarget.
func signalPID(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return fmt.Errorf("signal %v to pid %d: %w", sig, pid, domain.ErrProcessGone)
	case err != nil:
		return fmt.Errorf("signal %v to pid %d: %w", sig, pid, err)
	}
	return nil
}
