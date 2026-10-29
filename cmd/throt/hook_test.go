package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/transport"
)

func TestLightInvocationsDoNotWait(t *testing.T) {
	for _, in := range []string{
		"go version", "go env GOPATH", "go fmt ./...", "go list ./...", "go doc fmt",
		"npm --version", "npm view react", "npm config get prefix", "npm ls",
		"cargo --version", "cargo metadata --format-version 1", "cargo tree",
		"make -n", "make --dry-run all", "java -version", "dotnet --info", "gradle tasks",
		"mvn -v", "make --help", "pytest --version", "cd x && go version",
		"./gradlew --stop", "./gradlew --status", "./gradlew tasks", "gradlew projects", "./gradlew properties",
		"./gradlew help", "./gradlew --version", "./gradlew dependencies", "./gradlew dependencies --offline", "./gradlew tasks projects", "gradle --stop", "./mvnw -v", "./mvnw --version",
		"docker build --help",
	} {
		if name, ok := heavyCommand(in); ok {
			t.Errorf("heavyCommand(%q) = %q, want light", in, name)
		}
	}
	for _, in := range []string{"go build ./...", "go test ./...", "npm run build", "cargo build", "make", "make -j8", "gradle test", "pytest"} {
		if _, ok := heavyCommand(in); !ok {
			t.Errorf("heavyCommand(%q) is light, want heavy", in)
		}
	}
}

func TestHeavyCommand(t *testing.T) {
	tests := []struct {
		in   string
		want string // heavy tool name, or "" for not heavy
	}{
		// Heavy tools, alone.
		{"make", "make"},
		{"make -j8 all", "make"},
		{"go test ./...", "go"},
		{"npm run build", "npm"},
		{"cargo build --release", "cargo"},
		{"pytest -x tests/", "pytest"},
		{"gradle assemble", "gradle"},
		// Build wrappers and Docker image builds.
		{"./gradlew test", "gradlew"},
		{"gradlew build", "gradlew"},
		{"./gradlew", "gradlew"},
		{"cd api && ./gradlew test --tests 'FooTest'", "gradlew"},
		{"./gradlew -q tasks", "gradlew"},          // a flag first: not provably light
		{"./gradlew dependencies test", "gradlew"}, // a light task does not make the rest light
		{"gradle tasks build", "gradle"},
		{"./mvnw verify", "mvnw"},
		{"backend/mvnw -B test", "mvnw"},
		{"docker build -t app .", "docker"},
		{"docker buildx build --platform linux/amd64 .", "docker"},
		{"docker compose build", "docker"},
		{"docker compose build api", "docker"},
		{"  make  ", "make"},
		{"./node_modules/.bin/jest --ci", "jest"},
		{"/usr/bin/make", "make"},
		// cd prefix.
		{"cd x && make", "make"},
		{"cd web&&npm test", "npm"},
		{`cd "my dir" && make`, "make"},
		{"cd a && cd b && go build", "go"},
		{"cd $HOME/src && make", "make"},
		// Env assignments and time.
		{"FOO=1 make", "make"},
		{`CGO_ENABLED=0 GOOS="linux" go build`, "go"},
		{"time make", "make"},
		{"time FOO=1 cargo test", "cargo"},
		{"cd x && NODE_ENV=production npm run build", "npm"},
		// Quoted and escaped arguments.
		{`go test -run 'TestA|TestB' ./...`, "go"},
		{`make CFLAGS="-O2 -g"`, "make"},
		{`npm run "build; rm -rf /"`, "npm"},
		{`make a\ b`, "make"},
		{`go test -run "Test\"X"`, "go"},
		{`make "$TARGET"`, "make"},
		{"pytest -k 'a && b'", "pytest"},

		// Light commands.
		{"ls -la", ""},
		{"git status", ""},
		{"node script.js", ""},
		{"python -m pytest", ""},
		{"docker ps", ""},
		{"docker compose up -d", ""},
		{"docker compose up --build", ""},
		{"docker run postgres", ""},
		{"docker buildx ls", ""},
		{"docker", ""},
		{"docker --context x build .", ""},
		{"sh gradlew test", ""},
		{"gradlew.bat test", ""},
		{"cd x", ""},
		{"FOO=1", ""},
		{"time", ""},
		// throt run itself waits; the hook leaves it to that.
		{"throt run -- make", ""},
		{"cd x && throt run -- make", ""},
		// Only cd may precede.
		{"make && ./run", ""},
		{"cd x && make && ./run", ""},
		{"ls && make", ""},
		{"FOO=1 cd x && make", ""},
		{"cd && make", ""},
		{"cd a b && make", ""},
		// Unsupported syntax.
		{"make | tee log", ""},
		{"make || true", ""},
		{"make; echo done", ""},
		{"make &", ""},
		{"make > build.log", ""},
		{"make 2>&1", ""},
		{"make < input", ""},
		{"(cd x && make)", ""},
		{"{ make; }", ""},
		{"make $(nproc)", ""},
		{"make -j`nproc`", ""},
		{"cat <<EOF\nmake\nEOF", ""},
		{"make # build", ""},
		{"make\nls", ""},
		{"! make", ""},
		{`make "unterminated`, ""},
		// Command words that are not plain literals.
		{`"make"`, ""},
		{`m\ake`, ""},
		{"$MAKE all", ""},
		{"~/bin/make", ""},
		{"ma*e", ""},
		{"make/", ""},
		{"sudo make install", ""},
		{"env FOO=1 make", ""},
		{"command make", ""},
		{"exec make", ""},
		{"time -p make", ""},
		{"claude", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got, ok := heavyCommand(tt.in)
		if tt.want == "" {
			if ok {
				t.Errorf("heavyCommand(%q) = %q, want not heavy", tt.in, got)
			}
			continue
		}
		if !ok || got != tt.want {
			t.Errorf("heavyCommand(%q) = %q, %v; want %q", tt.in, got, ok, tt.want)
		}
	}
	if len(tests) < 40 {
		t.Fatalf("only %d cases", len(tests))
	}
}

func bashEvent(command string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s", "cwd": "/p", "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": command, "description": "d", "timeout": 120000},
	})
	return string(b)
}

