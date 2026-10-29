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
	"sync"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/shellwords"
	"github.com/cfcromn/Throt/internal/signature"
	"github.com/cfcromn/Throt/internal/transport"
)

// The Claude Code hook. As a PreToolUse hook on the Bash tool it receives
// {"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{...}}
// on stdin (https://code.claude.com/docs/en/hooks). When the command is a
// heavy tool, or one throtd learned heavy, it waits, up to --max-wait,
// until throtd's capacity verdict says a heavy job may start, then exits
// 0. To let throtd learn, it reports each simple command's signature
// (internal/signature: program and first argument base names, nothing
// more) to throtd before it starts. It never modifies, approves,
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

// hookDebugEnv set to 1 makes the hook explain on stderr why it ignored its
// input. Claude Code never shows that stderr when the hook exits 0.
const hookDebugEnv = "THROT_HOOK_DEBUG"

const hookUsage = `Usage: throt hook claude-code [--max-wait D]

Claude Code PreToolUse hook for the Bash tool. When the command is a heavy
tool (alone, or after "cd DIR &&", variable assignments, or "time"), or a
command throtd learned heavy because it started a heavy workload before,
wait up to D for throtd to report capacity for a heavy job, then let it run
unchanged. Only the command's signature (program and first argument base
names) is sent to throtd, to learn from. Advisory: no slot is reserved.
It never blocks, denies, or changes a tool call, and returns at once on
any error or when throtd is unreachable. Keep D below the hook's "timeout" in settings.json. With
THROT_HOOK_DEBUG=1 it says on stderr why it ignored an event.

Flags:
`

// hookWaiter holds the dependencies of the hook, injected for tests.
type hookWaiter struct {
	// newCheck builds the capacity check. It is called only for a heavy
	// command, so a light one never touches the config or the daemon.
	newCheck func() checkFunc
	// expect reports the signature of the command about to start to
	// throtd and returns whether throtd learned it heavy. named says the
	// hook already treats it as heavy by name. nil skips it.
	expect func(ctx context.Context, sig string, named bool) (bool, error)
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	// debug, when set, gets a one-line hint about input the hook ignores.
	debug io.Writer
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
		// Exit 1, not exitUsage: Claude Code blocks the tool call on exit 2,
		// so a typo in settings.json must not block every Bash command.
		return exitError{code: 1, msg: "usage: throt hook claude-code [--max-wait D]"}
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
	// The deadline leaves room for a bounded report to throtd before and
	// after the wait, and one last bounded check at maxWait. It also
	// bounds reading stdin, so input held open cannot overrun it.
	ctx, cancel := context.WithTimeout(context.Background(), *maxWait+3*runCallTimeout)
	defer cancel()
	// The config is read once, and only when the daemon is asked.
	socket := sync.OnceValue(func() string { return clientConfig(cfgPath, errw).SocketPath })
	h := hookWaiter{
		newCheck: func() checkFunc { return hookCheck(socket()) },
		expect: func(ctx context.Context, sig string, named bool) (bool, error) {
			return expectHeavy(ctx, socket(), sig, named)
		},
		now:   time.Now,
		sleep: sleepCtx,
	}
	if os.Getenv(hookDebugEnv) == "1" {
		h.debug = errw
	}
	h.claudeCode(ctx, in, out, *maxWait)
	return nil
}

// hookCheck is the advisory capacity verdict for the agent found through
// this process's ancestors. Claude Code runs the hook through a shell, so
// the agent is usually the grandparent.
func hookCheck(socket string) checkFunc {
	r := runner{
		status:    daemonClient{socket: socket}.Status,
		ppid:      os.Getppid(),
		ancestors: processAncestors,
	}
	return r.advisoryCheck()
}

// expectHeavy tells throtd that this process's agent is about to start the
// command with signature sig, and returns whether throtd learned it heavy.
// Any error, including an older throtd without the method, means not
// known: the caller then behaves as before learning existed.
func expectHeavy(ctx context.Context, socket, sig string, named bool) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, runCallTimeout)
	defer cancel()
	var r app.ExpectResult
	err := transport.Call(ctx, socket, "expect", app.ExpectParams{PID: os.Getpid(), Signature: sig, Named: named}, &r)
	return r.Heavy && err == nil, err
}

