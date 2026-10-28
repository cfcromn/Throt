package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/capacity"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/platform"
)

const (
	runPollInterval   = time.Second
	runNoticeInterval = 10 * time.Second
	// defaultRunTimeout matches the scheduler's default max_queue_wait, so
	// an admission wait gives up no later than queued work would age.
	defaultRunTimeout = 5 * time.Minute
	// runCallTimeout bounds one status call so a stuck daemon cannot hold a
	// command back for long.
	runCallTimeout = 2 * time.Second
	// maxAncestorDepth bounds the walk from this process to its agent.
	maxAncestorDepth = 64

	exitNotFound  = 127
	exitNotExecOK = 126
)

const runUsage = `Usage: throt run [--timeout D] [--no-wait] [--] CMD [ARGS...]

Wait until throtd grants a heavy slot, then exec CMD in place of throt
(same PID, signals, and exit status). The slot is reserved for CMD until it
shows up as a heavy workload, exits, or 60s pass, so two commands asking at
the same moment cannot both take the last slot. It never pauses anything. If
throtd is unreachable, CMD runs immediately; an older throtd without slot
reservation is only asked for advice. By default it waits at most 5m
(the scheduler's default max_queue_wait) and then runs anyway, so a stuck
queue never hangs a build; --timeout 0 waits forever. Unlike a shell, CMD
found only through "." in PATH is refused (exit 126).

Flags:
`

// runOptions are the parsed arguments of `throt run`.
type runOptions struct {
	timeout time.Duration // 0 waits forever; default defaultRunTimeout
	noWait  bool
	argv    []string
}

// parseRunArgs parses the arguments of 'throt run'. Help goes to stdout,
// errors and usage after an error to errw.
func parseRunArgs(args []string, stdout, errw io.Writer) (runOptions, error) {
	var o runOptions
	flags := flag.NewFlagSet("throt run", flag.ContinueOnError)
	flags.SetOutput(errw)
	flags.Usage = func() { fmt.Fprint(flags.Output(), runUsage); flags.PrintDefaults() }
	flags.DurationVar(&o.timeout, "timeout", defaultRunTimeout, "stop waiting after this long and run anyway (0 waits forever)")
	flags.BoolVar(&o.noWait, "no-wait", false, "check capacity once, print the verdict, and run regardless")
	if err := parseFlags(flags, args, stdout); err != nil {
		return o, err
	}
	if o.timeout < 0 {
		return o, usageError("throt run", "--timeout must not be negative")
	}
	o.argv = flags.Args()
	if len(o.argv) == 0 {
		return o, usageError("throt run", "missing command")
	}
	return o, nil
}

// exitError ends throt with a specific exit status, as a shell would.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

// runner holds the dependencies of `throt run`, injected for tests.
type runner struct {
	// acquire asks throtd for an admission lease for this process. When it
	// is nil, or the daemon does not know the method, the runner only asks
	// for advice through status.
	acquire func(ctx context.Context) (app.AcquireResult, error)
	// status fetches a daemon snapshot.
	status func(ctx context.Context) (app.StatusResult, error)
	// ppid is the parent of this process.
	ppid int
	// ancestors returns this process's ancestors, nearest first, starting
	// with ppid. It is called at most once, and only when ppid is not
	// itself an agent.
	ancestors func(ctx context.Context) []int
	lookPath  func(file string) (string, error)
	// exec replaces this process; it returns only on failure.
	exec   func(path string, argv, env []string) error
	env    []string
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	stderr io.Writer
}

// checkFunc asks throtd once whether the command may start now. ok means
// it may (and, with a lease, that its slot is reserved).
type checkFunc func(ctx context.Context) (ok bool, v capacity.Verdict, err error)