// fakeHook drives hookWaiter with a fake clock. Each check returns the
// next verdict from oks; past the end it keeps returning the last one.
type fakeHook struct {
	t       *testing.T
	oks     []bool
	errs    []error
	calls   int
	built   int
	clock   time.Time
	started time.Time
	// learned are the signatures throtd learned heavy; expectErr fails
	// every report. reports records each report as "sig" or "sig (named)".
	learned   map[string]bool
	expectErr error
	reports   []string
}

func (f *fakeHook) waiter() hookWaiter {
	f.started = f.clock
	return hookWaiter{
		expect: func(_ context.Context, sig string, named bool) (bool, error) {
			r := sig
			if named {
				r += " (named)"
			}
			f.reports = append(f.reports, r)
			return f.learned[sig] && f.expectErr == nil, f.expectErr
		},
		newCheck: func() checkFunc {
			f.built++
			return func(context.Context) (bool, app.Verdict, error) {
				i := min(f.calls, max(len(f.oks), len(f.errs))-1)
				f.calls++
				var err error
				if i < len(f.errs) {
					err = f.errs[i]
				}
				ok := i < len(f.oks) && f.oks[i]
				return ok, app.Verdict{OKToStart: ok, Reason: "pressure high"}, err
			}
		},
		now: func() time.Time { return f.clock },
		sleep: func(_ context.Context, d time.Duration) error {
			if d <= 0 {
				f.t.Fatalf("sleep(%v)", d)
			}
			f.clock = f.clock.Add(d)
			return nil
		},
	}
}

func (f *fakeHook) run(in string, maxWait time.Duration) string {
	var out bytes.Buffer
	f.waiter().claudeCode(context.Background(), strings.NewReader(in), &out, maxWait)
	return out.String()
}

func systemMessage(t *testing.T, out string) string {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if len(r) != 1 {
		t.Errorf("output %q has fields other than systemMessage", out)
	}
	s, _ := r["systemMessage"].(string)
	return s
}

func TestHookWaitsUntilOK(t *testing.T) {
	f := &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{false, false, false, true}}
	out := f.run(bashEvent("cd web && npm run build"), 50*time.Second)
	if f.calls != 4 {
		t.Errorf("checks = %d, want 4", f.calls)
	}
	if got := f.clock.Sub(f.started); got != 3*time.Second {
		t.Errorf("waited %v, want 3s", got)
	}
	if msg := systemMessage(t, out); !strings.Contains(msg, "waited 3s") {
		t.Errorf("systemMessage = %q", msg)
	}
	if strings.Contains(out, "updatedInput") || strings.Contains(out, "permissionDecision") || strings.Contains(out, "npm") {
		t.Errorf("output %q must not change, decide, or echo the command", out)
	}
}

