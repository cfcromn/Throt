package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/mcp"
	"github.com/cfcromn/Throt/internal/tui"
)

// cmdTop is 'throt top', the terminal UI. Quitting it only ends this
// client; throtd keeps running.
func cmdTop(ctx context.Context, e env, args []string) error {
	if err := noArgs("top", args); err != nil {
		return err
	}
	if !isTerminalStream(e.stdin) || !isTerminalStream(e.stdout) {
		fmt.Fprintln(e.stderr, noTerminal)
		return exitError{code: 1}
	}
	cfg := e.clientConfig()
	_, err := tea.NewProgram(tui.New(daemonClient{socket: cfg.SocketPath}, cfg.SocketPath)).Run()
	return err
}

// noTerminal is what 'throt top' (and throt with no command) says when
// stdin or stdout is not a terminal, say in a pipe or a script.
const noTerminal = "throt top needs a terminal; try 'throt status'"

// isTerminalStream reports whether s is a terminal.
func isTerminalStream(s any) bool {
	f, ok := s.(*os.File)
	return ok && isTerminal(f)
}

// cmdMCP is 'throt mcp', a read-only MCP server on stdio. stdout carries
// MCP messages only; logs go to stderr.
func cmdMCP(ctx context.Context, e env, args []string) error {
	if err := noArgs("mcp", args); err != nil {
		return err
	}
	cfg := e.clientConfig()
	log := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return mcp.New(daemonClient{socket: cfg.SocketPath}, app.Version, log).Serve(ctx, e.stdin, e.stdout)
}