// run waits for capacity as o asks, then execs the command. It never
// blocks the command because of a throtd failure: an unreachable daemon
// means run now.
func (r runner) run(ctx context.Context, o runOptions) error {
	check := r.checker()
	ok, v, err := check(ctx)
	if err != nil {
		fmt.Fprintf(r.stderr, "throt: throtd is not reachable (%v); running without waiting\n", err)
		return r.execute(o.argv)
	}
	if o.noWait {
		verdict := "no"
		if ok {
			verdict = "yes"
		}
		fmt.Fprintf(r.stderr, "throt: ok to start a heavy job: %s (%s); running now (--no-wait)\n", verdict, v.Reason)
		return r.execute(o.argv)
	}
	start := r.now()
	var lastNotice time.Time
	waited := false
	for {
		if ok {
			if waited {
				fmt.Fprintf(r.stderr, "throt: capacity available after %s; starting\n", r.now().Sub(start).Round(time.Second))
			}
			return r.execute(o.argv)
		}
		elapsed := r.now().Sub(start)
		if o.timeout > 0 && elapsed >= o.timeout {
			fmt.Fprintf(r.stderr, "throt: still no capacity after %s (%s); running anyway (--timeout)\n", o.timeout, v.Reason)
			return r.execute(o.argv)
		}
		if !waited || r.now().Sub(lastNotice) >= runNoticeInterval {
			fmt.Fprintf(r.stderr, "throt: waiting for a heavy slot (%s): %s...\n", slotSummary(v), v.Reason)
			lastNotice = r.now()
		}
		waited = true
		d := runPollInterval
		if o.timeout > 0 {
			d = min(d, o.timeout-elapsed)
		}
		if err := r.sleep(ctx, d); err != nil {
			return err
		}
		if ok, v, err = check(ctx); err != nil {
			fmt.Fprintf(r.stderr, "throt: lost throtd (%v); running without waiting\n", err)
			return r.execute(o.argv)
		}
	}
}

// checker returns the check run uses: acquire a lease, or, against a
// throtd that predates leases, the advisory status verdict.
func (r runner) checker() checkFunc {
	advisory := r.advisoryCheck()
	if r.acquire == nil {
		return advisory
	}
	legacy := false
	return func(ctx context.Context) (bool, capacity.Verdict, error) {
		if !legacy {
			ctx, cancel := context.WithTimeout(ctx, runCallTimeout)
			res, err := r.acquire(ctx)
			cancel()
			if err == nil {
				v := res.Verdict
				v.OKToStart, v.Reason = res.Granted, res.Reason
				return res.Granted, v, nil
			}
			if !isUnknownMethod(err) {
				return false, capacity.Verdict{}, err
			}
			legacy = true
		}
		return advisory(ctx)
	}
}

// isUnknownMethod reports a throtd too old to know the acquire method.
func isUnknownMethod(err error) bool {
	return strings.Contains(err.Error(), "unknown method: acquire")
}

// advisoryCheck assesses a status snapshot for the nearest agent ancestor.
// It reserves nothing, so two callers checking at once can both start.
func (r runner) advisoryCheck() checkFunc {
	agents := agentResolver(r.ppid, r.ancestors)
	return func(ctx context.Context) (bool, capacity.Verdict, error) {
		st, err := r.callStatus(ctx)
		if err != nil {
			return false, capacity.Verdict{}, err
		}
		v := capacity.Assess(st, agents(ctx, st))
		return v.OKToStart, v, nil
	}
}

func (r runner) callStatus(ctx context.Context) (app.StatusResult, error) {
	ctx, cancel := context.WithTimeout(ctx, runCallTimeout)
	defer cancel()
	return r.status(ctx)
}

// slotSummary is the short state shown while waiting, e.g.
// "2/2 running, 1 reserved, pressure elevated".
func slotSummary(v capacity.Verdict) string {
	reserved := ""
	if v.Reserved > 0 {
		reserved = fmt.Sprintf(", %d reserved", v.Reserved)
	}
	if v.SchedulerMode == "off" {
		return fmt.Sprintf("%d running%s, pressure %s", v.RunningHeavy, reserved, v.Pressure)
	}
	return fmt.Sprintf("%d/%d running%s, pressure %s", v.RunningHeavy, v.HeavyLimit, reserved, v.Pressure)
}

