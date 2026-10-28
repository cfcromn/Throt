package platform

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// ProcessSampler enumerates the process table.
type ProcessSampler interface {
	// SampleProcesses returns every process visible to the current user.
	// Processes that cannot be read (for example because they exit during
	// the scan) are skipped; an error is returned only when the process list
	// as a whole cannot be read.
	SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error)
}

// ProcessController sends job-control signals to single processes. It never
// decides whether a process may be controlled: callers must first confirm,
// via Identity, that the PID still names the process they mean to signal.
type ProcessController interface {
	// Identity re-reads pid from the OS now. Start and UID are exactly the
	// values the platform's ProcessSampler reports for the same process, so
	// they can be compared with a domain.ProcessKey. An error wrapping
	// domain.ErrProcessGone means pid does not exist.
	Identity(ctx context.Context, pid int) (start time.Time, uid uint32, err error)
	// Stop sends SIGSTOP to pid alone, never to a process group.
	Stop(pid int) error
	// Continue sends SIGCONT to pid alone, never to a process group.
	Continue(pid int) error
}

// checkSignalTarget refuses PIDs that must never be signalled: 0 and
// negative values address process groups, 1 is init/launchd, and stopping
// the current process would freeze the daemon.
func checkSignalTarget(pid int) error {
	switch {
	case pid <= 1:
		return fmt.Errorf("refusing to signal pid %d", pid)
	case pid == os.Getpid():
		return fmt.Errorf("refusing to signal pid %d: it is this process", pid)
	}
	return nil
}

// checkIdentityPID rejects PIDs that cannot name a single process.
func checkIdentityPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("identity of pid %d: invalid pid", pid)
	}
	return nil
}
