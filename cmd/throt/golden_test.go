package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
	"github.com/cfcromn/Throt/internal/stats"
	"github.com/cfcromn/Throt/internal/transport"
)

// The golden tests run the real main in a child process (this test binary,
// re-executed) against a fake throtd and compare its exit status, stdout,
// and stderr byte for byte with the expectations in goldenWant.
//
// To regenerate the expectations, run
//
//	THROT_GOLDEN_DUMP=/path/out.go go test -run TestGolden ./cmd/throt
//
// and paste the function it writes over goldenWant.

// goldenArgsEnv carries the child's arguments as a JSON array.
const goldenArgsEnv = "THROT_GOLDEN_ARGS"

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(goldenArgsEnv); ok {
		var args []string
		if err := json.Unmarshal([]byte(v), &args); err != nil {
			fmt.Fprintln(os.Stderr, "golden: bad args:", err)
			os.Exit(2)
		}
		os.Args = append([]string{"throt"}, args...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type goldenCase struct {
	name  string
	args  []string // "$DIR" is replaced by the test directory
	stdin string
	down  bool // no daemon at the default socket
}

type golden struct {
	code           int
	stdout, stderr string
}

// goldenStart is the start time of the fake daemon's processes.
var goldenStart = time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)

func goldenCases() []goldenCase {
	return []goldenCase{
		{name: "help", args: []string{"-h"}},
		{name: "help long", args: []string{"--help"}},
		{name: "bad global flag", args: []string{"-bogus"}},
		{name: "unknown command", args: []string{"frob"}},
		{name: "unknown command with args", args: []string{"frob", "a", "b"}},
		{name: "too many arguments", args: []string{"ping", "a", "b"}},
		{name: "invalid config", args: []string{"-config", "$DIR/bad.json", "status"}},

		{name: "ping", args: []string{"ping"}},
		{name: "ping down", args: []string{"ping"}, down: true},
		{name: "ping extra", args: []string{"ping", "x"}},
		{name: "ping -h", args: []string{"ping", "-h"}},

		{name: "status", args: []string{"status"}},
		{name: "status down", args: []string{"status"}, down: true},
		{name: "status -h", args: []string{"status", "-h"}},
		{name: "status --json", args: []string{"status", "--json"}},
		{name: "status down invalid config", args: []string{"-config", "$DIR/bad.json", "status"}, down: true},
		{name: "ping invalid config", args: []string{"-config", "$DIR/bad.json", "ping"}},
		{name: "resume invalid config", args: []string{"-config", "$DIR/bad.json", "resume", "4312"}},
		{name: "events invalid config", args: []string{"-config", "$DIR/bad.json", "events", "-n", "1"}},
		{name: "hook invalid config", args: []string{"-config", "$DIR/bad.json", "hook", "claude-code", "--max-wait", "1s"}, stdin: `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"make -j8"}}`},

		{name: "stats", args: []string{"stats"}},
		{name: "stats --json", args: []string{"stats", "--json"}},
		{name: "stats -json", args: []string{"stats", "-json"}},
		{name: "stats -h", args: []string{"stats", "-h"}},
		{name: "stats -bogus", args: []string{"stats", "-bogus"}},
		{name: "stats extra", args: []string{"stats", "x"}},

		{name: "learned", args: []string{"learned"}},
		{name: "learned --forget", args: []string{"learned", "--forget", "python train.py"}},
		{name: "learned --clear", args: []string{"learned", "--clear"}},
		{name: "learned both", args: []string{"learned", "--clear", "--forget", "x"}},
		{name: "learned -h", args: []string{"learned", "-h"}},
		{name: "learned extra", args: []string{"learned", "x"}},
		{name: "learned down", args: []string{"learned"}, down: true},

		{name: "events", args: []string{"events"}},
		{name: "events -n", args: []string{"events", "-n", "1"}},
		{name: "global -n events", args: []string{"-n", "1", "events"}},
		{name: "global -n events -n", args: []string{"-n", "1", "events", "-n", "2"}},
		{name: "events -h", args: []string{"events", "-h"}},
		{name: "events -bogus", args: []string{"events", "-bogus"}},
		{name: "events extra", args: []string{"events", "x"}},
		{name: "events down", args: []string{"events"}, down: true},

		{name: "explain", args: []string{"explain", "4312"}},
		{name: "explain not heavy", args: []string{"explain", "99"}},
		{name: "explain missing", args: []string{"explain"}},
		{name: "explain not a pid", args: []string{"explain", "x"}},
		{name: "explain -h", args: []string{"explain", "-h"}},
		{name: "explain too many", args: []string{"explain", "1", "2"}},

		{name: "pause", args: []string{"pause", "4312"}},
		{name: "pause missing", args: []string{"pause"}},
		{name: "pause -h", args: []string{"pause", "-h"}},
		{name: "pause --force", args: []string{"pause", "--force", "4312"}},
		{name: "pause refused", args: []string{"pause", "13"}},
		{name: "resume", args: []string{"resume", "4312"}},
		{name: "resume --force first", args: []string{"resume", "--force", "4312"}},
		{name: "resume --force last", args: []string{"resume", "4312", "--force"}},
		{name: "resume -force", args: []string{"resume", "-force", "4312"}},
		{name: "resume missing", args: []string{"resume"}},
		{name: "resume -h", args: []string{"resume", "-h"}},
		{name: "resume two pids", args: []string{"resume", "1", "2"}},
		{name: "resume not a pid", args: []string{"resume", "x"}},

		{name: "reload", args: []string{"reload"}},
		{name: "reload invalid config", args: []string{"-config", "$DIR/bad.json", "reload"}},
		{name: "reload -h", args: []string{"reload", "-h"}},

		{name: "run -h", args: []string{"run", "-h"}},
		{name: "run missing", args: []string{"run"}},
		{name: "run negative timeout", args: []string{"run", "--timeout", "-1s", "--", "true"}},
		{name: "run -bogus", args: []string{"run", "-bogus", "true"}},
		{name: "run", args: []string{"run", "--", "sh", "-c", "echo out; echo err >&2; exit 3"}},
		{name: "run no dashes", args: []string{"run", "sh", "-c", "exit 4"}},
		{name: "run --no-wait", args: []string{"run", "--no-wait", "--", "sh", "-c", "exit 0"}},
		{name: "run down", args: []string{"run", "--", "sh", "-c", "echo hi"}, down: true},
		{name: "run not found", args: []string{"run", "--", "throt-no-such-command"}},
		{name: "run invalid config", args: []string{"-config", "$DIR/bad.json", "run", "--", "true"}},

		{name: "config", args: []string{"config"}},
		{name: "config -h", args: []string{"config", "-h"}},
		{name: "config check -h", args: []string{"config", "check", "-h"}},
		{name: "config bogus", args: []string{"config", "bogus"}},
		{name: "config check no file", args: []string{"config", "check"}},
		{name: "config check", args: []string{"config", "check", "--config", "$DIR/plain.json"}},
		{name: "config check global", args: []string{"-config", "$DIR/plain.json", "config", "check"}},
		{name: "config check invalid", args: []string{"config", "check", "--config", "$DIR/bad.json"}},
		{name: "config check extra", args: []string{"config", "check", "x"}},
		{name: "config check --json", args: []string{"config", "check", "--json"}},
		{name: "config show", args: []string{"config", "show", "--config", "$DIR/cool.json"}},
		{name: "config show --json", args: []string{"config", "show", "--json", "--config", "$DIR/cool.json"}},

		{name: "mcp", args: []string{"mcp"}},
		{name: "mcp -h", args: []string{"mcp", "-h"}},
		{name: "mcp --help", args: []string{"mcp", "--help"}},
		{name: "top -h", args: []string{"top", "-h"}},
		{name: "doctor -h", args: []string{"doctor", "-h"}},
		{name: "mcp too many", args: []string{"mcp", "a", "b"}},
		{name: "top too many", args: []string{"top", "a", "b"}},
		{name: "top no terminal", args: []string{"top"}},
		{name: "no command no terminal", args: []string{}},
		{name: "doctor too many", args: []string{"doctor", "a", "b"}},
		{name: "mcp initialize", args: []string{"mcp"}, stdin: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n"},

		{name: "hook", args: []string{"hook"}},
		{name: "hook -h", args: []string{"hook", "-h"}},
		{name: "hook codex", args: []string{"hook", "codex"}},
		{name: "hook claude-code -h", args: []string{"hook", "claude-code", "-h"}},
		{name: "hook claude-code bad flag", args: []string{"hook", "claude-code", "--max-wait", "x"}},
		{name: "hook claude-code extra", args: []string{"hook", "claude-code", "x"}},
		{name: "hook claude-code light", args: []string{"hook", "claude-code"}, stdin: `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`},
		{name: "hook claude-code heavy", args: []string{"hook", "claude-code", "--max-wait", "1s"}, stdin: `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"make -j8"}}`},
		{name: "hook claude-code empty", args: []string{"hook", "claude-code"}},

		{name: "setup", args: []string{"setup"}},
		{name: "setup -h", args: []string{"setup", "-h"}},
		{name: "setup claude-code", args: []string{"setup", "claude-code"}},
		{name: "setup codex", args: []string{"setup", "codex"}},
		// $HOME is the test's own, empty home.
		{name: "setup claude-code --write", args: []string{"setup", "claude-code", "--write"}},
		{name: "setup codex --write", args: []string{"setup", "codex", "--write"}},
		{name: "setup bogus", args: []string{"setup", "bogus"}},
		{name: "setup extra", args: []string{"setup", "codex", "x"}},

		{name: "completion", args: []string{"completion"}},
		{name: "completion -h", args: []string{"completion", "-h"}},
		{name: "completion bash", args: []string{"completion", "bash"}},
		{name: "completion zsh", args: []string{"completion", "zsh"}},
		{name: "completion fish", args: []string{"completion", "fish"}},
		{name: "completion tcsh", args: []string{"completion", "tcsh"}},
		{name: "completion extra", args: []string{"completion", "bash", "x"}},

		{name: "version", args: []string{"version"}},
		{name: "version --json", args: []string{"version", "--json"}},
		{name: "version -json down", args: []string{"version", "-json"}, down: true},
		{name: "version -h", args: []string{"version", "-h"}},
		{name: "version extra", args: []string{"version", "x"}},

		{name: "wait -h", args: []string{"wait", "-h"}},
		{name: "wait extra", args: []string{"wait", "x"}},
		{name: "version invalid config", args: []string{"-config", "$DIR/bad.json", "version"}},

		{name: "update -h", args: []string{"update", "-h"}},
		{name: "update -bogus", args: []string{"update", "-bogus"}},
		{name: "update extra", args: []string{"update", "x"}},
		{name: "update -version missing", args: []string{"update", "-version"}},
	}
}

// fakeDaemon serves canned answers on socket until the test ends.
func fakeDaemon(t *testing.T, socket string) {
	t.Helper()
	makeKey := domain.ProcessKey{PID: 4312, Start: goldenStart}
	gradleKey := domain.ProcessKey{PID: 4400, Start: goldenStart}
	status := app.StatusResult{
		Pressure: domain.PressureElevated,
		Reason:   "load 1.20 per CPU",
		Host: &domain.HostSample{
			CPUUsage: 0.42, Load1: 1.2, MemTotal: 16 << 30, MemAvailable: 6 << 30,
			Latency: 3 * time.Millisecond, SwapTotal: 2 << 30, SwapUsed: 512 << 20,
		},
		Agents: []process.AgentView{{Key: domain.ProcessKey{PID: 1000, Start: goldenStart}, Agent: "claude-1", CPU: 0.05, RSS: 300 << 20, Children: 2}},
		Heavy: []process.WorkloadView{
			{Key: makeKey, Name: "make", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 1000, CPU: 1.5, RSS: 2 << 30},
			{Key: gradleKey, Name: "gradle", Class: domain.ClassBuild, CPU: 0.25, RSS: 700 << 20},
		},
		Scheduler: app.SchedulerStatus{Mode: scheduler.ModeObserve, HeavyLimit: 2},
	}
	events := []domain.Event{
		{Seq: 1, Time: goldenStart.Add(time.Second), Kind: domain.EventProcessDiscovered, Attrs: map[string]string{"pid": "4312", "name": "make"}},
		{Seq: 2, Time: goldenStart.Add(2 * time.Second), Kind: domain.EventSchedulerDecision, Attrs: map[string]string{
			"decision": "admit", "pid": "4312", "reason": scheduler.ReasonSlotsExhausted,
			"pressure": "elevated", "running": "1", "limit": "2", "observed": "true",
		}},
	}
	verdict := app.Verdict{OKToStart: true, Reason: "capacity available", Pressure: "elevated", SchedulerMode: "observe", RunningHeavy: 2, HeavyLimit: 2, Suggestion: "start now"}
	control := func(_ context.Context, p json.RawMessage) (any, error) {
		var c app.ControlParams
		if err := json.Unmarshal(p, &c); err != nil {
			return nil, err
		}
		if c.PID == 13 {
			return nil, errors.New("pid 13 is not a heavy workload")
		}
		return nil, nil
	}
	srv := transport.NewServer(map[string]transport.Handler{
		"ping": func(context.Context, json.RawMessage) (any, error) {
			return app.PingResult{Version: "v9.9.9", Protocol: transport.ProtocolVersion, PID: 77, Uptime: "1h2m3s"}, nil
		},
		"status": func(context.Context, json.RawMessage) (any, error) { return status, nil },
		"stats": func(context.Context, json.RawMessage) (any, error) {
			row := stats.Row{Agent: "claude-1", Process: domain.ProcessKey{PID: 1000, Start: goldenStart}, CPUSeconds: 125.5, PeakRSS: 900 << 20, HeavyStarted: 3, QueuedSeconds: 12}
			return app.StatsResult{Since: goldenStart, Agents: []stats.Row{row}, Total: row, Unattributed: stats.Row{CPUSeconds: 4}}, nil
		},
		"events": func(_ context.Context, p json.RawMessage) (any, error) {
			var ep app.EventsParams
			if err := json.Unmarshal(p, &ep); err != nil {
				return nil, err
			}
			if ep.Limit < len(events) {
				return events[len(events)-ep.Limit:], nil
			}
			return events, nil
		},
		"pause":  control,
		"resume": control,
		"reload": func(context.Context, json.RawMessage) (any, error) {
			return app.ReloadResult{Changed: []string{"scheduler.mode off -> observe"}, RequiresRestart: []string{"socket_path"}}, nil
		},
		"learned": func(_ context.Context, raw json.RawMessage) (any, error) {
			var p app.LearnedParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			r := app.LearnedResult{Signatures: []app.LearnedSignature{
				{Signature: "python train.py", PeakRSS: 6 << 30, HeavyRuns: 4, LastHeavy: goldenStart},
				{Signature: "npx playwright", PeakRSS: 900 << 20, HeavyRuns: 1, Misses: 2, LastHeavy: goldenStart.Add(-time.Hour)},
			}}
			if p.Clear {
				r.Forgotten, r.Signatures = 2, []app.LearnedSignature{}
			} else if p.Forget == "python train.py" {
				r.Forgotten, r.Signatures = 1, r.Signatures[1:]
			}
			return r, nil
		},
		"acquire": func(context.Context, json.RawMessage) (any, error) {
			return app.AcquireResult{Granted: true, Reason: "granted", LeaseID: "1", Verdict: verdict}, nil
		},
	})
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, l) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the binary once per case")
	}
	// A short directory keeps the socket path under the platform limit.
	dir, err := os.MkdirTemp("/tmp", "throtgolden")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for _, d := range []string{"home", "xdg", "run/throt", "norun"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"bad.json": `{"scheduler": {"mode": "sometimes"}}`,
		// Neither file's output depends on the CPU count or $DIR.
		"cool.json":  `{"profile": "cool", "socket_path": "/nonexistent/throtd.sock"}`,
		"plain.json": `{"socket_path": "/nonexistent/throtd.sock", "event_history": 500, "scheduler": {"mode": "observe"}}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fakeDaemon(t, filepath.Join(dir, "run", "throt", "throtd.sock"))

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := goldenCases()
	results := make([]golden, len(cases))
	errs := make([]error, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(4, runtime.NumCPU()))
	for i, tc := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], errs[i] = runGolden(exe, dir, tc)
		}()
	}
	wg.Wait()
	got := map[string]golden{}
	for i, tc := range cases {
		if errs[i] != nil {
			t.Fatalf("%s: %v", tc.name, errs[i])
		}
		got[tc.name] = results[i]
	}

	if out := os.Getenv("THROT_GOLDEN_DUMP"); out != "" {
		if err := os.WriteFile(out, dumpGolden(got), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Skipf("wrote %s", out)
	}
	want := goldenWant()
	for _, tc := range goldenCases() {
		w, ok := want[tc.name]
		if !ok {
			t.Errorf("%s: no expectation", tc.name)
			continue
		}
		g := got[tc.name]
		if g.code != w.code {
			t.Errorf("%s: exit %d, want %d", tc.name, g.code, w.code)
		}
		if g.stdout != w.stdout {
			t.Errorf("%s: stdout\n%s\nwant\n%s", tc.name, g.stdout, w.stdout)
		}
		if g.stderr != w.stderr {
			t.Errorf("%s: stderr\n%s\nwant\n%s", tc.name, g.stderr, w.stderr)
		}
	}
	if len(want) != len(goldenCases()) {
		t.Errorf("%d expectations for %d cases", len(want), len(goldenCases()))
	}
}

// runGolden runs main with tc's arguments in a child process.
func runGolden(exe, dir string, tc goldenCase) (golden, error) {
	args := make([]string, len(tc.args))
	for i, a := range tc.args {
		args[i] = strings.ReplaceAll(a, "$DIR", dir)
	}
	argsJSON, _ := json.Marshal(args)
	runtimeDir := filepath.Join(dir, "run")
	if tc.down {
		runtimeDir = filepath.Join(dir, "norun")
	}
	// Under -cover each child writes coverage data; parallel children
	// sharing one directory race on its files.
	covDir, err := os.MkdirTemp(dir, "cover")
	if err != nil {
		return golden{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = []string{
		goldenArgsEnv + "=" + string(argsJSON),
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(dir, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg"),
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"TMPDIR=" + dir,
		"TZ=UTC",
		"GOCOVERDIR=" + covDir,
	}
	cmd.Stdin = strings.NewReader(tc.stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		return golden{}, err
	}
	return golden{code: code, stdout: normalize(stdout.String(), dir), stderr: normalize(stderr.String(), dir)}, nil
}

// normalize replaces what varies between hosts and runs.
func normalize(s, dir string) string {
	s = strings.ReplaceAll(s, "/private"+dir, "$DIR")
	s = strings.ReplaceAll(s, dir, "$DIR")
	s = strings.ReplaceAll(s, strconv.Quote(runtime.Version()), `"$GO"`)
	// printStats computes the age from the clock.
	return regexp.MustCompile(`\([0-9hms.]+ ago,`).ReplaceAllString(s, "(AGE ago,")
}

// dumpGolden renders got as the source of goldenWant.
func dumpGolden(got map[string]golden) []byte {
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	usage := got["help"].stdout
	var b bytes.Buffer
	fmt.Fprintf(&b, "// goldenUsage is the output of throt -h.\nconst goldenUsage = %s\n\n", goSource(usage))
	b.WriteString("func goldenWant() map[string]golden {\n\treturn map[string]golden{\n")
	for _, n := range names {
		g := got[n]
		fmt.Fprintf(&b, "\t\t%q: {code: %d, stdout: %s, stderr: %s},\n", n, g.code, withUsage(g.stdout, usage), withUsage(g.stderr, usage))
	}
	b.WriteString("\t}\n}\n")
	return b.Bytes()
}

// withUsage quotes s, with goldenUsage in place of each copy of usage.
func withUsage(s, usage string) string {
	if usage == "" || !strings.Contains(s, usage) {
		return goSource(s)
	}
	var parts []string
	for i, p := range strings.Split(s, usage) {
		if i > 0 {
			parts = append(parts, "goldenUsage")
		}
		if p != "" {
			parts = append(parts, goSource(p))
		}
	}
	return strings.Join(parts, " + ")
}

// goSource quotes s, as a raw string when that is possible and readable.
func goSource(s string) string {
	if strings.Contains(s, "\n") && !strings.Contains(s, "`") && !strings.Contains(s, "\r") {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

// goldenUsage is the output of throt -h.
const goldenUsage = `Usage: throt [flags] [command]

Commands:
  top        open the terminal UI (the default with no command)
  status     show host resources and pressure; --json for JSON; --idle
             lists agent sessions by memory and which look idle (report only)
  stats      show per-agent resource usage since throtd started; --json for JSON
  learned [--forget SIGNATURE | --clear]
             list the commands throtd learned heavy, which the hook then
             waits for like heavy tools; --forget or --clear drops them
  ping       check that throtd is reachable
  events     print recent daemon events as JSON lines; -f keeps printing new ones
  explain PID
             explain why a heavy workload is running, paused, or queued
  pause PID  pause a heavy workload (SIGSTOP) shown in 'status'
  resume [--force] PID
             resume a paused workload (SIGCONT). Also resumes a stopped
             process throtd did not pause (listed by 'doctor', e.g. one an
             earlier throtd left paused) when it is a heavy workload of an
             agent; --force resumes any other such process of yours too,
             such as a shell job suspended with Ctrl-Z (prefer fg there)
  run [--timeout D] [--no-wait] -- CMD [ARGS...]
             wait until throtd grants a heavy slot, reserved for CMD, then
             exec CMD (runs immediately if throtd is down)
  wait [--timeout D]
             wait until a heavy slot is held for the calling agent (its place
             in line from 'hook claude-code --defer'), at most D (default 30m)
  reload     make throtd re-read its config file
  doctor     print a sanitized diagnostic report (safe to share)
  mcp        run a read-only MCP server on stdio for coding agents
  config check|show
             'check' validates the config file as throtd would, lists what
             it changes from the defaults, and warns about risky settings;
             'show [--json]' prints the effective config and the source
             (default, profile, or file) of each value. Both accept --config PATH.
  hook claude-code [--max-wait D] [--defer]
             Claude Code PreToolUse hook: before a lone heavy Bash command,
             wait up to D (default 50s) for heavy-job capacity (advisory;
             fails open); --defer instead holds the agent's place in line
             and denies the call, asking Claude to run 'throt wait'
  setup claude-code|codex [--write]
             print the configuration to paste into a coding agent;
             --write installs it in your home directory instead (adds only,
             keeps a .throt-backup of each changed file)
  completion bash|zsh|fish
             print a shell completion script
  version [--json]
             print the client version; --json adds the protocol and Go
             versions, and throtd's version when it is reachable
  update [--check] [--version vX.Y.Z] [--pre] [--yes]
             install the newest GitHub release over throt and throtd after
             verifying its SHA-256; --check only reports (exit 0 when up to
             date, 10 when an update is available); the only command that
             uses the network

Flags:
  -config string
    	path to config file (default "$DIR/xdg/throt/config.json")
  -n int
    	number of events for 'events' (default 50)
`

func goldenWant() map[string]golden {
	return map[string]golden{
		"bad global flag": {code: 2, stdout: "", stderr: `throt: flag provided but not defined: -bogus; see 'throt -h'
`},
		"completion": {code: 2, stdout: "", stderr: `throt: usage: throt completion bash|zsh|fish
`},
		"completion -h": {code: 0, stdout: `usage: throt completion bash|zsh|fish

print a shell completion script
`, stderr: ""},
		"completion bash": {code: 0, stdout: `# bash completion for throt. Load with: source <(throt completion bash)
_throt_pids() {
    throt status 2>/dev/null | awk '/^HEAVY WORKLOADS/ {f = 1; next} f && $2 ~ /^[0-9]+$/ {print $2}'
}

_throt() {
    local cur prev i cmd=""
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    for ((i = 1; i < COMP_CWORD; i++)); do
        case "${COMP_WORDS[i]}" in
            -config|--config|-n|--n) ((i++)) ;;
            -*) ;;
            *) cmd="${COMP_WORDS[i]}"; break ;;
        esac
    done
    if [[ -z "$cmd" ]]; then
        case "$prev" in
            -config|--config) COMPREPLY=($(compgen -f -- "$cur")); return ;;
            -n|--n) return ;;
        esac
        COMPREPLY=($(compgen -W "top status stats learned ping events explain pause resume run wait reload config doctor mcp hook setup completion version update -config -n" -- "$cur"))
        return
    fi
    local pos=$((COMP_CWORD - i))
    case "$cmd" in
        events) COMPREPLY=($(compgen -W "-f -n" -- "$cur")) ;;
        pause|resume|explain) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "$(_throt_pids)" -- "$cur")) ;;
        completion) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur")) ;;
        config) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "check show" -- "$cur")) || COMPREPLY=($(compgen -W "--config --json" -- "$cur")) ;;
        status) COMPREPLY=($(compgen -W "--json --idle" -- "$cur")) ;;
        version) COMPREPLY=($(compgen -W "--json" -- "$cur")) ;;
        learned) [[ "$prev" == --forget || "$prev" == -forget ]] || COMPREPLY=($(compgen -W "--forget --clear" -- "$cur")) ;;
        update) [[ "$prev" == --version || "$prev" == -version ]] || COMPREPLY=($(compgen -W "--check --version --pre --yes" -- "$cur")) ;;
        hook) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "claude-code" -- "$cur")) ;;
        setup) [[ $pos -eq 1 ]] && COMPREPLY=($(compgen -W "claude-code codex" -- "$cur")) || COMPREPLY=($(compgen -W "--write" -- "$cur")) ;;
        run)
            local j
            for ((j = i + 1; j < COMP_CWORD; j++)); do
                case "${COMP_WORDS[j]}" in
                    --timeout|-timeout) ((j++)) ;;
                    -*) [[ "${COMP_WORDS[j]}" == "--" ]] && { ((j++)); break; } ;;
                    *) break ;;
                esac
            done
            if [[ $j -ge $COMP_CWORD ]]; then
                case "$prev" in --timeout|-timeout) return ;; esac
                if [[ "$cur" == -* ]]; then
                    COMPREPLY=($(compgen -W "--timeout --no-wait --" -- "$cur"))
                else
                    COMPREPLY=($(compgen -c -- "$cur"))
                fi
            fi
            ;;
    esac
}

complete -o default -F _throt throt
`, stderr: ""},
		"completion extra": {code: 2, stdout: "", stderr: `throt: usage: throt completion bash|zsh|fish
`},
		"completion fish": {code: 0, stdout: `# fish completion for throt. Load with: throt completion fish | source
function __throt_pids
    throt status 2>/dev/null | awk '/^HEAVY WORKLOADS/ {f = 1; next} f && $2 ~ /^[0-9]+$/ {print $2}'
end

complete -c throt -f
complete -c throt -n __fish_use_subcommand -o config -r -F -d 'path to config file'
complete -c throt -n __fish_use_subcommand -o n -x -d 'number of events'
complete -c throt -n __fish_use_subcommand -a top -d 'open the terminal UI'
complete -c throt -n __fish_use_subcommand -a status -d 'show host resources and pressure'
complete -c throt -n __fish_use_subcommand -a ping -d 'check that throtd is reachable'
complete -c throt -n __fish_use_subcommand -a events -d 'print recent daemon events'
complete -c throt -n __fish_use_subcommand -a explain -d 'explain why a heavy workload is running, paused, or queued'
complete -c throt -n __fish_use_subcommand -a pause -d 'pause a heavy workload'
complete -c throt -n __fish_use_subcommand -a resume -d 'resume a paused workload'
complete -c throt -n __fish_use_subcommand -a run -d 'wait for heavy-job capacity, then run a command'
complete -c throt -n __fish_use_subcommand -a wait -d 'wait until a heavy slot is held for this agent'
complete -c throt -n __fish_use_subcommand -a stats -d 'per-agent resource usage since throtd started'
complete -c throt -n __fish_use_subcommand -a learned -d 'list or forget the commands throtd learned heavy'
complete -c throt -n __fish_use_subcommand -a reload -d 'make throtd re-read its config file'
complete -c throt -n __fish_use_subcommand -a config -d 'check or show the config file'
complete -c throt -n __fish_use_subcommand -a doctor -d 'print a sanitized diagnostic report'
complete -c throt -n __fish_use_subcommand -a mcp -d 'run a read-only MCP server on stdio'
complete -c throt -n __fish_use_subcommand -a hook -d 'Claude Code hook that waits for heavy-job capacity'
complete -c throt -n __fish_use_subcommand -a setup -d 'print the configuration to paste into a coding agent'
complete -c throt -n __fish_use_subcommand -a completion -d 'print a shell completion script'
complete -c throt -n __fish_use_subcommand -a version -d 'print the client version'
complete -c throt -n __fish_use_subcommand -a update -d 'install the newest verified release in place'
complete -c throt -n '__fish_seen_subcommand_from events' -o f -d 'keep printing new events'
complete -c throt -n '__fish_seen_subcommand_from events' -o n -x -d 'number of recent events'
complete -c throt -n '__fish_seen_subcommand_from pause resume explain' -x -a '(__throt_pids)'
complete -c throt -n '__fish_seen_subcommand_from completion' -x -a 'bash zsh fish'
complete -c throt -n '__fish_seen_subcommand_from config; and not __fish_seen_subcommand_from check show' -x -a 'check show'
complete -c throt -n '__fish_seen_subcommand_from config' -l config -r -F -d 'path to config file'
complete -c throt -n '__fish_seen_subcommand_from config status version' -l json -d 'print JSON'
complete -c throt -n '__fish_seen_subcommand_from status' -l idle -d 'list agent sessions and which look idle'
complete -c throt -n '__fish_seen_subcommand_from update' -l check -d 'only report whether an update is available'
complete -c throt -n '__fish_seen_subcommand_from update' -l version -x -d 'install this release'
complete -c throt -n '__fish_seen_subcommand_from update' -l pre -d 'include prereleases'
complete -c throt -n '__fish_seen_subcommand_from update' -l yes -d 'do not ask for confirmation'
complete -c throt -n '__fish_seen_subcommand_from learned' -l forget -x -d 'forget one learned command'
complete -c throt -n '__fish_seen_subcommand_from learned' -l clear -d 'forget every learned command'
complete -c throt -n '__fish_seen_subcommand_from hook' -x -a 'claude-code'
complete -c throt -n '__fish_seen_subcommand_from setup' -x -a 'claude-code codex'
complete -c throt -n '__fish_seen_subcommand_from setup' -l write -d 'install the configuration in your home directory'
complete -c throt -n '__fish_seen_subcommand_from run' -l timeout -x -d 'run anyway after this long'
complete -c throt -n '__fish_seen_subcommand_from run' -l no-wait -d 'check once and run regardless'
complete -c throt -n '__fish_seen_subcommand_from run' -a '(__fish_complete_command)'
`, stderr: ""},
		"completion tcsh": {code: 2, stdout: "", stderr: `throt: usage: throt completion bash|zsh|fish
`},
		"completion zsh": {code: 0, stdout: `#compdef throt
# zsh completion for throt. Load with: source <(throt completion zsh)

_throt_pids() {
    local -a pids
    pids=(${(f)"$(throt status 2>/dev/null | awk '/^HEAVY WORKLOADS/ {f = 1; next} f && $2 ~ /^[0-9]+$/ {print $2}')"})
    compadd -a pids
}

_throt() {
    local -a commands
    commands=(
        'top:open the terminal UI'
        'status:show host resources and pressure'
        'ping:check that throtd is reachable'
        'events:print recent daemon events'
        'explain:explain why a heavy workload is running, paused, or queued'
        'pause:pause a heavy workload'
        'resume:resume a paused workload'
        'run:wait for heavy-job capacity, then run a command'
        'wait:wait until a heavy slot is held for this agent'
        'stats:per-agent resource usage since throtd started'
        'learned:list or forget the commands throtd learned heavy'
        'reload:make throtd re-read its config file'
        'config:check or show the config file'
        'doctor:print a sanitized diagnostic report'
        'mcp:run a read-only MCP server on stdio'
        'hook:Claude Code hook that waits for heavy-job capacity'
        'setup:print the configuration to paste into a coding agent'
        'completion:print a shell completion script'
        'version:print the client version'
        'update:install the newest verified release in place'
    )
    local curcontext="$curcontext" state line
    _arguments -C \
        '-config[path to config file]:file:_files' \
        '-n[number of events]:count:' \
        '1:command:->command' \
        '*::arg:->args'
    case $state in
        command) _describe -t commands 'throt command' commands ;;
        args)
            case $words[1] in
                events) _arguments '-f[keep printing new events]' '-n[number of recent events]:count:' ;;
                pause|resume|explain) (( CURRENT == 2 )) && _throt_pids ;;
                completion) (( CURRENT == 2 )) && compadd bash zsh fish ;;
                config) (( CURRENT == 2 )) && compadd check show || _arguments '--config[path to config file]:file:_files' '--json[print JSON]' ;;
                status) _arguments '(--idle)--json[print JSON]' '(--json)--idle[list agent sessions and which look idle]' ;;
                version) _arguments '--json[print JSON]' ;;
                learned) _arguments '--forget[forget one learned command]:signature:' '--clear[forget every learned command]' ;;
                update) _arguments '--check[only report whether an update is available]' '--version[install this release]:version:' '--pre[include prereleases]' '--yes[do not ask for confirmation]' ;;
                hook) (( CURRENT == 2 )) && compadd claude-code ;;
                setup) (( CURRENT == 2 )) && compadd claude-code codex || _arguments '--write[install the configuration in your home directory]' ;;
                run) _arguments -S '--timeout[run anyway after this long]:duration:' '--no-wait[check once and run regardless]' '*::command:_normal' ;;
            esac
            ;;
    esac
}

if [ "$funcstack[1]" = "_throt" ]; then
    _throt "$@"
else
    compdef _throt throt
fi
`, stderr: ""},
		"config": {code: 2, stdout: "", stderr: `throt: usage: throt config check|show [--config PATH] [--json]
`},
		"config -h": {code: 0, stdout: `usage: throt config check|show [--config PATH] [--json]
`, stderr: ""},
		"config bogus": {code: 2, stdout: "", stderr: `throt: usage: throt config check|show [--config PATH] [--json]
`},
		"config check": {code: 0, stdout: `ok: $DIR/plain.json
  socket_path $DIR/run/throt/throtd.sock -> /nonexistent/throtd.sock (file)
  event_history 1000 -> 500 (file)
  scheduler.mode off -> observe (file)
`, stderr: ""},
		"config check --json": {code: 2, stdout: "", stderr: `throt: flag provided but not defined: -json; see 'throt config check -h'
`},
		"config check -h": {code: 0, stdout: `usage: throt config check|show [--config PATH] [--json]
  -config string
    	path to config file (default "$DIR/xdg/throt/config.json")
`, stderr: ""},
		"config check extra": {code: 2, stdout: "", stderr: `throt: usage: throt config check|show [--config PATH] [--json]
`},
		"config check global": {code: 0, stdout: `ok: $DIR/plain.json
  socket_path $DIR/run/throt/throtd.sock -> /nonexistent/throtd.sock (file)
  event_history 1000 -> 500 (file)
  scheduler.mode off -> observe (file)
`, stderr: ""},
		"config check invalid": {code: 1, stdout: "", stderr: `throt: config $DIR/bad.json: scheduler.mode must be off, observe, or enforce, not "sometimes"
`},
		"config check no file": {code: 0, stdout: `no config file at $DIR/xdg/throt/config.json; defaults apply
same as the defaults
`, stderr: ""},
		"config show": {code: 0, stdout: `file:    $DIR/cool.json
profile: cool
KEY                          VALUE                     SOURCE
socket_path                  /nonexistent/throtd.sock  file
sample_interval              2s                        default
event_history                1000                      default
profile                      cool                      file
scheduler.mode               off                       default
scheduler.heavy_limit        1                         profile
scheduler.start_limit        1                         profile
scheduler.resume_cooldown    10s                       default
scheduler.pause_on_critical  true                      profile
scheduler.max_queue_wait     5m0s                      default
scheduler.adaptive           true                      profile
scheduler.per_agent_limit    0                         default
scheduler.background         true                      default
agents                       []                        default
`, stderr: ""},
		"config show --json": {code: 0, stdout: `{
  "path": "$DIR/cool.json",
  "file": true,
  "profile": "cool",
  "config": {
    "socket_path": "/nonexistent/throtd.sock",
    "sample_interval": "2s",
    "event_history": 1000,
    "profile": "cool",
    "scheduler": {
      "mode": "off",
      "heavy_limit": 1,
      "start_limit": 1,
      "resume_cooldown": "10s",
      "pause_on_critical": true,
      "max_queue_wait": "5m0s",
      "adaptive": true,
      "per_agent_limit": 0,
      "background": true
    },
    "agents": null
  },
  "settings": [
    {
      "key": "socket_path",
      "value": "/nonexistent/throtd.sock",
      "source": "file"
    },
    {
      "key": "sample_interval",
      "value": "2s",
      "source": "default"
    },
    {
      "key": "event_history",
      "value": "1000",
      "source": "default"
    },
    {
      "key": "profile",
      "value": "cool",
      "source": "file"
    },
    {
      "key": "scheduler.mode",
      "value": "off",
      "source": "default"
    },
    {
      "key": "scheduler.heavy_limit",
      "value": "1",
      "source": "profile"
    },
    {
      "key": "scheduler.start_limit",
      "value": "1",
      "source": "profile"
    },
    {
      "key": "scheduler.resume_cooldown",
      "value": "10s",
      "source": "default"
    },
    {
      "key": "scheduler.pause_on_critical",
      "value": "true",
      "source": "profile"
    },
    {
      "key": "scheduler.max_queue_wait",
      "value": "5m0s",
      "source": "default"
    },
    {
      "key": "scheduler.adaptive",
      "value": "true",
      "source": "profile"
    },
    {
      "key": "scheduler.per_agent_limit",
      "value": "0",
      "source": "default"
    },
    {
      "key": "scheduler.background",
      "value": "true",
      "source": "default"
    },
    {
      "key": "agents",
      "value": "[]",
      "source": "default"
    }
  ]
}
`, stderr: ""},
		"doctor -h": {code: 0, stdout: `usage: throt doctor

print a sanitized diagnostic report (safe to share)
`, stderr: ""},
		"doctor too many": {code: 2, stdout: "", stderr: `throt: unexpected argument "a"; see 'throt doctor -h'
`},
		"events": {code: 0, stdout: `{"seq":1,"time":"2026-10-28T14:00:01Z","kind":"process_discovered","attrs":{"name":"make","pid":"4312"}}
{"seq":2,"time":"2026-10-28T14:00:02Z","kind":"scheduler_decision","attrs":{"decision":"admit","limit":"2","observed":"true","pid":"4312","pressure":"elevated","reason":"heavy_slots_exhausted","running":"1"}}
`, stderr: ""},
		"events -bogus": {code: 2, stdout: "", stderr: `throt: flag provided but not defined: -bogus; see 'throt events -h'
`},
		"events -h": {code: 0, stdout: `usage: throt events [-f] [-n N]
  -f	keep printing new events until interrupted
  -n int
    	number of recent events to print first (default 50)
`, stderr: ""},
		"events -n": {code: 0, stdout: `{"seq":2,"time":"2026-10-28T14:00:02Z","kind":"scheduler_decision","attrs":{"decision":"admit","limit":"2","observed":"true","pid":"4312","pressure":"elevated","reason":"heavy_slots_exhausted","running":"1"}}
`, stderr: ""},
		"events down": {code: 1, stdout: "", stderr: "throt: throtd is not running (no socket at $DIR/norun/throt/throtd.sock); start it with `throtd`\n"},
		"events extra": {code: 2, stdout: "", stderr: `throt: usage: throt events [-f] [-n N]
`},
		"events invalid config": {code: 0, stdout: `{"seq":2,"time":"2026-10-28T14:00:02Z","kind":"scheduler_decision","attrs":{"decision":"admit","limit":"2","observed":"true","pid":"4312","pressure":"elevated","reason":"heavy_slots_exhausted","running":"1"}}
`, stderr: `throt: config $DIR/bad.json is invalid: scheduler.mode must be off, observe, or enforce, not "sometimes"; using the default socket
`},
		"explain": {code: 0, stdout: `make (pid 4312, build)
state      running
owner      claude-1 (pid 1000)
scheduler  observe, limit 2, 2/2 heavy running
pressure   elevated

RECENT EVENTS
TIME      KIND                ACTION  REASON                 PRESSURE  SLOTS
14:00:01  process_discovered  -       -                      -         -
14:00:02  scheduler_decision  admit   heavy_slots_exhausted  elevated  1/2

Running; at 14:00:02 the scheduler would have admit it (heavy slots exhausted), but observe mode never acts.
`, stderr: ""},
		"explain -h": {code: 0, stdout: `usage: throt explain PID

explain why a heavy workload is running, paused, or queued
`, stderr: ""},
		"explain missing": {code: 2, stdout: "", stderr: `throt: usage: throt explain PID
`},
		"explain not a pid": {code: 2, stdout: "", stderr: `throt: usage: throt explain PID
`},
		"explain not heavy": {code: 1, stdout: "", stderr: `throt: pid 99 is not a heavy workload in the latest sample; heavy workloads: 4312 (make), 4400 (gradle)
`},
		"explain too many": {code: 2, stdout: "", stderr: `throt: usage: throt explain PID
`},
		"global -n events": {code: 0, stdout: `{"seq":2,"time":"2026-10-28T14:00:02Z","kind":"scheduler_decision","attrs":{"decision":"admit","limit":"2","observed":"true","pid":"4312","pressure":"elevated","reason":"heavy_slots_exhausted","running":"1"}}
`, stderr: ""},
		"global -n events -n": {code: 0, stdout: `{"seq":1,"time":"2026-10-28T14:00:01Z","kind":"process_discovered","attrs":{"name":"make","pid":"4312"}}
{"seq":2,"time":"2026-10-28T14:00:02Z","kind":"scheduler_decision","attrs":{"decision":"admit","limit":"2","observed":"true","pid":"4312","pressure":"elevated","reason":"heavy_slots_exhausted","running":"1"}}
`, stderr: ""},
		"help":      {code: 0, stdout: goldenUsage, stderr: ""},
		"help long": {code: 0, stdout: goldenUsage, stderr: ""},
		"hook": {code: 1, stdout: "", stderr: `throt: usage: throt hook claude-code [--max-wait D] [--defer]
`},
		"hook -h": {code: 0, stdout: `usage: throt hook claude-code [--max-wait D] [--defer]

Claude Code PreToolUse hook: before a lone heavy Bash command,
wait up to D (default 50s) for heavy-job capacity (advisory;
fails open); --defer instead holds the agent's place in line
and denies the call, asking Claude to run 'throt wait'
`, stderr: ""},
		"hook claude-code -h": {code: 0, stdout: `Usage: throt hook claude-code [--max-wait D] [--defer]

Claude Code PreToolUse hook for the Bash tool. When the command is a heavy
tool (alone, or after "cd DIR &&", variable assignments, or "time"), or a
command throtd learned heavy because it started a heavy workload before,
wait up to D for throtd to report capacity for a heavy job, then let it run
unchanged. Only the command's signature (program and first argument base
names) is sent to throtd, to learn from. Advisory: no slot is reserved.
Without --defer it never blocks, denies, or changes a tool call. It returns at once on
any error or when throtd is unreachable. Keep D below the hook's "timeout" in settings.json. With
THROT_HOOK_DEBUG=1 it says on stderr why it ignored an event.

With --defer it does not wait when no slot is free: throtd holds the
agent's place in line and the hook denies the call, telling Claude to run
'throt wait' in the background and rerun the command when it returns; the
rerun takes the slot held for it. On any error, or with an older throtd,
it waits as above.

Flags:
  -defer
    	when no slot is free, hold the agent's place in line and deny the call instead of waiting
  -max-wait duration
    	wait at most this long for capacity (keep it below the hook timeout) (default 50s)
`, stderr: ""},
		"hook claude-code bad flag": {code: 0, stdout: "", stderr: `throt hook: bad arguments; not waiting
`},
		"hook claude-code empty": {code: 0, stdout: "", stderr: ""},
		"hook claude-code extra": {code: 0, stdout: "", stderr: `throt hook: bad arguments; not waiting
`},
		"hook claude-code heavy": {code: 0, stdout: `{"systemMessage":"throt: no heavy-job slot after 1s (all heavy slots are in use (2 running, limit 2)); running anyway"}
`, stderr: ""},
		"hook claude-code light": {code: 0, stdout: "", stderr: ""},
		"hook codex": {code: 1, stdout: "", stderr: `throt: usage: throt hook claude-code [--max-wait D] [--defer]
`},
		"hook invalid config": {code: 0, stdout: `{"systemMessage":"throt: no heavy-job slot after 1s (all heavy slots are in use (2 running, limit 2)); running anyway"}
`, stderr: `throt: config $DIR/bad.json is invalid: scheduler.mode must be off, observe, or enforce, not "sometimes"; using the default socket
`},
		"invalid config": {code: 0, stdout: `PRESSURE  ELEVATED (load 1.20 per CPU)
CPU       42%
LOAD      1.20 per CPU
MEM       10.0 GiB used of 16.0 GiB
LATENCY   3ms
SWAP      512 MiB used of 2.0 GiB
SCHEDULER observe, limit 2, 0 queued, 0 reserved

AGENTS
AGENT     PID   CPU  MEM      CHILDREN
claude-1  1000  5%   300 MiB  2

HEAVY WORKLOADS
OWNER     PID   PROCESS  CLASS  CPU   MEM      STATE
claude-1  4312  make     build  150%  2.0 GiB  running
-         4400  gradle   build  25%   700 MiB  running
`, stderr: `throt: config $DIR/bad.json is invalid: scheduler.mode must be off, observe, or enforce, not "sometimes"; using the default socket
`},
		"learned": {code: 0, stdout: `SIGNATURE        PEAK MEM  HEAVY RUNS  LIGHT SINCE  LAST HEAVY
python train.py  6.0 GiB   4           0            2026-10-28 14:00
npx playwright   900 MiB   1           2            2026-10-28 13:00
`, stderr: ""},
		"learned --clear": {code: 0, stdout: `forgot 2 learned command(s)
`, stderr: ""},
		"learned --forget": {code: 0, stdout: `forgot 1 learned command(s)
`, stderr: ""},
		"learned -h": {code: 0, stdout: `usage: throt learned [--forget SIGNATURE | --clear]
  -clear
    	forget every learned command
  -forget SIGNATURE
    	forget one learned SIGNATURE, as listed
`, stderr: ""},
		"learned both": {code: 2, stdout: "", stderr: `throt: usage: throt learned [--forget SIGNATURE | --clear]
`},
		"learned down": {code: 1, stdout: "", stderr: "throt: throtd is not running (no socket at $DIR/norun/throt/throtd.sock); start it with `throtd`\n"},
		"learned extra": {code: 2, stdout: "", stderr: `throt: usage: throt learned [--forget SIGNATURE | --clear]
`},
		"mcp": {code: 0, stdout: "", stderr: ""},
		"mcp --help": {code: 0, stdout: `usage: throt mcp

run a read-only MCP server on stdio for coding agents
`, stderr: ""},
		"mcp -h": {code: 0, stdout: `usage: throt mcp

run a read-only MCP server on stdio for coding agents
`, stderr: ""},
		"mcp initialize": {code: 0, stdout: `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"instructions":"Throt reports local machine pressure and heavy-workload capacity. Call throt_capacity before starting a heavy build, test suite, or container build; if ok_to_start is false, call throt_wait_for_capacity to wait for a slot, or run a lighter task. Call throt_explain with a PID from throt_status to learn why a heavy job is paused or queued. All tools are read-only and advisory.","protocolVersion":"2025-06-18","serverInfo":{"name":"throt","version":"dev"}}}
`, stderr: ""},
		"mcp too many": {code: 2, stdout: "", stderr: `throt: unexpected argument "a"; see 'throt mcp -h'
`},
		"no command no terminal": {code: 1, stdout: "", stderr: `throt top needs a terminal; try 'throt status'
`},
		"pause": {code: 0, stdout: `paused pid 4312
`, stderr: ""},
		"pause --force": {code: 2, stdout: "", stderr: `throt: usage: throt pause PID
`},
		"pause -h": {code: 0, stdout: `usage: throt pause PID

pause a heavy workload (SIGSTOP) shown in 'status'
`, stderr: ""},
		"pause missing": {code: 2, stdout: "", stderr: `throt: usage: throt pause PID
`},
		"pause refused": {code: 1, stdout: "", stderr: `throt: pid 13 is not a heavy workload
`},
		"ping": {code: 0, stdout: `throtd v9.9.9 (protocol v1, pid 77, up 1h2m3s)
`, stderr: ""},
		"ping -h": {code: 0, stdout: `usage: throt ping

check that throtd is reachable
`, stderr: ""},
		"ping down": {code: 1, stdout: "", stderr: "throt: throtd is not running (no socket at $DIR/norun/throt/throtd.sock); start it with `throtd`\n"},
		"ping extra": {code: 2, stdout: "", stderr: `throt: unexpected argument "x"; see 'throt ping -h'
`},
		"ping invalid config": {code: 0, stdout: `throtd v9.9.9 (protocol v1, pid 77, up 1h2m3s)
`, stderr: `throt: config $DIR/bad.json is invalid: scheduler.mode must be off, observe, or enforce, not "sometimes"; using the default socket
`},
		"reload": {code: 0, stdout: `applied: scheduler.mode off -> observe
requires restart: socket_path
`, stderr: ""},
		"reload -h": {code: 0, stdout: `usage: throt reload

make throtd re-read its config file
`, stderr: ""},
		"reload invalid config": {code: 0, stdout: `applied: scheduler.mode off -> observe
requires restart: socket_path
`, stderr: ""},
		"resume": {code: 0, stdout: `resumed pid 4312
`, stderr: ""},
		"resume --force first": {code: 0, stdout: `resumed pid 4312
`, stderr: ""},
		"resume --force last": {code: 0, stdout: `resumed pid 4312
`, stderr: ""},
		"resume -force": {code: 0, stdout: `resumed pid 4312
`, stderr: ""},
		"resume -h": {code: 0, stdout: `usage: throt resume [--force] PID

resume a paused workload (SIGCONT). Also resumes a stopped
process throtd did not pause (listed by 'doctor', e.g. one an
earlier throtd left paused) when it is a heavy workload of an
agent; --force resumes any other such process of yours too,
such as a shell job suspended with Ctrl-Z (prefer fg there)
`, stderr: ""},
		"resume invalid config": {code: 0, stdout: `resumed pid 4312
`, stderr: `throt: config $DIR/bad.json is invalid: scheduler.mode must be off, observe, or enforce, not "sometimes"; using the default socket
`},
		"resume missing": {code: 2, stdout: "", stderr: `throt: usage: throt resume [--force] PID
`},
		"resume not a pid": {code: 2, stdout: "", stderr: `throt: usage: throt resume [--force] PID
`},
		"resume two pids": {code: 2, stdout: "", stderr: `throt: usage: throt resume [--force] PID
`},
		"run": {code: 3, stdout: `out
`, stderr: `err
`},
		"run --no-wait": {code: 0, stdout: "", stderr: `throt: ok to start a heavy job: yes (granted); running now (--no-wait)
`},
		"run -bogus": {code: 2, stdout: "", stderr: `throt: flag provided but not defined: -bogus; see 'throt run -h'
`},
		"run -h": {code: 0, stdout: `Usage: throt run [--timeout D] [--no-wait] [--] CMD [ARGS...]

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
  -no-wait
    	check capacity once, print the verdict, and run regardless
  -timeout duration
    	stop waiting after this long and run anyway (0 waits forever) (default 5m0s)
`, stderr: ""},
		"run down": {code: 0, stdout: `hi
`, stderr: "throt: throtd is not reachable (throtd is not running (no socket at $DIR/norun/throt/throtd.sock); start it with `throtd`); running without waiting\n"},
		"run invalid config": {code: 0, stdout: "", stderr: `throt: config $DIR/bad.json is invalid: scheduler.mode must be off, observe, or enforce, not "sometimes"; using the default socket
`},
		"run missing": {code: 2, stdout: "", stderr: `throt: missing command; see 'throt run -h'
`},
		"run negative timeout": {code: 2, stdout: "", stderr: `throt: --timeout must not be negative; see 'throt run -h'
`},
		"run no dashes": {code: 4, stdout: "", stderr: ""},
		"run not found": {code: 127, stdout: "", stderr: `throt: run: throt-no-such-command: command not found
`},
		"setup": {code: 2, stdout: "", stderr: `throt: usage: throt setup claude-code|codex [--write]
`},
		"setup -h": {code: 0, stdout: `usage: throt setup claude-code|codex [--write]

print the configuration to paste into a coding agent;
--write installs it in your home directory instead (adds only,
keeps a .throt-backup of each changed file)
`, stderr: ""},
		"setup bogus": {code: 2, stdout: "", stderr: `throt: usage: throt setup claude-code|codex [--write]
`},
		"setup claude-code": {code: 0, stdout: `Claude Code setup (nothing is written; paste these yourself, or run
"throt setup claude-code --write" to add the hook to ~/.claude/settings.json).

1. Hook: add this to ~/.claude/settings.json (all projects) or
   .claude/settings.json (one project). If the file already has "hooks",
   add the PreToolUse entry to them instead of replacing them.

{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "throt hook claude-code --max-wait 110s",
            "timeout": 120
          }
        ]
      }
    ]
  }
}

   Before a Bash call whose command is a lone heavy tool, such as "make" or
   "cd web && npm run build", the hook waits up to 110s for throtd to report
   capacity for a heavy job, then lets the command run. It never changes,
   approves, or denies a command, so your permission rules apply exactly as
   before. Commands with pipes, ";", "||", redirections or subshells, light
   commands, and input it does not understand pass at once, as does every
   command when throtd is down. Keep --max-wait below "timeout": a hook
   that times out blocks the call.

   This is advisory admission: the hook exits before the command starts,
   so no slot is reserved, and two commands let through at the same moment
   can both start. For a reserved slot, prefix the command with
   "throt run --". Do not add permission allow rules for "throt run": such
   a rule would allow every command run through it.

2. MCP server (pressure and capacity tools): run

   claude mcp add --scope user throt -- throt mcp

   or add this to .mcp.json in your project root:

{
  "mcpServers": {
    "throt": {
      "command": "throt",
      "args": ["mcp"]
    }
  }
}

Both need throt on PATH, and the MCP tools need throtd running.
`, stderr: ""},
		"setup claude-code --write": {code: 0, stdout: `Created $DIR/home/.claude/settings.json with a PreToolUse hook for Bash:
  throt hook claude-code --max-wait 110s (timeout 120s)
Restart running Claude Code sessions to use it. For the MCP server, run:
  claude mcp add --scope user throt -- throt mcp
`, stderr: ""},
		"setup codex": {code: 0, stdout: "Codex setup (nothing is written; paste these yourself, or run\n\"throt setup codex --write\" to add both to ~/.codex).\n\n1. MCP server (pressure and capacity tools): add this to ~/.codex/config.toml:\n\n[mcp_servers.throt]\ncommand = \"throt\"\nargs = [\"mcp\"]\n\n2. Add this paragraph to ~/.codex/AGENTS.md (all projects) or a project AGENTS.md:\n\nPrefix heavy commands (builds, full test suites, container builds) with `throt run --`, e.g. `throt run -- npm run build` or `cd web && throt run -- make`. It waits for a free heavy-job slot and then runs the command unchanged; if throtd is down it runs at once.\n\nBoth need throt on PATH, and the MCP tools need throtd running.\n", stderr: ""},
		"setup codex --write": {code: 0, stdout: `Created $DIR/home/.codex/config.toml with [mcp_servers.throt].
Created $DIR/home/.codex/AGENTS.md with the 'throt run --' paragraph.
Restart running Codex sessions to use it.
`, stderr: ""},
		"setup extra": {code: 2, stdout: "", stderr: `throt: usage: throt setup claude-code|codex [--write]
`},
		"stats": {code: 0, stdout: `SINCE 2026-10-28 14:00:00 (AGE ago, in memory; resets when throtd restarts)

AGENT         PID   CPU-TIME  PEAK MEM  HEAVY JOBS  TIME QUEUED  ACTIVE FOR
claude-1      1000  2m6s      900 MiB   3           12s          0s
all agents    -     2m6s      900 MiB   3           12s          0s
unattributed  -     4s        0 MiB     0           0s           0s
`, stderr: ""},
		"stats --json": {code: 0, stdout: `{
  "since": "2026-10-28T14:00:00Z",
  "agents": [
    {
      "agent": "claude-1",
      "process": {
        "pid": 1000,
        "start": "2026-10-28T14:00:00Z"
      },
      "cpu_seconds": 125.5,
      "peak_rss": 943718400,
      "heavy_started": 3,
      "queued_seconds": 12,
      "first_seen": "0001-01-01T00:00:00Z",
      "last_seen": "0001-01-01T00:00:00Z"
    }
  ],
  "total": {
    "agent": "claude-1",
    "process": {
      "pid": 1000,
      "start": "2026-10-28T14:00:00Z"
    },
    "cpu_seconds": 125.5,
    "peak_rss": 943718400,
    "heavy_started": 3,
    "queued_seconds": 12,
    "first_seen": "0001-01-01T00:00:00Z",
    "last_seen": "0001-01-01T00:00:00Z"
  },
  "unattributed": {
    "process": {
      "pid": 0,
      "start": "0001-01-01T00:00:00Z"
    },
    "cpu_seconds": 4,
    "peak_rss": 0,
    "heavy_started": 0,
    "queued_seconds": 0,
    "first_seen": "0001-01-01T00:00:00Z",
    "last_seen": "0001-01-01T00:00:00Z"
  }
}
`, stderr: ""},
		"stats -bogus": {code: 2, stdout: "", stderr: `throt: flag provided but not defined: -bogus; see 'throt stats -h'
`},
		"stats -h": {code: 0, stdout: `usage: throt stats [--json]
  -json
    	print the stats as JSON
`, stderr: ""},
		"stats -json": {code: 0, stdout: `{
  "since": "2026-10-28T14:00:00Z",
  "agents": [
    {
      "agent": "claude-1",
      "process": {
        "pid": 1000,
        "start": "2026-10-28T14:00:00Z"
      },
      "cpu_seconds": 125.5,
      "peak_rss": 943718400,
      "heavy_started": 3,
      "queued_seconds": 12,
      "first_seen": "0001-01-01T00:00:00Z",
      "last_seen": "0001-01-01T00:00:00Z"
    }
  ],
  "total": {
    "agent": "claude-1",
    "process": {
      "pid": 1000,
      "start": "2026-10-28T14:00:00Z"
    },
    "cpu_seconds": 125.5,
    "peak_rss": 943718400,
    "heavy_started": 3,
    "queued_seconds": 12,
    "first_seen": "0001-01-01T00:00:00Z",
    "last_seen": "0001-01-01T00:00:00Z"
  },
  "unattributed": {
    "process": {
      "pid": 0,
      "start": "0001-01-01T00:00:00Z"
    },
    "cpu_seconds": 4,
    "peak_rss": 0,
    "heavy_started": 0,
    "queued_seconds": 0,
    "first_seen": "0001-01-01T00:00:00Z",
    "last_seen": "0001-01-01T00:00:00Z"
  }
}
`, stderr: ""},
		"stats extra": {code: 2, stdout: "", stderr: `throt: usage: throt stats [--json]
`},
		"status": {code: 0, stdout: `PRESSURE  ELEVATED (load 1.20 per CPU)
CPU       42%
LOAD      1.20 per CPU
MEM       10.0 GiB used of 16.0 GiB
LATENCY   3ms
SWAP      512 MiB used of 2.0 GiB
SCHEDULER observe, limit 2, 0 queued, 0 reserved

AGENTS
AGENT     PID   CPU  MEM      CHILDREN
claude-1  1000  5%   300 MiB  2

HEAVY WORKLOADS
OWNER     PID   PROCESS  CLASS  CPU   MEM      STATE
claude-1  4312  make     build  150%  2.0 GiB  running
-         4400  gradle   build  25%   700 MiB  running
`, stderr: ""},
		"status --json": {code: 0, stdout: `{
  "pressure": "elevated",
  "reason": "load 1.20 per CPU",
  "host": {
    "time": "0001-01-01T00:00:00Z",
    "cpu_usage": 0.42,
    "load1": 1.2,
    "mem_total": 17179869184,
    "mem_available": 6442450944,
    "swap_total": 2147483648,
    "swap_used": 536870912,
    "latency": 3000000
  },
  "agents": [
    {
      "process": {
        "pid": 1000,
        "start": "2026-10-28T14:00:00Z"
      },
      "agent": "claude-1",
      "name": "",
      "children": 2,
      "cpu": 0.05,
      "rss": 314572800
    }
  ],
  "heavy": [
    {
      "process": {
        "pid": 4312,
        "start": "2026-10-28T14:00:00Z"
      },
      "name": "make",
      "class": "build",
      "owner": "claude-1",
      "owner_pid": 1000,
      "cpu": 1.5,
      "rss": 2147483648
    },
    {
      "process": {
        "pid": 4400,
        "start": "2026-10-28T14:00:00Z"
      },
      "name": "gradle",
      "class": "build",
      "owner": "",
      "owner_pid": 0,
      "cpu": 0.25,
      "rss": 734003200
    }
  ],
  "paused": null,
  "scheduler": {
    "mode": "observe",
    "heavy_limit": 2,
    "per_agent_limit": 0,
    "queued": null,
    "would_queue": null,
    "leases": null
  }
}
`, stderr: ""},
		"status -h": {code: 0, stdout: `usage: throt status

show host resources and pressure; --json for JSON; --idle
lists agent sessions by memory and which look idle (report only)
`, stderr: ""},
		"status down":                {code: 1, stdout: "", stderr: "throt: throtd is not running (no socket at $DIR/norun/throt/throtd.sock); start it with `throtd`\n"},
		"status down invalid config": {code: 1, stdout: "", stderr: "throt: config $DIR/bad.json is invalid: scheduler.mode must be off, observe, or enforce, not \"sometimes\"; using the default socket\nthrot: throtd is not running (no socket at $DIR/norun/throt/throtd.sock); start it with `throtd`\n"},
		"too many arguments": {code: 2, stdout: "", stderr: `throt: unexpected argument "a"; see 'throt ping -h'
`},
		"top -h": {code: 0, stdout: `usage: throt top

open the terminal UI (the default with no command)
`, stderr: ""},
		"top no terminal": {code: 1, stdout: "", stderr: `throt top needs a terminal; try 'throt status'
`},
		"top too many": {code: 2, stdout: "", stderr: `throt: unexpected argument "a"; see 'throt top -h'
`},
		"unknown command": {code: 2, stdout: "", stderr: `throt: unknown command "frob"; see 'throt -h'
`},
		"unknown command with args": {code: 2, stdout: "", stderr: `throt: unknown command "frob"; see 'throt -h'
`},
		"update -bogus": {code: 2, stdout: "", stderr: `throt: flag provided but not defined: -bogus; see 'throt update -h'
`},
		"update -h": {code: 0, stdout: `Usage: throt update [--check] [--version vX.Y.Z] [--pre] [--yes]

Download the newest Throt release from GitHub, verify its SHA-256 against
the release's checksums.txt, and replace throt and the throtd next to it
in place. This is the only command that uses the network.

A private repository needs a token: GH_TOKEN or GITHUB_TOKEN, else the
output of 'gh auth token' when the GitHub CLI is installed.

Exit status with --check: 0 when up to date, 10 when an update is
available, 1 on error. A development build or one installed with go install
is updated with go install instead; --check still reports the latest
release for it (exit 0 for a development build, which it cannot compare).

Flags:
  -check
    	only report the current and latest versions (exit 10 when an update is available)
  -pre
    	include prereleases (the default when the running version is one)
  -version string
    	install this release, newer or older (a rollback)
  -yes
    	do not ask for confirmation
`, stderr: ""},
		"update -version missing": {code: 2, stdout: "", stderr: `throt: flag needs an argument: -version; see 'throt update -h'
`},
		"update extra": {code: 2, stdout: "", stderr: `throt: unexpected argument "x"; see 'throt update -h'
`},
		"version": {code: 0, stdout: `dev
`, stderr: ""},
		"version --json": {code: 0, stdout: `{
  "version": "dev",
  "protocol": 1,
  "go": "$GO",
  "daemon": {
    "version": "v9.9.9",
    "protocol": 1,
    "pid": 77,
    "uptime": "1h2m3s"
  }
}
`, stderr: ""},
		"version -h": {code: 0, stdout: `usage: throt version [--json]

print the client version; --json adds the protocol and Go
versions, and throtd's version when it is reachable
`, stderr: ""},
		"version -json down": {code: 0, stdout: "{\n  \"version\": \"dev\",\n  \"protocol\": 1,\n  \"go\": \"$GO\",\n  \"daemon\": null,\n  \"daemon_error\": \"throtd is not running (no socket at $DIR/norun/throt/throtd.sock); start it with `throtd`\"\n}\n", stderr: ""},
		"version extra": {code: 2, stdout: "", stderr: `throt: usage: throt version [--json]
`},
		"wait -h": {code: 0, stdout: `Usage: throt wait [--timeout D]

Wait until throtd holds a heavy slot for the agent this runs under, then
print "slot ready" and exit 0. It keeps the agent's place in line (taken
by 'throt hook claude-code --defer', or now if there is none), renewing it
once a second; the slot is then held for 60s for the agent's next heavy
command, which the hook admits at once. Exits 1 when D passes (0 waits
forever), when throtd is unreachable or too old to hold a place in line,
or when no agent runs above this process.

Flags:
  -timeout duration
    	give up after this long (0 waits forever) (default 30m0s)
`, stderr: ""},
		"wait extra": {code: 2, stdout: "", stderr: `throt: unexpected argument "x"; see 'throt wait -h'
`},
		"version invalid config": {code: 0, stdout: `dev
`, stderr: ""},
	}
}