// claudeCode reads one PreToolUse event and, for a heavy Bash command,
// waits for capacity. It writes at most one JSON object, after waiting.
func (h hookWaiter) claudeCode(ctx context.Context, in io.Reader, out io.Writer, maxWait time.Duration) {
	defer func() { _ = recover() }() // never fail a tool call
	data, err := readHookInput(ctx, in)
	if err != nil {
		h.hint(err.Error())
		return
	}
	command, why := bashCommand(data)
	if why != "" {
		h.hint(why)
		return
	}
	name, args, ok := simpleCommand(command)
	if !ok || maxWait == 0 || lightTools[name] || lightInvocation(name, args) {
		return
	}
	named := heavyTool(name, args)
	// Only the signature of the command reaches throtd, never its text.
	sig, hasSig := signature.FromArgs(name, args)
	learned := false
	if hasSig && h.expect != nil {
		known, err := h.expect(ctx, sig, named)
		learned = err == nil && known && !named
	}
	if !named && !learned {
		return
	}
	msg := h.wait(ctx, maxWait)
	if msg != "" && hasSig && h.expect != nil {
		// The command starts only now: report it again, so throtd still
		// connects it to the workload it starts.
		h.expect(ctx, sig, named)
	}
	if msg != "" && learned {
		msg += " (learned heavy: it started a heavy workload before; see 'throt learned')"
	}
	if msg != "" {
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

// hint writes why the hook ignored its input when debugging is on.
func (h hookWaiter) hint(why string) {
	if h.debug != nil {
		fmt.Fprintf(h.debug, "throt hook: %s; not waiting\n", why)
	}
}

// readHookInput reads stdin, up to maxHookInput bytes, until EOF or ctx
// ends. A stdin held open must not hold the tool call past the hook's
// budget; the reader left behind ends with the process.
func readHookInput(ctx context.Context, in io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(in, maxHookInput+1))
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		switch {
		case r.err != nil:
			return nil, fmt.Errorf("reading stdin: %w", r.err)
		case len(r.data) > maxHookInput:
			return nil, fmt.Errorf("the input is over %d bytes", maxHookInput)
		}
		return r.data, nil
	case <-ctx.Done():
		return nil, errors.New("stdin did not end within the hook's time budget")
	}
}

// bashCommand extracts the command of a PreToolUse event for the Bash
// tool. For anything else it returns why the event does not count.
func bashCommand(data []byte) (command, why string) {
	var ev struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
		ToolInput     struct {
			Command *string `json:"command"`
		} `json:"tool_input"`
	}
	switch {
	case json.Unmarshal(data, &ev) != nil:
		return "", "the input is not a JSON hook event"
	case ev.HookEventName == "":
		return "", "the input has no hook_event_name; Claude Code sends one to every hook"
	case ev.HookEventName != "PreToolUse":
		return "", fmt.Sprintf("hook_event_name is %q, not PreToolUse", ev.HookEventName)
	case ev.ToolName != "Bash":
		return "", fmt.Sprintf("tool_name is %q, not Bash; match the hook to the Bash tool", ev.ToolName)
	case ev.ToolInput.Command == nil:
		return "", "the Bash event has no tool_input.command"
	}
	return *ev.ToolInput.Command, ""
}

// heavyCommand returns the base name of command's heavy tool, and ok false
// when command is not provably a lone heavy tool. It accepts a single
// simple command, optionally preceded by `cd DIR &&` steps, whose command
// word (after variable assignments and `time`) is a plain word naming a
// heavy tool (see heavyTool). Pipes, other lists, redirections,
// substitutions and anything else shellwords refuses do not count.
func heavyCommand(command string) (string, bool) {
	name, args, ok := simpleCommand(command)
	if !ok || !heavyTool(name, args) || lightInvocation(name, args) {
		return "", false
	}
	return name, true
}

// simpleCommand splits command into its command word's base name and the
// raw words after it. It accepts a single simple command, optionally
// preceded by `cd DIR &&` steps, whose command word (after variable
// assignments and `time`) is a plain word. Pipes, other lists,
// redirections, substitutions and anything else shellwords refuses do not
// count.
func simpleCommand(command string) (name string, args []string, ok bool) {
	cmds, ok := shellwords.Split(command)
	if !ok {
		return "", nil, false
	}
	last := len(cmds) - 1
	for _, c := range cmds[:last] {
		if len(c) != 2 || c[0].Raw != "cd" {
			return "", nil, false // only `cd DIR &&` may come first
		}
	}
	words := cmds[last]
	i := 0
	for i < len(words) && (words[i].Raw == "time" || isAssignment(words[i].Raw)) {
		i++
	}
	if i == len(words) || !isPlainCommand(words[i].Raw) {
		return "", nil, false
	}
	args = make([]string, 0, len(words)-i-1)
	for _, w := range words[i+1:] {
		args = append(args, w.Raw)
	}
	return path.Base(words[i].Raw), args, true
}

