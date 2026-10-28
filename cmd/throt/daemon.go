package main

import (
	"context"
	"fmt"
	"io"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// daemonClient implements tui.Client and mcp.Client over the local protocol.
type daemonClient struct{ socket string }

func (c daemonClient) Status(ctx context.Context) (app.StatusResult, error) {
	var st app.StatusResult
	err := transport.Call(ctx, c.socket, "status", nil, &st)
	return st, err
}

// Acquire asks for an admission lease for the process p names.
func (c daemonClient) Acquire(ctx context.Context, p app.AcquireParams) (app.AcquireResult, error) {
	var r app.AcquireResult
	err := transport.Call(ctx, c.socket, "acquire", p, &r)
	return r, err
}

func (c daemonClient) Events(ctx context.Context, limit int) ([]domain.Event, error) {
	var events []domain.Event
	err := transport.Call(ctx, c.socket, "events", app.EventsParams{Limit: limit}, &events)
	return events, err
}

func (c daemonClient) Stats(ctx context.Context) (app.StatsResult, error) {
	var r app.StatsResult
	err := transport.Call(ctx, c.socket, "stats", nil, &r)
	return r, err
}

// EventsSince returns up to limit events newer than seq, or the newest limit
// events when seq is 0.
func (c daemonClient) EventsSince(ctx context.Context, seq uint64, limit int) ([]domain.Event, error) {
	var events []domain.Event
	err := transport.Call(ctx, c.socket, "events", app.EventsParams{Limit: limit, Since: seq}, &events)
	return events, err
}

func (c daemonClient) Pause(ctx context.Context, k domain.ProcessKey) error {
	return transport.Call(ctx, c.socket, "pause", app.ControlParams{PID: k.PID, Start: k.Start}, nil)
}

func (c daemonClient) Resume(ctx context.Context, k domain.ProcessKey) error {
	return transport.Call(ctx, c.socket, "resume", app.ControlParams{PID: k.PID, Start: k.Start}, nil)
}

// cmdPing is 'throt ping'.
func cmdPing(ctx context.Context, e env, args []string) error {
	if err := noArgs("ping", args); err != nil {
		return err
	}
	cfg := e.clientConfig()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var r app.PingResult
	if err := transport.Call(ctx, cfg.SocketPath, "ping", nil, &r); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "throtd %s (protocol v%d, pid %d, up %s)\n", r.Version, r.Protocol, r.PID, r.Uptime)
	return nil
}

// cmdReload is 'throt reload'.
func cmdReload(ctx context.Context, e env, args []string) error {
	if err := noArgs("reload", args); err != nil {
		return err
	}
	cfg, err := e.loadConfig()
	if err != nil {
		// throtd reports the invalid file itself and keeps its config, so
		// reach it at the best socket path the file allows.
		if cfg.SocketPath == "" {
			cfg.SocketPath = config.DefaultSocketPath()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var r app.ReloadResult
	if err := transport.Call(ctx, cfg.SocketPath, "reload", nil, &r); err != nil {
		return err
	}
	printReload(e.stdout, r)
	return nil
}

func printReload(w io.Writer, r app.ReloadResult) {
	if len(r.Changed) == 0 {
		fmt.Fprintln(w, "reloaded: no changes applied")
	}
	for _, c := range r.Changed {
		fmt.Fprintln(w, "applied: "+c)
	}
	for _, c := range r.RequiresRestart {
		fmt.Fprintln(w, "requires restart: "+c)
	}
}
