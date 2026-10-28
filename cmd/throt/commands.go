package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/config"
)

// env is what a command gets from main: the standard streams and the
// flags given before the command.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	cfgPath        string // -config
	limit          int    // -n
	// usage prints throt's usage to stderr.
	usage func()
}

// command is one throt command. The usage text and the completion scripts
// list the commands in this order.
type command struct {
	name string
	// synopsis is the arguments after the name, e.g. "[--force] PID".
	synopsis string
	// help describes the command in the usage text, one line per line.
	help string
	// brief is the one-line description the completion scripts show.
	brief string
	run   func(ctx context.Context, e env, args []string) error
}

// commands returns the command table.
func commands() []command {
	return []command{
		{name: "top", help: "open the terminal UI (the default with no command)",
			brief: "open the terminal UI", run: cmdTop},
		{name: "status", help: "show host resources and pressure",
			brief: "show host resources and pressure", run: cmdStatus},
		{name: "stats", help: "show per-agent resource usage since throtd started; --json for JSON",
			brief: "per-agent resource usage since throtd started", run: cmdStats},
		{name: "ping", help: "check that throtd is reachable",
			brief: "check that throtd is reachable", run: cmdPing},
		{name: "events", help: "print recent daemon events as JSON lines; -f keeps printing new ones",
			brief: "print recent daemon events", run: cmdEvents},
		{name: "explain", synopsis: "PID", help: "explain why a heavy workload is running, paused, or queued",
			brief: "explain why a heavy workload is running, paused, or queued", run: cmdExplain},
		{name: "pause", synopsis: "PID", help: "pause a heavy workload (SIGSTOP) shown in 'status'",
			brief: "pause a heavy workload", run: cmdPause},
		{name: "resume", synopsis: "[--force] PID", help: `resume a paused workload (SIGCONT). Also resumes a stopped
process throtd did not pause (listed by 'doctor', e.g. one an
earlier throtd left paused) when it is a heavy workload of an
agent; --force resumes any other such process of yours too,
such as a shell job suspended with Ctrl-Z (prefer fg there)`,
			brief: "resume a paused workload", run: cmdResume},
		{name: "run", synopsis: "[--timeout D] [--no-wait] -- CMD [ARGS...]", help: `wait until throtd grants a heavy slot, reserved for CMD, then
exec CMD (runs immediately if throtd is down)`,
			brief: "wait for heavy-job capacity, then run a command", run: cmdRun},
		{name: "reload", help: "make throtd re-read its config file",
			brief: "make throtd re-read its config file", run: cmdReload},
		{name: "doctor", help: "print a sanitized diagnostic report (safe to share)",
			brief: "print a sanitized diagnostic report", run: cmdDoctor},
		{name: "mcp", help: "run a read-only MCP server on stdio for coding agents",
			brief: "run a read-only MCP server on stdio", run: cmdMCP},
		{name: "config", synopsis: "check|show", help: `'check' validates the config file as throtd would, lists what
it changes from the defaults, and warns about risky settings;
'show [--json]' prints the effective config and the source
(default, profile, or file) of each value. Both accept --config PATH.`,
			brief: "check or show the config file", run: cmdConfig},
		{name: "hook", synopsis: "claude-code [--max-wait D]", help: `Claude Code PreToolUse hook: before a lone heavy Bash command,
wait up to D (default 50s) for heavy-job capacity; never
changes or blocks the command (advisory; fails open)`,
			brief: "Claude Code hook that waits for heavy-job capacity", run: cmdHook},
		{name: "setup", synopsis: "claude-code|codex", help: "print the configuration to paste into a coding agent",
			brief: "print the configuration to paste into a coding agent", run: cmdSetup},
		{name: "completion", synopsis: "bash|zsh|fish", help: "print a shell completion script",
			brief: "print a shell completion script", run: cmdCompletion},
		{name: "version", synopsis: "[--json]", help: `print the client version; --json adds the protocol and Go
versions, and throtd's version when it is reachable`,
			brief: "print the client version", run: cmdVersion},
		{name: "update", synopsis: "[--check] [--version vX.Y.Z] [--pre] [--yes]", help: `install the newest GitHub release over throt and throtd after
verifying its SHA-256; --check only reports (exit 0 when up to
date, 10 when an update is available); the only command that
uses the network`,
			brief: "install the newest verified release in place", run: cmdUpdate},
	}
}

// lookup returns the command called name.
func lookup(name string) (command, bool) {
	for _, c := range commands() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// usageText is throt's usage, up to the flag defaults.
func usageText() string {
	// A short head shares a line with its help; a long one stands alone.
	const indent = "             "
	var b strings.Builder
	b.WriteString("Usage: throt [flags] [command]\n\nCommands:\n")
	for _, c := range commands() {
		head := strings.TrimSpace(c.name + " " + c.synopsis)
		lines := strings.Split(c.help, "\n")
		if len(head) <= len(indent)-4 {
			fmt.Fprintf(&b, "  %-*s%s\n", len(indent)-2, head, lines[0])
			lines = lines[1:]
		} else {
			fmt.Fprintf(&b, "  %s\n", head)
		}
		for _, l := range lines {
			b.WriteString(indent + l + "\n")
		}
	}
	b.WriteString("\nFlags:\n")
	return b.String()
}

// atMostOneArg reports too many arguments the way throt always has: the
// usage, then the error.
func (e env) atMostOneArg(args []string) error {
	if len(args) > 1 {
		e.usage()
		return errors.New("too many arguments")
	}
	return nil
}

// arg returns args[i], or "" when there is none.
func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// loadConfig loads the config file named by -config.
func (e env) loadConfig() (config.Config, error) {
	return config.Load(e.cfgPath)
}

// callTimeout bounds the daemon calls of one-shot commands.
const callTimeout = 5 * time.Second
