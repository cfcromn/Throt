package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/shellwords"
)

// The Claude Code hook. As a PreToolUse hook on the Bash tool it receives
// {"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{...}}
// on stdin (https://code.claude.com/docs/en/hooks). When the command is a
// heavy tool it waits, up to --max-wait, until throtd's capacity verdict
// says a heavy job may start, then exits 0. It never modifies, approves,
// or denies the call, so the command runs exactly as Claude wrote it under
// the user's own permission rules. The only output is an optional
// systemMessage (shown to the user) after it has waited; nothing is
// written while waiting. Admission is advisory: the hook exits before the
// command starts, so it cannot hold a lease for it.

const (
	// maxHookInput bounds what the hook reads from stdin. Larger input is
	// left alone.
	maxHookInput = 1 << 20
	// defaultHookMaxWait stays well under the hook timeout the setup
	// snippet configures, and under a 60s timeout should a user pick one.
	defaultHookMaxWait = 50 * time.Second
)

const hookUsage = `Usage: throt hook claude-code [--max-wait D]

Claude Code PreToolUse hook for the Bash tool. When the command is a heavy
tool (alone, or after "cd DIR &&", variable assignments, or "time"), wait up
to D for throtd to report capacity for a heavy job, then let it run
unchanged. Advisory: no slot is reserved. It never blocks, denies, or
changes a tool call, and returns at once on any error or when throtd is
unreachable. Keep D below the hook's "timeout" in settings.json.

Flags:
`

// hookWaiter holds the dependencies of the hook, injected for tests.
type hookWaiter struct {
	// newCheck builds the capacity check. It is called only for a heavy
	// command, so a light one never touches the config or the daemon.
	newCheck func() checkFunc
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
}

// cmdHook is 'throt hook'. It touches the config and the daemon only for
// heavy commands.
func cmdHook(_ context.Context, e env, args []string) error {
	return runHook(args, e.cfgPath, e.stdin, e.stdout, e.stderr)
}

// runHook handles `throt hook AGENT [flags]`. For the supported agent it
// fails open: it returns nil, without waiting, on any problem, including
// bad flags (reported on stderr, which Claude never sees on exit 0).
func runHook(args []string, cfgPath string, in io.Reader, out, errw io.Writer) error {
	if len(args) == 0 || args[0] != "claude-code" {
		return usageLine("usage: throt hook claude-code [--max-wait D]")
	}
	flags := flag.NewFlagSet("throt hook", flag.ContinueOnError)
	flags.SetOutput(errw)
	flags.Usage = func() { fmt.Fprint(flags.Output(), hookUsage); flags.PrintDefaults() }
	maxWait := flags.Duration("max-wait", defaultHookMaxWait, "wait at most this long for capacity (keep it below the hook timeout)")
	err := parseFlags(flags, args[1:], out)
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	if err != nil || flags.NArg() > 0 || *maxWait < 0 {
		fmt.Fprintln(errw, "throt hook: bad arguments; not waiting")
		return nil
	}
	// The deadline leaves room for one last bounded check at maxWait.
	ctx, cancel := context.WithTimeout(context.Background(), *maxWait+runCallTimeout)
	defer cancel()
	h := hookWaiter{
		newCheck: func() checkFunc { return hookCheck(cfgPath) },
		now:      time.Now,
		sleep:    sleepCtx,
	}
	h.claudeCode(ctx, in, out, *maxWait)
	return nil
}

// hookCheck is the advisory capacity verdict for the agent found through
// this process's ancestors. Claude Code runs the hook through a shell, so
// the agent is usually the grandparent.
func hookCheck(cfgPath string) checkFunc {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return func(context.Context) (bool, app.Verdict, error) { return false, app.Verdict{}, err }
	}
	r := runner{
		status:    daemonClient{socket: cfg.SocketPath}.Status,
		ppid:      os.Getppid(),
		ancestors: processAncestors,
	}
	return r.advisoryCheck()
}

// claudeCode reads one PreToolUse event and, for a heavy Bash command,
// waits for capacity. It writes at most one JSON object, after waiting.
func (h hookWaiter) claudeCode(ctx context.Context, in io.Reader, out io.Writer, maxWait time.Duration) {
	defer func() { _ = recover() }() // never fail a tool call
	command, ok := readBashCommand(in)
	if !ok {
		return
	}
	if _, heavy := heavyCommand(command); !heavy || maxWait == 0 {
		return
	}
	if msg := h.wait(ctx, maxWait); msg != "" {
		resp, err := json.Marshal(struct {
			SystemMessage string `json:"systemMessage"`
		}{msg})
		if err == nil {
			out.Write(append(resp, '\n'))
		}
	}
}

