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
	if err := e.atMostOneArg(args); err != nil {
		return err
	}
	return sendControl(ctx, e, "pause", arg(args, 0), false)
}

// cmdResume is 'throt resume [--force] PID'.
func cmdResume(ctx context.Context, e env, args []string) error {
	pid, force, err := parseResumeArgs(args)
	if err != nil {
		return err
	}
	return sendControl(ctx, e, "resume", pid, force)
}

// sendControl sends the pause or resume method for pidArg.
func sendControl(ctx context.Context, e env, method, pidArg string, force bool) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(pidArg)
	if err != nil {
		return fmt.Errorf("usage: throt %s PID", method)
	}
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
			return "", false, fmt.Errorf("usage: throt resume [--force] PID")
		}
	}
	if pid == "" {
		return "", false, fmt.Errorf("usage: throt resume [--force] PID")
	}
	return pid, force, nil
}
