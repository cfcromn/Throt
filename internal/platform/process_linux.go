//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"golang.org/x/sys/unix"
)

// NewProcessSampler returns a process sampler for the current OS. On Linux
// it reads /proc/<pid>/stat and /proc/<pid>/status for every numeric entry
// in /proc.
func NewProcessSampler() ProcessSampler { return &linuxProcessSampler{boot: &bootClock{}} }

// NewProcessController returns a process controller for the current OS. On
// Linux it pins the process with a pidfd, verifies identity from
// /proc/<pid>/stat and /proc/<pid>/status, and signals through the pidfd.
// It caches boot time independently of any sampler; use NewProcessTools
// when identities must match a sampler's Start values.
func NewProcessController() ProcessController {
	return linuxProcessController{boot: &bootClock{}}
}

// NewProcessTools returns a sampler and a controller that share one boot-time
// cache, so the controller compares exactly the Start the sampler reports.
func NewProcessTools() (ProcessSampler, ProcessController) {
	boot := &bootClock{}
	return &linuxProcessSampler{boot: boot}, linuxProcessController{boot: boot}
}

// bootClock caches the boot time. It is read once: the kernel derives btime
// from the wall clock, so re-reading it after a clock step would shift every
// process's Start and change every identity at once. It is safe for
// concurrent use because a sampler and a controller may share it.
type bootClock struct {
	mu   sync.Mutex
	boot time.Time
}

// get returns the cached boot time, reading /proc/stat until it succeeds once.
func (b *bootClock) get() (time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.boot.IsZero() {
		var boot time.Time
		if err := withFile("/proc/stat", func(r io.Reader) (err error) {
			boot, err = parseBootTime(r)
			return err
		}); err != nil {
			return time.Time{}, err
		}
		b.boot = boot
	}
	return b.boot, nil
}

// linuxProcessSampler is not safe for concurrent use.
type linuxProcessSampler struct {
	boot *bootClock
}

func (s *linuxProcessSampler) SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	boot, err := s.boot.get()
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	pageSize := uint64(os.Getpagesize())
	out := make([]domain.ProcessSample, 0, len(entries))
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil || !e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("sample processes: %w", err)
		}
		p, err := readLinuxProcess("/proc/"+e.Name(), boot, pageSize)
		if err != nil {
			// Usually ENOENT/ESRCH because the process exited mid-scan; any
			// other per-process failure also just means it is not visible.
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("sample processes: /proc listed no readable processes")
	}
	return out, nil
}

type linuxProcessController struct {
	boot *bootClock
	// pidfdOpen opens a pidfd; nil means unix.PidfdOpen. Tests replace it
	// to exercise the fallback for kernels without pidfds.
	pidfdOpen func(pid, flags int) (int, error)
}

func (c linuxProcessController) Stop(ctx context.Context, key domain.ProcessKey, uid uint32) error {
	return c.signal(ctx, key, uid, unix.SIGSTOP)
}

func (c linuxProcessController) Continue(ctx context.Context, key domain.ProcessKey, uid uint32) error {
	return c.signal(ctx, key, uid, unix.SIGCONT)
}

func (c linuxProcessController) Gone(ctx context.Context, key domain.ProcessKey) (bool, error) {
	return goneBy(ctx, c.identity, key)
}

// signal verifies key and sends sig to it without a reuse window. The pidfd
// is opened first and pins the process: it can never refer to a later
// process that reuses the PID. If /proc, read after the pidfd is open, still
// shows key's start time and uid, the pidfd refers to key's process, and
// pidfd_send_signal reaches that process or fails with ESRCH.
//
// Without pidfd support (ENOSYS before Linux 5.3, or EPERM when a seccomp
// filter blocks the syscall, as older container runtimes do) it falls back
// to verifying and then calling kill(2), which leaves the same short reuse
// window as macOS.
func (c linuxProcessController) signal(ctx context.Context, key domain.ProcessKey, uid uint32, sig unix.Signal) error {
	if err := checkSignalTarget(key.PID); err != nil {
		return err
	}
	open := c.pidfdOpen
	if open == nil {
		open = unix.PidfdOpen
	}
	fd, err := open(key.PID, 0)
	switch {
	case errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM):
		return verifyThenKill(ctx, c.identity, key, uid, sig)
	case errors.Is(err, unix.ESRCH):
		return fmt.Errorf("signal %v to pid %d: %w", sig, key.PID, domain.ErrProcessGone)
	case err != nil:
		return fmt.Errorf("signal %v to pid %d: pidfd_open: %w", sig, key.PID, err)
	}
	defer unix.Close(fd)
	if err := verifyIdentity(ctx, c.identity, key, uid); err != nil {
		return fmt.Errorf("signal %v to pid %d: %w", sig, key.PID, err)
	}
	err = unix.PidfdSendSignal(fd, sig, nil, 0)
	switch {
	case errors.Is(err, unix.ESRCH):
		return fmt.Errorf("signal %v to pid %d: %w", sig, key.PID, domain.ErrProcessGone)
	case err != nil:
		return fmt.Errorf("signal %v to pid %d: %w", sig, key.PID, err)
	}
	return nil
}

// identity reads pid from /proc. Start and uid match what the sampler
// reports.
func (c linuxProcessController) identity(ctx context.Context, pid int) (time.Time, uint32, error) {
	if err := checkIdentityPID(pid); err != nil {
		return time.Time{}, 0, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	boot, err := c.boot.get()
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	// The same reader as the sampler, so Start and UID match it exactly.
	p, err := readLinuxProcess("/proc/"+strconv.Itoa(pid), boot, uint64(os.Getpagesize()))
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH):
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, domain.ErrProcessGone)
	case err != nil:
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	case p.PID != pid:
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: stat reports pid %d", pid, p.PID)
	}
	return p.Start, p.UID, nil
}

// readLinuxProcess reads one process directory such as /proc/123.
func readLinuxProcess(dir string, boot time.Time, pageSize uint64) (domain.ProcessSample, error) {
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return domain.ProcessSample{}, err
	}
	st, err := parsePIDStat(b)
	if err != nil {
		return domain.ProcessSample{}, err
	}
	var uid uint32
	if err := withFile(dir+"/status", func(r io.Reader) (err error) {
		uid, err = parseStatusUID(r)
		return err
	}); err != nil {
		return domain.ProcessSample{}, err
	}
	return domain.ProcessSample{
		PID:     st.PID,
		PPID:    st.PPID,
		UID:     uid,
		Start:   boot.Add(ticksToDuration(st.StartTime)),
		Name:    st.Comm,
		CPUTime: ticksToDuration(st.CPUTicks),
		RSS:     st.RSSPages * pageSize,
	}, nil
}