// wait polls the capacity check until it says ok, maxWait passes, or the
// check fails. It returns a notice for the user when it waited at all.
func (h hookWaiter) wait(ctx context.Context, maxWait time.Duration) string {
	check := h.newCheck()
	start := h.now()
	ok, v, err := check(ctx)
	waited := false
	for {
		if err != nil {
			return "" // throtd unreachable or the deadline passed: run now
		}
		elapsed := h.now().Sub(start)
		if ok {
			if waited {
				return fmt.Sprintf("throt: waited %s for a heavy-job slot", elapsed.Round(time.Second))
			}
			return ""
		}
		if elapsed >= maxWait {
			return fmt.Sprintf("throt: no heavy-job slot after %s (%s); running anyway", maxWait, v.Reason)
		}
		waited = true
		if err := h.sleep(ctx, min(runPollInterval, maxWait-elapsed)); err != nil {
			return fmt.Sprintf("throt: no heavy-job slot after %s (%s); running anyway", maxWait, v.Reason)
		}
		ok, v, err = check(ctx)
	}
}

// readBashCommand extracts the command of a PreToolUse event for the Bash
// tool, and ok false for anything else.
func readBashCommand(in io.Reader) (string, bool) {
	data, err := io.ReadAll(io.LimitReader(in, maxHookInput+1))
	if err != nil || len(data) > maxHookInput {
		return "", false
	}
	var ev struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
		ToolInput     struct {
			Command *string `json:"command"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.HookEventName != "PreToolUse" || ev.ToolName != "Bash" || ev.ToolInput.Command == nil {
		return "", false
	}
	return *ev.ToolInput.Command, true
}

// heavyCommand returns the base name of command's heavy tool, and ok false
// when command is not provably a lone heavy tool. It accepts a single
// simple command, optionally preceded by `cd DIR &&` steps, whose command
// word (after variable assignments and `time`) is a plain word naming a
// heavy tool per agent.Classify. Pipes, other lists, redirections,
// substitutions and anything else shellwords refuses do not count.
func heavyCommand(command string) (string, bool) {
	cmds, ok := shellwords.Split(command)
	if !ok {
		return "", false
	}
	last := len(cmds) - 1
	for _, c := range cmds[:last] {
		if len(c) != 2 || c[0].Raw != "cd" {
			return "", false // only `cd DIR &&` may come first
		}
	}
	words := cmds[last]
	i := 0
	for i < len(words) && (words[i].Raw == "time" || isAssignment(words[i].Raw)) {
		i++
	}
	if i == len(words) || !isPlainCommand(words[i].Raw) {
		return "", false
	}
	name := path.Base(words[i].Raw)
	if !isHeavy(name) {
		return "", false
	}
	args := make([]string, 0, len(words)-i-1)
	for _, w := range words[i+1:] {
		args = append(args, w.Raw)
	}
	if lightInvocation(name, args) {
		return "", false
	}
	return name, true
}

// lightFlags make any heavy tool return at once: agents run these
// constantly, and waiting for a heavy slot would stall them for nothing.
var lightFlags = map[string]bool{
	"-v": true, "-V": true, "--version": true, "-version": true, "version": true,
	"-h": true, "--help": true, "-help": true, "help": true,
	"-n": true, "--dry-run": true, "--just-print": true,
}

// lightSubcommands are per-tool first arguments that only read state.
// ponytail: a short allowlist of the commonest cases; anything unlisted
// still waits (the safe direction), extend when agents hit one often.
var lightSubcommands = map[string]map[string]bool{
	"go":     {"version": true, "env": true, "fmt": true, "doc": true, "list": true, "mod": true},
	"npm":    {"view": true, "config": true, "ls": true, "list": true, "outdated": true, "root": true, "bin": true, "prefix": true, "info": true},
	"pnpm":   {"view": true, "config": true, "ls": true, "list": true, "outdated": true, "root": true, "info": true},
	"yarn":   {"info": true, "config": true, "list": true, "why": true, "bin": true},
	"bun":    {"pm": true},
	"cargo":  {"metadata": true, "tree": true, "search": true, "locate-project": true, "pkgid": true},
	"gradle": {"tasks": true, "projects": true, "properties": true},
	"dotnet": {"--info": true, "--list-sdks": true, "--list-runtimes": true},
}

// lightInvocation reports whether args make a heavy tool's run trivial.
func lightInvocation(name string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	if lightFlags[args[0]] {
		return true
	}
	for _, a := range args {
		if a == "--version" || a == "--help" || a == "--dry-run" {
			return true
		}
	}
	return lightSubcommands[name][args[0]]
}

// isHeavy reports whether an executable base name is a heavy workload.
func isHeavy(name string) bool {
	c := agent.Classify(domain.ProcessSample{Name: name})
	return c != domain.ClassUnknown && c != domain.ClassAgent
}

// isAssignment reports whether a raw word is a shell variable assignment
// such as FOO=1 or FOO="a b": an unquoted name, then =.
func isAssignment(raw string) bool {
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '=':
			return i > 0
		case c == '_' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z':
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return false
}

// isPlainCommand reports whether a raw command word needs no quoting and
// expands to itself: no globs, tilde, parameters, quotes or escapes. A
// trailing slash (a directory) does not count.
func isPlainCommand(raw string) bool {
	if raw == "" || raw[len(raw)-1] == '/' {
		return false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("_./+-", c) >= 0) {
			return false
		}
	}
	return true
}
