package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/app"
)

// defaultWaitTimeout bounds `throt wait`. Its polls keep the reservation
// alive, so it may wait longer than the reservation's TTL.
const defaultWaitTimeout = 30 * time.Minute

// waitReady is the line `throt wait` prints once the slot is held. The
// agent lease waits leaseTTL (60s) for the rerun.
const waitReady = "slot ready: rerun your command within 60s"

const waitUsage = `Usage: throt wait [--timeout D]

Wait until throtd holds a heavy slot for the agent this runs under, then
print "slot ready" and exit 0. It keeps the agent's place in line (taken
by 'throt hook claude-code --defer', or now if there is none), renewing it
once a second; the slot is then held for 60s for the agent's next heavy
command, which the hook admits at once. Exits 1 when D passes (0 waits
forever), when throtd is unreachable or too old to hold a place in line,
or when no agent runs above this process.

Flags:
`

// slotWaiter holds the dependencies of `throt wait`, injected for tests.
type slotWaiter struct {
	// reserve renews the agent's reservation without consuming a grant.
	reserve        func(ctx context.Context) (app.ReserveResult, error)
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
	stdout, stderr io.Writer
}

// cmdWait is 'throt wait [--timeout D]'.
func cmdWait(ctx context.Context, e env, args []string) error {
	flags := flag.NewFlagSet("throt wait", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	flags.Usage = func() { fmt.Fprint(flags.Output(), waitUsage); flags.PrintDefaults() }
	timeout := flags.Duration("timeout", defaultWaitTimeout, "give up after this long (0 waits forever)")
	if err := parseFlags(flags, args, e.stdout); err != nil {
		return err
	}
	switch {
	case flags.NArg() > 0:
		return usageError("throt wait", fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
	case *timeout < 0:
		return usageError("throt wait", "--timeout must not be negative")
	}
	c := daemonClient{socket: e.clientConfig().SocketPath}
	self := app.ReserveParams{PID: os.Getpid(), Wait: true}
	w := slotWaiter{
		reserve: func(ctx context.Context) (app.ReserveResult, error) { return c.Reserve(ctx, self) },
		now:     time.Now,
		sleep:   sleepCtx,
		stdout:  e.stdout,
		stderr:  e.stderr,
	}
	return w.wait(ctx, *timeout)
}

// wait polls the reservation until throtd holds a slot for the agent,
// timeout passes, or throtd fails.
func (w slotWaiter) wait(ctx context.Context, timeout time.Duration) error {
	start := w.now()
	noticed := false
	for {
		cctx, cancel := context.WithTimeout(ctx, runCallTimeout)
		r, err := w.reserve(cctx)
		cancel()
		switch {
		case err != nil && strings.Contains(err.Error(), "unknown method: reserve"):
			return exitError{1, "wait: throtd is too old to hold a place in line; update it (" + err.Error() + ")"}
		case err != nil:
			return exitError{1, "wait: cannot reach throtd: " + err.Error()}
		case r.Granted:
			fmt.Fprintln(w.stdout, waitReady)
			return nil
		case r.Reason == app.ReasonNoAgent:
			return exitError{1, "wait: no agent runs above this process, so there is no place in line to hold"}
		case r.Position == 0:
			return exitError{1, "wait: throtd holds no place in line (" + r.Reason + ")"}
		}
		if !noticed {
			fmt.Fprintf(w.stderr, "throt: waiting for a heavy slot, #%d in line (%s)\n", r.Position, r.Reason)
			noticed = true
		}
		elapsed := w.now().Sub(start)
		if timeout > 0 && elapsed >= timeout {
			return exitError{1, fmt.Sprintf("wait: no heavy slot after %s (%s); the place in line is kept for 10m", timeout, r.Reason)}
		}
		d := runPollInterval
		if timeout > 0 {
			d = min(d, timeout-elapsed)
		}
		if err := w.sleep(ctx, d); err != nil {
			return exitError{1, "wait: " + err.Error()}
		}
	}
}
