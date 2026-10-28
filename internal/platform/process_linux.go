//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"golang.org/x/sys/unix"
)

// NewProcessSampler returns a process sampler for the current OS. On Linux
// it reads /proc/<pid>/stat for every numeric entry in /proc, and
// /proc/<pid>/status (for the uid) only for processes it has not seen
// before, by PID and start time, and for a rolling 1/uidRefresh of the rest.
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
	boot  *bootClock
	root  string // procfs mount; empty means /proc. Tests use fixture trees.
	procs procReader
}

func (s *linuxProcessSampler) SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	boot, err := s.boot.get()
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	dir, err := os.Open(procRoot(s.root))
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	slices.Sort(names) // the order os.ReadDir returned before
	dirfd := int(dir.Fd())
	r := &s.procs
	if r.next == nil {
		r.seen, r.next = make(map[procID]procInfo, len(names)), make(map[procID]procInfo, len(names))
	}
	clear(r.next)
	pageSize := uint64(os.Getpagesize())
	out := make([]domain.ProcessSample, 0, len(names))
	for _, name := range names {
		if _, ok := parseProcPID(name); !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("sample processes: %w", err)
		}
		p, err := r.read(dirfd, name, boot, pageSize)
		if err != nil {
			// Usually ENOENT/ESRCH because the process exited mid-scan;
			// ENOTDIR for an entry that is not a process directory; any
			// other per-process failure also just means it is not visible.
			continue
		}
		out = append(out, p)
	}
	// Processes not read this sample drop out of the cache.
	r.seen, r.next = r.next, r.seen
	r.turn = (r.turn + 1) % uidRefresh
	if len(out) == 0 {
		return nil, errors.New("sample processes: /proc listed no readable processes")
	}
	return out, nil
}

type linuxProcessController struct {
	boot *bootClock
	root string // procfs mount; empty means /proc. Tests use fixture trees.
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
	p, err := c.Identify(ctx, pid)
	if err != nil {
		return time.Time{}, 0, err
	}
	return p.Start, p.UID, nil
}

// Identify reads pid from /proc/<pid>/stat and status with the sampler's
// parsers and boot time, so Start, UID and PPID match what it reports.
func (c linuxProcessController) Identify(ctx context.Context, pid int) (domain.ProcessSample, error) {
	if err := checkIdentityPID(pid); err != nil {
		return domain.ProcessSample{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	boot, err := c.boot.get()
	if err != nil {
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	// A reader without a cache reads both files.
	var r procReader
	p, err := r.read(unix.AT_FDCWD, procRoot(c.root)+"/"+strconv.Itoa(pid), boot, uint64(os.Getpagesize()))
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH):
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, domain.ErrProcessGone)
	case err != nil:
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, err)
	case p.PID != pid:
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: stat reports pid %d", pid, p.PID)
	}
	return p, nil
}

// procRoot returns root, or /proc when it is empty.
func procRoot(root string) string {
	if root == "" {
		return "/proc"
	}
	return root
}

// uidRefresh is how many samples a cached uid is trusted. setuid(2) changes
// a process's real uid but not its PID or start time, so only a fresh read
// of status notices it. Each sample re-reads the uids of the PIDs in one
// residue class modulo uidRefresh, spreading the cost evenly.
// ponytail: a rolling refresh instead of change detection; a process that
// changes its real uid in place (a root daemon dropping privileges after
// its first sample) keeps its old uid for up to this many samples. The
// controller checks every member's live uid before a pause sends its first
// signal, and again with each signal, so a stale one never reaches another
// user's process.
const uidRefresh = 15

// procID is a process identity: a reused PID has a new start time.
type procID struct {
	pid   int
	start uint64 // clock ticks after boot
}

// procInfo is what a sampler keeps of a process between samples.
type procInfo struct {
	uid  uint32
	name string
}

// procReader reads process directories with one reused buffer. With its
// maps set, it reads /proc/<pid>/status only for processes missing from
// seen (or due for a uidRefresh), records every process it reads in next,
// and reuses the cached name string while comm is unchanged. It is not
// safe for concurrent use.
type procReader struct {
	buf        []byte
	seen, next map[procID]procInfo // the previous and the current sample
	turn       int                 // residue modulo uidRefresh re-read this sample
}

// read reads the process directory dir, relative to dirfd, such as "123"
// relative to an open /proc or "/proc/123" relative to unix.AT_FDCWD.
func (r *procReader) read(dirfd int, dir string, boot time.Time, pageSize uint64) (domain.ProcessSample, error) {
	b, err := readFileAt(dirfd, dir+"/stat", &r.buf)
	if err != nil {
		return domain.ProcessSample{}, err
	}
	st, comm, err := parsePIDStatRaw(b)
	if err != nil {
		return domain.ProcessSample{}, err
	}
	id := procID{pid: st.PID, start: st.StartTime}
	info, known := r.seen[id]
	// comm aliases r.buf, so it is used before status is read into it.
	if !known || !commEqual(comm, info.name) {
		info.name = sanitizeComm(comm)
	}
	if !known || st.PID%uidRefresh == r.turn {
		b, err := readFileAt(dirfd, dir+"/status", &r.buf)
		if err != nil {
			return domain.ProcessSample{}, err
		}
		if info.uid, err = statusUID(b); err != nil {
			return domain.ProcessSample{}, err
		}
	}
	if r.next != nil {
		r.next[id] = info
	}
	return domain.ProcessSample{
		PID:     st.PID,
		PPID:    st.PPID,
		UID:     info.uid,
		Start:   boot.Add(ticksToDuration(st.StartTime)),
		Name:    info.name,
		CPUTime: ticksToDuration(st.CPUTicks),
		RSS:     st.RSSPages * pageSize,
		State:   st.State,
	}, nil
}

// readFileAt reads the file at path, relative to dirfd, into *buf, which
// it grows as needed and keeps for the next call, and returns the
// contents. It costs three system calls where os.ReadFile costs six, and
// allocates nothing once *buf is large enough.
func readFileAt(dirfd int, path string, buf *[]byte) ([]byte, error) {
	var (
		fd  int
		err error = unix.EINTR
	)
	for err == unix.EINTR {
		fd, err = unix.Openat(dirfd, path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	b := (*buf)[:0]
	for {
		if len(b) == cap(b) {
			b = append(b, make([]byte, max(4096, cap(b)))...)[:len(b)]
		}
		n, err := unix.Read(fd, b[len(b):cap(b)])
		switch {
		case err == unix.EINTR:
			continue
		case err != nil:
			*buf = b
			return nil, &fs.PathError{Op: "read", Path: path, Err: err}
		case n == 0:
			*buf = b
			return b, nil
		}
		b = b[:len(b)+n]
	}
}

// parseProcPID returns the PID a /proc entry name stands for. Like the
// strconv.Atoi check it replaces, it accepts any integer; a name that is
// not a process directory fails when its stat file is opened.
func parseProcPID(name string) (int, bool) {
	pid, err := strconv.Atoi(name)
	return pid, err == nil
}