// lightTools are commands agents run all the time that are never heavy
// workloads: the hook does not report them to throtd, which keeps it from
// contacting throtd on most tool calls and keeps their reports from making
// a heavy command's ambiguous. throt itself handles its own admission.
// ponytail: the commonest cases only; an unlisted light command is merely
// reported, and never learned unless a heavy workload starts with it.
var lightTools = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "echo": true, "printf": true, "pwd": true,
	"cd": true, "grep": true, "rg": true, "egrep": true, "fgrep": true, "sed": true, "awk": true,
	"find": true, "fd": true, "wc": true, "sort": true, "uniq": true, "cut": true, "tr": true,
	"diff": true, "cmp": true, "file": true, "stat": true, "which": true, "type": true, "command": true,
	"test": true, "true": true, "false": true, "mkdir": true, "rmdir": true, "rm": true, "mv": true,
	"cp": true, "ln": true, "touch": true, "chmod": true, "date": true, "sleep": true, "export": true,
	"git": true, "gh": true, "jq": true, "less": true, "tee": true, "basename": true, "dirname": true,
	"realpath": true, "readlink": true, "du": true, "df": true, "ps": true, "kill": true, "pgrep": true,
	"pkill": true, "lsof": true, "curl": true, "wget": true, "ssh": true, "open": true, "throt": true,
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
	"go":      {"version": true, "env": true, "fmt": true, "doc": true, "list": true, "mod": true},
	"npm":     {"view": true, "config": true, "ls": true, "list": true, "outdated": true, "root": true, "bin": true, "prefix": true, "info": true},
	"pnpm":    {"view": true, "config": true, "ls": true, "list": true, "outdated": true, "root": true, "info": true},
	"yarn":    {"info": true, "config": true, "list": true, "why": true, "bin": true},
	"bun":     {"pm": true},
	"cargo":   {"metadata": true, "tree": true, "search": true, "locate-project": true, "pkgid": true},
	"gradle":  gradleLight,
	"gradlew": gradleLight,
	"dotnet":  {"--info": true, "--list-sdks": true, "--list-runtimes": true},
}

// gradleLight are Gradle tasks and flags that only report on the build or
// manage daemons. "dependencies" may download, but compiles and tests
// nothing.
var gradleLight = map[string]bool{
	"tasks": true, "projects": true, "properties": true, "help": true, "dependencies": true,
	"--stop": true, "--status": true,
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
	if !lightSubcommands[name][args[0]] {
		return false
	}
	if name == "gradle" || name == "gradlew" {
		// Gradle runs every task named, so `dependencies test` tests.
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") && !gradleLight[a] {
				return false
			}
		}
	}
	return true
}

// wrappers are build-tool wrapper scripts the process classifier cannot
// know: a wrapper runs as sh and then java, so only the hook, which sees
// the command word, can tell the build apart.
var wrappers = map[string]bool{"gradlew": true, "mvnw": true}

// heavyTool reports whether the command word name with args runs a heavy
// workload: a classified heavy tool, a build wrapper, or a Docker image
// build.
func heavyTool(name string, args []string) bool {
	switch {
	case wrappers[name]:
		return true
	case name == "docker":
		return dockerBuild(args)
	}
	return isHeavy(name)
}

// dockerBuild reports whether docker args build an image: "docker build",
// "docker buildx build" or "docker compose build", subcommand first. Other
// subcommands pass. "docker compose up" in particular stays light: it is
// long-lived, so it would wait for a slot only to run for hours, and its
// containers run in Docker's VM, where no slot covers them.
func dockerBuild(args []string) bool {
	switch {
	case len(args) >= 1 && args[0] == "build":
		return true
	case len(args) >= 2 && (args[0] == "buildx" || args[0] == "compose") && args[1] == "build":
		return true
	}
	return false
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