func TestHookOKAtOnceIsSilent(t *testing.T) {
	f := &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{true}}
	if out := f.run(bashEvent("make"), 50*time.Second); out != "" || f.calls != 1 {
		t.Errorf("output %q after %d checks, want none after 1", out, f.calls)
	}
}

func TestHookMaxWait(t *testing.T) {
	f := &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{false}}
	out := f.run(bashEvent("make"), 2500*time.Millisecond)
	if got := f.clock.Sub(f.started); got != 2500*time.Millisecond {
		t.Errorf("waited %v, want exactly the 2.5s max-wait", got)
	}
	if f.calls != 4 {
		t.Errorf("checks = %d, want 4 (0s, 1s, 2s, 2.5s)", f.calls)
	}
	if msg := systemMessage(t, out); !strings.Contains(msg, "running anyway") || !strings.Contains(msg, "pressure high") {
		t.Errorf("systemMessage = %q", msg)
	}
}

func TestHookMaxWaitZeroNeverAsks(t *testing.T) {
	f := &fakeHook{t: t, oks: []bool{false}}
	if out := f.run(bashEvent("make"), 0); out != "" || f.built != 0 {
		t.Errorf("output %q, check built %d times", out, f.built)
	}
}

func TestHookDaemonDown(t *testing.T) {
	f := &fakeHook{t: t, clock: time.Unix(1000, 0), errs: []error{errors.New("connection refused")}}
	if out := f.run(bashEvent("make"), 50*time.Second); out != "" || f.calls != 1 || !f.clock.Equal(f.started) {
		t.Errorf("output %q, %d checks, waited %v; want none, 1, 0", out, f.calls, f.clock.Sub(f.started))
	}
	// Losing throtd while waiting also lets the command go at once.
	f = &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{false, false}, errs: []error{nil, errors.New("gone")}}
	if out := f.run(bashEvent("make"), 50*time.Second); out != "" || f.calls != 2 {
		t.Errorf("lost daemon: output %q after %d checks", out, f.calls)
	}
}

func TestHookSkipsWithoutDaemon(t *testing.T) {
	cases := map[string]string{
		"light command":   bashEvent("ls -la"),
		"pipe":            bashEvent("make | tee log"),
		"heavy then more": bashEvent("make && ./run"),
		"throt run":       bashEvent("throt run -- make"),
		"other tool":      `{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"command":"make"}}`,
		"other event":     `{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"make"}}`,
		"no command":      `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`,
		"command number":  `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":42}}`,
		"no tool input":   `{"hook_event_name":"PreToolUse","tool_name":"Bash"}`,
		"tool input list": `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":["make"]}`,
		"malformed":       `{"hook_event_name":"PreToolUse",`,
		"not json":        "make",
		"empty":           "",
		"json array":      `[1,2]`,
		"too large":       `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"make","x":"` + strings.Repeat("a", maxHookInput) + `"}}`,
	}
	for name, in := range cases {
		f := &fakeHook{t: t, oks: []bool{false}}
		if out := f.run(in, 50*time.Second); out != "" || f.built != 0 || f.calls != 0 || len(f.reports) != 0 {
			t.Errorf("%s: output %q, check built %d, called %d, reports %q; want no daemon contact", name, out, f.built, f.calls, f.reports)
		}
	}
}

