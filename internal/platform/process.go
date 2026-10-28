package platform

import (
	"context"
	"fmt"
	"os"

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
// decides whether a process may be controlled, but it does refuse to signal
// a process that is not the one the caller means: Stop and Continue verify
// identity and signal as one operation.
type ProcessController interface {
	// Stop sends SIGSTOP to key's process alone, never to a process group,
	// if key's PID still names a process with key's start time owned by
	// uid. Otherwise it sends nothing and returns an error wrapping
	// domain.ErrProcessGone. Start and uid are compared with exactly the
	// values the platform's ProcessSampler reports.
	Stop(ctx context.Context, key domain.ProcessKey, uid uint32) error
	// Continue is Stop with SIGCONT.
	Continue(ctx context.Context, key domain.ProcessKey, uid uint32) error
	// Gone reports whether key's process no longer exists: its PID names no
	// process, or one with another start time. An error means the OS could
	// not be asked.
	Gone(ctx context.Context, key domain.ProcessKey) (bool, error)
}

// ProcessIdentifier looks up one live process. Every ProcessController of
// this package implements it.
type ProcessIdentifier interface {
	// Identify returns pid's parent, uid and start time, with exactly the
	// values the platform's ProcessSampler reports (from NewProcessTools on
	// Linux, which shares the boot time). It fails with domain.ErrProcessGone
	// when pid names no process. Name may be empty.
	Identify(ctx context.Context, pid int) (domain.ProcessSample, error)
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
