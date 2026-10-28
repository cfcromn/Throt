package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
)

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
}

func (f *fakeHook) waiter() hookWaiter {
	f.started = f.clock
	return hookWaiter{
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
		if out := f.run(in, 50*time.Second); out != "" || f.built != 0 || f.calls != 0 {
			t.Errorf("%s: output %q, check built %d, called %d; want no daemon contact", name, out, f.built, f.calls)
		}
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

// TestHookCheckBadConfig checks that an unreadable config means "run now".
func TestHookCheckBadConfig(t *testing.T) {
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hookCheck(path)(context.Background()); err == nil {
		t.Error("bad config did not fail the check")
	}
}
