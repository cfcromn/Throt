package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// FuzzHookClaudeCode feeds arbitrary stdin to the Claude Code hook. The
// parsers must not panic (claudeCode recovers, so they are also called
// directly), and the only output is nothing or one {"systemMessage"} line,
// written only after the hook waited for a heavy command.
func FuzzHookClaudeCode(f *testing.F) {
	for _, s := range []string{
		bashEvent("make -j8"), bashEvent("cd a && go test ./..."), bashEvent("FOO=1 time cargo build"),
		bashEvent("go version"), bashEvent("ls"), bashEvent("make | tee log"), bashEvent(""),
		bashEvent("cd && make"), bashEvent("'make'"), bashEvent("make\x00"), bashEvent("\xff"),
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":null}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":1}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":[]}`,
		`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Edit"}`,
		`{}`, `null`, `[]`, ``, `{`, "\xff",
	} {
		f.Add([]byte(s), uint8(0))
	}
	f.Add([]byte(bashEvent("make")), uint8(0b0011))
	f.Add([]byte(bashEvent("gradle build")), uint8(0b1000_0001))
	f.Fuzz(func(t *testing.T, in []byte, pattern uint8) {
		command, why := bashCommand(in)
		_, heavy := heavyCommand(command)
		if why != "" && command != "" {
			t.Fatalf("input %q: both a command and a reason to ignore it", in)
		}
		// pattern: bit i says whether check i finds capacity; bit 7 makes
		// the checks fail (daemon down).
		f := &fakeHook{t: t, clock: time.Unix(0, 0)}
		for i := range 7 {
			f.oks = append(f.oks, pattern&(1<<i) != 0)
		}
		if pattern&0x80 != 0 {
			f.errs = []error{errors.New("down")}
		}
		const maxWait = 5 * time.Second
		out := f.run(string(in), maxWait)
		if f.clock.Sub(f.started) > maxWait {
			t.Fatalf("input %q: waited %v, over %v", in, f.clock.Sub(f.started), maxWait)
		}
		if out == "" {
			return
		}
		if !heavy || why != "" {
			t.Fatalf("input %q: output %q for an event that is not a heavy Bash command", in, out)
		}
		line, rest, ok := strings.Cut(out, "\n")
		var r map[string]string
		if !ok || rest != "" || json.Unmarshal([]byte(line), &r) != nil || len(r) != 1 ||
			!strings.HasPrefix(r["systemMessage"], "throt: ") || !bytes.Equal([]byte(out), bytes.ToValidUTF8([]byte(out), nil)) {
			t.Fatalf("input %q: output %q is not one systemMessage line", in, out)
		}
		if f.calls < 2 {
			t.Fatalf("input %q: output %q without waiting", in, out)
		}
	})
}