func TestHookLearnedCommands(t *testing.T) {
	tests := []struct {
		command string
		learned bool
		// reports are the reports to throtd; wait whether the hook waits.
		reports []string
		wait    bool
	}{
		{"python train.py --epochs 3", true, []string{"python train.py", "python train.py"}, true},
		{"cd ml && OMP_NUM_THREADS=8 python scripts/train.py", true, []string{"python train.py", "python train.py"}, true},
		{"python train.py", false, []string{"python train.py"}, false},
		{"npx playwright test", true, []string{"npx playwright", "npx playwright"}, true},
		{"pnpm test:e2e", false, []string{"pnpm test:e2e (named)", "pnpm test:e2e (named)"}, true},
		{"./scripts/build.sh", true, []string{"build.sh", "build.sh"}, true},
		// Heavy by name: reported as such, and waits whatever throtd says.
		{"make -j8", false, []string{"make (named)", "make (named)"}, true},
		{"./gradlew test", false, []string{"gradlew test (named)", "gradlew test (named)"}, true},
		// Light by name or by flag: never reported.
		{"git status", true, nil, false},
		{"python --version", true, nil, false},
		{"go version", true, nil, false},
		// Not one simple command: nothing to report.
		{"python train.py | tee log", true, nil, false},
		{`"$PY" train.py`, true, nil, false},
	}
	for _, tt := range tests {
		f := &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{false, true}}
		if tt.learned {
			f.learned = map[string]bool{}
			for _, r := range tt.reports {
				f.learned[r] = true
			}
			f.learned["git status"], f.learned["python"], f.learned["go"] = true, true, true
		}
		out := f.run(bashEvent(tt.command), 50*time.Second)
		if strings.Join(f.reports, ",") != strings.Join(tt.reports, ",") {
			t.Errorf("%q: reports %q, want %q", tt.command, f.reports, tt.reports)
		}
		if waited := f.calls > 0; waited != tt.wait {
			t.Errorf("%q: waited %v, want %v", tt.command, waited, tt.wait)
		}
		if tt.wait {
			msg := systemMessage(t, out)
			if !strings.Contains(msg, "waited 1s") || strings.Contains(msg, "learned") != (tt.learned && !strings.Contains(tt.reports[0], "named")) {
				t.Errorf("%q: systemMessage %q", tt.command, msg)
			}
			if strings.Contains(out, "train") || strings.Contains(out, "test:e2e") {
				t.Errorf("%q: output %q echoes the command", tt.command, out)
			}
		}
	}
}

func TestHookLearnedFailsOpen(t *testing.T) {
	// A daemon that is down or predates "expect": an unknown command runs
	// at once, and a heavy tool still waits as before.
	for _, err := range []error{errors.New("connection refused"), errors.New("unknown method: expect")} {
		f := &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{true}, learned: map[string]bool{"python x.py": true}, expectErr: err}
		if out := f.run(bashEvent("python x.py"), 50*time.Second); out != "" || f.built != 0 || len(f.reports) != 1 {
			t.Errorf("%v: unknown command: output %q, check built %d, reports %q", err, out, f.built, f.reports)
		}
		f = &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{false, true}, expectErr: err}
		if out := f.run(bashEvent("make"), 50*time.Second); f.calls != 2 || !strings.Contains(out, "waited") {
			t.Errorf("%v: heavy tool: output %q after %d checks", err, out, f.calls)
		}
	}
	// A learned command that finds capacity at once is reported once and
	// says nothing.
	f := &fakeHook{t: t, clock: time.Unix(1000, 0), oks: []bool{true}, learned: map[string]bool{"python x.py": true}}
	if out := f.run(bashEvent("python x.py"), 50*time.Second); out != "" || f.calls != 1 || len(f.reports) != 1 {
		t.Errorf("output %q, %d checks, reports %q", out, f.calls, f.reports)
	}
}

// TestExpectHeavyAgainstDaemon checks the hook's call against a fake
// throtd: the signature and this process's PID are sent, nothing else, and
// a throtd without the method is an error the hook ignores.
func TestExpectHeavyAgainstDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "throtexp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := dir + "/s.sock"
	var got []string
	srv := transport.NewServer(map[string]transport.Handler{
		"expect": func(_ context.Context, raw json.RawMessage) (any, error) {
			got = append(got, string(raw))
			return app.ExpectResult{Heavy: true}, nil
		},
	})
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, l) }()
	defer func() { cancel(); <-done }()

	heavy, err := expectHeavy(context.Background(), socket, "python train.py", false)
	if err != nil || !heavy {
		t.Fatalf("expectHeavy = %v, %v", heavy, err)
	}
	want := fmt.Sprintf(`{"pid":%d,"signature":"python train.py"}`, os.Getpid())
	if len(got) != 1 || got[0] != want {
		t.Errorf("sent %q, want %q", got, want)
	}
	if heavy, err := expectHeavy(context.Background(), dir+"/none.sock", "a", false); err == nil || heavy {
		t.Errorf("daemon down: %v, %v", heavy, err)
	}
}

