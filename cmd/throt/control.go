package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/transport"
)

// cmdPause is 'throt pause PID'.
func cmdPause(ctx context.Context, e env, args []string) error {
	pid, err := pidArg(args, "usage: throt pause PID")
	if err != nil {
		return err
	}
	return sendControl(ctx, e, "pause", pid, false)
}

// cmdResume is 'throt resume [--force] PID'.
func cmdResume(ctx context.Context, e env, args []string) error {
	pidStr, force, err := parseResumeArgs(args)
	if err != nil {
		return err
	}
	pid, err := pidArg([]string{pidStr}, resumeUsage)
	if err != nil {
		return err
	}
	return sendControl(ctx, e, "resume", pid, force)
}

// resumeUsage is the usage of 'throt resume'.
const resumeUsage = "usage: throt resume [--force] PID"

// pidArg returns the single PID in args, or usage as a usage error.
func pidArg(args []string, usage string) (int, error) {
	if len(args) != 1 {
		return 0, usageLine(usage)
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, usageLine(usage)
	}
	return pid, nil
}

// sendControl sends the pause or resume method for pid.
func sendControl(ctx context.Context, e env, method string, pid int, force bool) error {
	cfg := e.clientConfig()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := transport.Call(ctx, cfg.SocketPath, method, app.ControlParams{PID: pid, Force: force}, nil); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%sd pid %d\n", method, pid)
	return nil
}

// parseResumeArgs accepts "PID" with an optional --force before or after it.
func parseResumeArgs(args []string) (pid string, force bool, err error) {
	for _, a := range args {
		switch {
		case a == "--force" || a == "-force":
			force = true
		case pid == "" && !strings.HasPrefix(a, "-"):
			pid = a
		default:
			return "", false, usageLine(resumeUsage)
		}
	}
	if pid == "" {
		return "", false, usageLine(resumeUsage)
	}
	return pid, force, nil
}
