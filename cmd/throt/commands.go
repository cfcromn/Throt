package main

import (
	"context"
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
}

// command is one throt command. The usage text lists the commands in this
// order.
type command struct {
	name string
	// synopsis is the arguments after the name, e.g. "[--force] PID".
	synopsis string
	// help describes the command in the usage text, one line per line.
	help string
	// flags is set when run parses its arguments with a flag.FlagSet,
	// which prints a detailed usage for -h itself. For the other commands
	// the dispatcher answers -h with commandUsage.
	flags bool
	run   func(ctx context.Context, e env, args []string) error
}

// commandUsage is the usage of a command without its own flag set.
func (c command) commandUsage() string {
	return "usage: throt " + strings.TrimSpace(c.name+" "+c.synopsis) + "\n\n" + c.help + "\n"
}

// isHelp reports whether a is a flag asking for help, as flag reads it.
func isHelp(a string) bool {
	switch a {
	case "-h", "--h", "-help", "--help":
		return true
	}
	return false
}

// commands returns the command table.
func commands() []command {
	return []command{
		{name: "top", run: cmdTop, help: "open the terminal UI (the default with no command)"},
		{name: "status", run: cmdStatus, help: "show host resources and pressure; --json for JSON"},
		{name: "stats", run: cmdStats, flags: true, help: "show per-agent resource usage since throtd started; --json for JSON"},
		{name: "ping", run: cmdPing, help: "check that throtd is reachable"},
		{name: "events", run: cmdEvents, flags: true, help: "print recent daemon events as JSON lines; -f keeps printing new ones"},
		{name: "explain", run: cmdExplain, synopsis: "PID", help: "explain why a heavy workload is running, paused, or queued"},
		{name: "pause", run: cmdPause, synopsis: "PID", help: "pause a heavy workload (SIGSTOP) shown in 'status'"},
		{name: "resume", run: cmdResume, synopsis: "[--force] PID", help: `resume a paused workload (SIGCONT). Also resumes a stopped
process throtd did not pause (listed by 'doctor', e.g. one an
earlier throtd left paused) when it is a heavy workload of an
agent; --force resumes any other such process of yours too,
such as a shell job suspended with Ctrl-Z (prefer fg there)`},
		{name: "run", run: cmdRun, flags: true, synopsis: "[--timeout D] [--no-wait] -- CMD [ARGS...]", help: `wait until throtd grants a heavy slot, reserved for CMD, then
exec CMD (runs immediately if throtd is down)`},
		{name: "reload", run: cmdReload, help: "make throtd re-read its config file"},
		{name: "doctor", run: cmdDoctor, help: "print a sanitized diagnostic report (safe to share)"},
		{name: "mcp", run: cmdMCP, help: "run a read-only MCP server on stdio for coding agents"},
		{name: "config", run: cmdConfig, flags: true, synopsis: "check|show", help: `'check' validates the config file as throtd would, lists what
it changes from the defaults, and warns about risky settings;
'show [--json]' prints the effective config and the source
(default, profile, or file) of each value. Both accept --config PATH.`},
		{name: "hook", run: cmdHook, synopsis: "claude-code [--max-wait D]", help: `Claude Code PreToolUse hook: before a lone heavy Bash command,
wait up to D (default 50s) for heavy-job capacity; never
changes or blocks the command (advisory; fails open)`},
		{name: "setup", run: cmdSetup, synopsis: "claude-code|codex", help: "print the configuration to paste into a coding agent"},
		{name: "completion", run: cmdCompletion, synopsis: "bash|zsh|fish", help: "print a shell completion script"},
		{name: "version", run: cmdVersion, synopsis: "[--json]", help: `print the client version; --json adds the protocol and Go
versions, and throtd's version when it is reachable`},
		{name: "update", run: cmdUpdate, flags: true, synopsis: "[--check] [--version vX.Y.Z] [--pre] [--yes]", help: `install the newest GitHub release over throt and throtd after
verifying its SHA-256; --check only reports (exit 0 when up to
date, 10 when an update is available); the only command that
uses the network`},
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

// exitUsage is the exit status of a usage error, as for most Unix tools.
const exitUsage = 2

// usageError is a one-line usage error: msg, then where to find the usage
// of cmd (e.g. "throt events"). It exits with exitUsage.
func usageError(cmd, msg string) error {
	return exitError{code: exitUsage, msg: msg + "; see '" + cmd + " -h'"}
}

// usageLine is a usage error that already is the usage, such as "usage:
// throt pause PID". It exits with exitUsage.
func usageLine(usage string) error {
	return exitError{code: exitUsage, msg: usage}
}

// noArgs rejects arguments to a command that takes none.
func noArgs(name string, args []string) error {
	if len(args) > 0 {
		return usageError("throt "+name, fmt.Sprintf("unexpected argument %q", args[0]))
	}
	return nil
}

// loadConfig loads the config file named by -config.
func (e env) loadConfig() (config.Config, error) {
	return config.Load(e.cfgPath)
}

// clientConfig is the config of a command that only talks to throtd.
func (e env) clientConfig() config.Config {
	return clientConfig(e.cfgPath, e.stderr)
}

// clientConfig loads the config at path for a client command. A broken
// file must not lock the user out of a running throtd, which keeps its
// last good config (say, to resume what it queued), so on error it warns
// on stderr and returns the defaults, with the default socket path.
func clientConfig(path string, stderr io.Writer) config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		msg := strings.TrimPrefix(err.Error(), "config "+path+": ")
		fmt.Fprintf(stderr, "throt: config %s is invalid: %s; using the default socket\n", path, msg)
		return config.Default()
	}
	return cfg
}

// callTimeout bounds the daemon calls of one-shot commands.
const callTimeout = 5 * time.Second