// TestHookDebugHints checks that THROT_HOOK_DEBUG=1 explains on stderr why
// an event was ignored, that stdout stays empty, and that nothing is said
// without it.
func TestHookDebugHints(t *testing.T) {
	cases := map[string]string{
		`{"tool_name":"Bash","tool_input":{"command":"make"}}`:                                 "no hook_event_name",
		`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"make"}}`: `hook_event_name is "PostToolUse"`,
		`{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{}}`:                  `tool_name is "Edit", not Bash`,
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`:                  "no tool_input.command",
		`not json`: "not a JSON hook event",
	}
	for in, want := range cases {
		for _, debug := range []string{"1", ""} {
			t.Setenv(hookDebugEnv, debug)
			var out, errw bytes.Buffer
			if err := runHook([]string{"claude-code"}, "/nonexistent/config.json", strings.NewReader(in), &out, &errw); err != nil || out.Len() != 0 {
				t.Errorf("%s: err %v, stdout %q", in, err, out.String())
			}
			got := errw.String()
			if debug == "" && got != "" {
				t.Errorf("%s: hint without %s: %q", in, hookDebugEnv, got)
			}
			if debug == "1" && (!strings.HasPrefix(got, "throt hook: ") || !strings.Contains(got, want) || strings.Count(got, "\n") != 1) {
				t.Errorf("%s: stderr %q, want one line with %q", in, got, want)
			}
		}
	}
}

// TestHookStdinHeldOpen checks that a stdin that never ends cannot hold
// the hook past its deadline: it gives up, writing nothing.
func TestHookStdinHeldOpen(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	f := &fakeHook{t: t, oks: []bool{false}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var out, errw bytes.Buffer
	h := f.waiter()
	h.debug = &errw
	done := make(chan struct{})
	go func() { defer close(done); h.claudeCode(ctx, r, &out, time.Second) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hook still reading a held-open stdin")
	}
	if out.Len() != 0 || f.built != 0 {
		t.Errorf("output %q, check built %d; want neither", out.String(), f.built)
	}
	if !strings.Contains(errw.String(), "stdin did not end") {
		t.Errorf("hint %q", errw.String())
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken stdin") }

func TestRunHookFailsOpen(t *testing.T) {
	var out, errw bytes.Buffer
	// A light command returns before any config or daemon access, so a
	// missing config path is fine.
	if err := runHook([]string{"claude-code"}, "/nonexistent/config.json", strings.NewReader(bashEvent("ls")), &out, &errw); err != nil || out.Len() != 0 {
		t.Errorf("light command: err %v, output %q", err, out.String())
	}
	if err := runHook([]string{"claude-code"}, "", failingReader{}, &out, &errw); err != nil || out.Len() != 0 {
		t.Errorf("broken stdin: err %v, output %q", err, out.String())
	}
	for _, args := range [][]string{{"claude-code", "--max-wait", "soon"}, {"claude-code", "--max-wait", "-1s"}, {"claude-code", "extra"}} {
		errw.Reset()
		if err := runHook(args, "", strings.NewReader(bashEvent("make")), &out, &errw); err != nil || out.Len() != 0 {
			t.Errorf("runHook(%q): err %v, output %q", args, err, out.String())
		}
		if errw.Len() == 0 {
			t.Errorf("runHook(%q): no warning on stderr", args)
		}
	}
	for _, args := range [][]string{nil, {"codex"}} {
		if err := runHook(args, "", strings.NewReader(""), &out, &errw); err == nil {
			t.Errorf("runHook(%q) succeeded", args)
		}
	}
}

// TestClientConfigBadConfig checks that a broken config warns once and
// falls back to the default socket, so client commands, the hook included,
// still reach a throtd running on its last good config.
func TestClientConfigBadConfig(t *testing.T) {
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(`{"scheduler": {"mode": "enforc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var errw bytes.Buffer
	cfg := clientConfig(path, &errw)
	if cfg.SocketPath != config.DefaultSocketPath() {
		t.Errorf("socket %q, want the default %q", cfg.SocketPath, config.DefaultSocketPath())
	}
	want := "throt: config " + path + ` is invalid: scheduler.mode must be off, observe, or enforce, not "enforc"; using the default socket` + "\n"
	if errw.String() != want {
		t.Errorf("stderr %q, want %q", errw.String(), want)
	}
	errw.Reset()
	if clientConfig(t.TempDir()+"/missing.json", &errw); errw.Len() != 0 {
		t.Errorf("missing file warned: %q", errw.String())
	}
}