// execute replaces this process with argv. Like a shell, it exits 127 when
// the command does not exist and 126 when it cannot be executed.
func (r runner) execute(argv []string) error {
	path, err := r.lookPath(argv[0])
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return exitError{exitNotFound, fmt.Sprintf("run: %s: command not found", argv[0])}
		}
		return exitError{exitNotExecOK, fmt.Sprintf("run: %v", err)}
	}
	if err := r.exec(path, argv, r.env); err != nil {
		return exitError{exitNotExecOK, fmt.Sprintf("run: exec %s: %v", path, err)}
	}
	return nil
}

// agentResolver returns a function that names the nearest agent ancestor
// of this process in a status snapshot, or "" when there is none. The
// parent is checked first; the full ancestor chain is fetched only if the
// parent is not an agent, and at most once.
func agentResolver(ppid int, ancestors func(context.Context) []int) func(context.Context, app.StatusResult) string {
	var chain []int
	return func(ctx context.Context, st app.StatusResult) string {
		if a := agentFor(st, []int{ppid}); a != "" || ancestors == nil {
			return a
		}
		if chain == nil {
			chain = ancestors(ctx)
			if len(chain) == 0 {
				chain = []int{ppid}
			}
		}
		return agentFor(st, chain)
	}
}

// agentFor returns the label of the first agent in chain (nearest first).
func agentFor(st app.StatusResult, chain []int) string {
	for _, pid := range chain {
		for _, a := range st.Agents {
			if a.Key.PID == pid {
				return a.Agent
			}
		}
	}
	return ""
}

// ancestorChain walks from ppid towards init through procs. When ppid is
// missing from procs the chain is ambiguous, so only ppid is returned.
func ancestorChain(ppid int, procs []domain.ProcessSample) []int {
	parent := make(map[int]int, len(procs))
	for _, p := range procs {
		parent[p.PID] = p.PPID
	}
	chain := []int{ppid}
	seen := map[int]bool{ppid: true}
	for pid := ppid; len(chain) < maxAncestorDepth; {
		pp, ok := parent[pid]
		if !ok || pp <= 0 || seen[pp] {
			break
		}
		chain = append(chain, pp)
		seen[pp] = true
		pid = pp
	}
	return chain
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// cmdRun is `throt run` with the real daemon, process table, and exec.
func cmdRun(ctx context.Context, e env, args []string) error {
	o, err := parseRunArgs(args, e.stdout, e.stderr)
	if err != nil {
		return err
	}
	r := runner{
		ppid:      os.Getppid(),
		lookPath:  exec.LookPath,
		exec:      syscall.Exec,
		env:       os.Environ(),
		now:       time.Now,
		sleep:     sleepCtx,
		stderr:    e.stderr,
		ancestors: processAncestors,
	}
	// A broken config falls back to the default socket, so the command
	// still waits for a throtd that runs on its last good config.
	c := daemonClient{socket: e.clientConfig().SocketPath}
	r.status = c.Status
	// throtd resolves this process's identity and parent itself.
	self := app.AcquireParams{PID: os.Getpid()}
	r.acquire = func(ctx context.Context) (app.AcquireResult, error) { return c.Acquire(ctx, self) }
	return r.run(ctx, o)
}

// processAncestors reads the process table once to find this process's
// ancestors. If the table cannot be read, only the parent is used.
func processAncestors(ctx context.Context) []int {
	ppid := os.Getppid()
	ctx, cancel := context.WithTimeout(ctx, runCallTimeout)
	defer cancel()
	procs, err := platform.NewProcessSampler().SampleProcesses(ctx)
	if err != nil {
		return []int{ppid}
	}
	return ancestorChain(ppid, procs)
}
