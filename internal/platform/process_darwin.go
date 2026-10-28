//go:build darwin

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"golang.org/x/sys/unix"
)

// psPath is invoked directly, never through a shell.
const psPath = "/bin/ps"

// maxPSOutput bounds how much ps output is read.
const maxPSOutput = 32 << 20

// NewProcessSampler returns a process sampler for the current OS. On macOS
// it runs /bin/ps for the process table: /bin/ps is setuid root, so it
// reads the CPU time and RSS of other users' processes, which
// proc_pidinfo refuses to an unprivileged caller and which the host CPU
// estimate needs. Names are read in-process where possible (see
// lookupPSNames). The caller's context deadline bounds how long ps may run.
func NewProcessSampler() ProcessSampler { return &darwinProcessSampler{} }

// darwinProcessSampler is not safe for concurrent use.
type darwinProcessSampler struct {
	names psNameCache
}

func (s *darwinProcessSampler) SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	// The stats pass has no attacker-controlled text and defines the process
	// set; names are looked up separately (see lookupPSNames), and only for
	// processes not named yet (see psNameCache).
	stats, err := runPS(ctx, psStatColumns)
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	procs, err := parsePSStats(bytes.NewReader(stats), time.Local)
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	if len(procs) == 0 {
		return nil, errors.New("sample processes: ps listed no processes")
	}
	if err := s.names.name(procs, func(pids []int) (map[int]string, error) {
		return lookupPSNames(ctx, pids)
	}); err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	return procs, nil
}

// maxPSNameQueries is how many PIDs lookupPSNames asks ps about one at a
// time before it reads every name in one pass instead. Measured on macOS
// 26 with about 1,200 processes: a one-PID ps costs about 2 ms of CPU, a
// full names pass about 50 ms, and a ps given two or more PIDs about
// 500 ms, so PIDs are never batched into one -p list.
const maxPSNameQueries = 16

// lookupPSNames returns names for pids, as parsePSNames maps them. Each
// name is read in-process from kern.procargs2 (about 25 µs, no fork);
// only processes it cannot read, those of other users, are asked of ps.
// Measured on macOS 26 on a busy host with about 1,200 processes, new
// PIDs made the ps name lookups cost 30 to 70 ms of CPU per sample, more
// than the stats pass.
func lookupPSNames(ctx context.Context, pids []int) (map[int]string, error) {
	names := make(map[int]string, len(pids))
	var rest []int
	for _, pid := range pids {
		if comm, err := procComm(pid); err == nil {
			names[pid] = psName(comm)
		} else {
			rest = append(rest, pid)
		}
	}
	if len(rest) > maxPSNameQueries {
		out, err := runPS(ctx, psNameColumns)
		if err != nil {
			return nil, err
		}
		all, err := parsePSNames(bytes.NewReader(out))
		if err != nil {
			return nil, err
		}
		for pid, name := range all {
			if _, ok := names[pid]; !ok {
				names[pid] = name
			}
		}
		return names, nil
	}
	for _, pid := range rest {
		out, err := execPS(ctx, "-o", psNameColumns, "-p", strconv.Itoa(pid))
		// ps exits 1 with no output when the PID no longer exists; that
		// process just stays unnamed. A context error has no exit status.
		var exitErr *exec.ExitError
		if err != nil && (!errors.As(err, &exitErr) || len(bytes.TrimSpace(out)) != 0) {
			return nil, err
		}
		got, err := parsePSNames(bytes.NewReader(out))
		if err != nil {
			return nil, err
		}
		if name, ok := got[pid]; ok {
			names[pid] = name
		}
	}
	return names, nil
}

// procComm returns what ps prints as comm for pid, read from the
// kern.procargs2 sysctl. It fails for processes of other users (EPERM:
// /bin/ps is setuid root and can read them), for zombies and for PIDs that
// do not exist; the caller then asks ps.
func procComm(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("procargs of pid %d: invalid pid", pid)
	}
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", fmt.Errorf("procargs of pid %d: %w", pid, err)
	}
	return procargsComm(b)
}

// NewProcessController returns a process controller for the current OS. On
// macOS it verifies identity with /bin/ps for the one PID and then signals
// with kill(2).
//
// Residual risk: macOS has no pidfd, so verification and signaling are two
// steps. If the process exits and its PID is reused by another process of
// the same user, started within the same second (ps lstart is
// second-granular), in the window between the ps call and kill(2), the
// signal reaches the new process. PIDs are allocated sequentially, so this
// needs the PID space to wrap within that window.
func NewProcessController() ProcessController { return darwinProcessController{} }

// NewProcessTools returns a sampler and a controller whose identities agree.
// On macOS both derive Start from ps lstart, so they share no state.
func NewProcessTools() (ProcessSampler, ProcessController) {
	return NewProcessSampler(), NewProcessController()
}

type darwinProcessController struct{}

func (c darwinProcessController) Stop(ctx context.Context, key domain.ProcessKey, uid uint32) error {
	return verifyThenKill(ctx, c.identity, key, uid, syscall.SIGSTOP)
}

func (c darwinProcessController) Continue(ctx context.Context, key domain.ProcessKey, uid uint32) error {
	return verifyThenKill(ctx, c.identity, key, uid, syscall.SIGCONT)
}

func (c darwinProcessController) Gone(ctx context.Context, key domain.ProcessKey) (bool, error) {
	return goneBy(ctx, c.identity, key)
}

// identity runs ps for pid. Start and uid match what the sampler reports.
func (darwinProcessController) identity(ctx context.Context, pid int) (time.Time, uint32, error) {
	if err := checkIdentityPID(pid); err != nil {
		return time.Time{}, 0, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	out, err := execPS(ctx, "-o", psIdentityColumns, "-p", strconv.Itoa(pid))
	// ps exits 1 with no output when the PID does not exist. A context error
	// comes back with no output too, so only an exit status counts as gone.
	var exitErr *exec.ExitError
	if len(bytes.TrimSpace(out)) == 0 && (err == nil || errors.As(err, &exitErr)) {
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, domain.ErrProcessGone)
	}
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	start, uid, err := parsePSIdentity(out, time.Local)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	return start, uid, nil
}

// Identify runs ps -o uid=,ppid=,lstart= for pid. Start and UID match what
// the sampler reports; Name is left empty.
func (darwinProcessController) Identify(ctx context.Context, pid int) (domain.ProcessSample, error) {
	if err := checkIdentityPID(pid); err != nil {
		return domain.ProcessSample{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	out, err := execPS(ctx, "-o", psIdentifyColumns, "-p", strconv.Itoa(pid))
	var exitErr *exec.ExitError
	if len(bytes.TrimSpace(out)) == 0 && (err == nil || errors.As(err, &exitErr)) {
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, domain.ErrProcessGone)
	}
	if err != nil {
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	p, err := parsePSIdentify(out, time.Local)
	if err != nil {
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, err)
	}
	p.PID = pid
	return p, nil
}

// runPS runs ps for all processes with the given -o columns and returns its
// bounded output.
func runPS(ctx context.Context, columns string) ([]byte, error) {
	return execPS(ctx, "-axww", "-o", columns)
}

// execPS runs ps with args and returns its bounded output. When ps exits
// non-zero, its output is returned along with the error.
func execPS(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, psPath, args...)
	// The C locale fixes the lstart format; TZ is kept so ps and Go agree on
	// local time. Nothing else from the environment is passed on.
	cmd.Env = []string{"LC_ALL=C"}
	if tz, ok := os.LookupEnv("TZ"); ok {
		cmd.Env = append(cmd.Env, "TZ="+tz)
	}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ps: %w", err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxPSOutput+1))
	overflow := len(out) > maxPSOutput
	if overflow || readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case overflow:
		return nil, fmt.Errorf("ps output exceeds %d bytes", maxPSOutput)
	case readErr != nil:
		return nil, fmt.Errorf("read ps output: %w", readErr)
	case waitErr != nil:
		return out, fmt.Errorf("ps: %w", waitErr)
	}
	return out, nil
}
