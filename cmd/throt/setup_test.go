package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonBlocks returns each top-level JSON object in s that starts on a line
// of its own with "{" and ends on a line of its own with "}".
func jsonBlocks(s string) []string {
	var blocks []string
	var cur []string
	for _, line := range strings.Split(s, "\n") {
		switch {
		case cur == nil && line == "{":
			cur = []string{line}
		case cur != nil:
			cur = append(cur, line)
			if line == "}" {
				blocks = append(blocks, strings.Join(cur, "\n"))
				cur = nil
			}
		}
	}
	return blocks
}

func TestSetupClaudeCode(t *testing.T) {
	var out bytes.Buffer
	if err := runSetup([]string{"claude-code"}, &out); err != nil {
		t.Fatal(err)
	}
	blocks := jsonBlocks(out.String())
	if len(blocks) != 2 {
		t.Fatalf("found %d JSON blocks, want 2:\n%s", len(blocks), out.String())
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	dec := json.NewDecoder(strings.NewReader(blocks[0]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&settings); err != nil {
		t.Fatalf("settings snippet: %v", err)
	}
	pre := settings.Hooks["PreToolUse"]
	if len(settings.Hooks) != 1 || len(pre) != 1 || pre[0].Matcher != "Bash" || len(pre[0].Hooks) != 1 {
		t.Fatalf("settings snippet = %+v", settings)
	}
	h := pre[0].Hooks[0]
	if h.Type != "command" || h.Command != "throt hook claude-code --max-wait 110s" || h.Timeout != 120 {
		t.Errorf("hook = %+v", h)
	}
	// --max-wait plus one bounded last check must fit in the timeout.
	maxWait, err := time.ParseDuration(strings.TrimPrefix(h.Command, "throt hook claude-code --max-wait "))
	if err != nil || maxWait+runCallTimeout >= time.Duration(h.Timeout)*time.Second {
		t.Errorf("max-wait %v (%v) does not fit in timeout %ds", maxWait, err, h.Timeout)
	}
	for _, want := range []string{"advisory", "no slot is reserved", "Do not add permission allow rules"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("setup text lacks %q", want)
		}
	}
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	dec = json.NewDecoder(strings.NewReader(blocks[1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mcp); err != nil {
		t.Fatalf("mcp snippet: %v", err)
	}
	if s := mcp.MCPServers["throt"]; s.Command != "throt" || len(s.Args) != 1 || s.Args[0] != "mcp" {
		t.Errorf("mcp server = %+v", s)
	}
	if !strings.Contains(out.String(), "claude mcp add --scope user throt -- throt mcp") {
		t.Error("no claude mcp add command")
	}
}

func TestSetupCodex(t *testing.T) {
	var out bytes.Buffer
	if err := runSetup([]string{"codex"}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	// The TOML block uses only a table header and basic strings or string
	// arrays, whose TOML values are also valid JSON values.
	i := strings.Index(s, "[mcp_servers.throt]\n")
	if i < 0 {
		t.Fatalf("no [mcp_servers.throt] table:\n%s", s)
	}
	want := map[string]any{"command": "throt", "args": []any{"mcp"}}
	got := map[string]any{}
	for _, line := range strings.Split(s[i:], "\n")[1:] {
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, " = ")
		if !ok {
			t.Fatalf("bad TOML line %q", line)
		}
		var val any
		if err := json.Unmarshal([]byte(v), &val); err != nil {
			t.Fatalf("TOML value %q: %v", v, err)
		}
		got[k] = val
	}
	if len(got) != 2 || got["command"] != want["command"] {
		t.Errorf("TOML block = %v, want %v", got, want)
	}
	if a, ok := got["args"].([]any); !ok || len(a) != 1 || a[0] != "mcp" {
		t.Errorf("args = %v", got["args"])
	}
	if !strings.Contains(s, agentsParagraph) || !strings.Contains(agentsParagraph, "`throt run --`") {
		t.Error("no AGENTS.md paragraph telling Codex to use throt run --")
	}
}

func TestSetupUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{nil, {"vim"}, {"codex", "x"}, {"--write"}, {"vim", "--write"}, {"codex", "--write", "x"}} {
		if err := runSetup(args, &bytes.Buffer{}); err == nil {
			t.Errorf("runSetup(%q) succeeded", args)
		}
	}
}

// setupHome makes a temporary $HOME, and no $CODEX_HOME, for --write.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	return home
}

func writeTestFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// throtHooks counts the hooks in settings that run throt.
func throtHooks(settings map[string]any) int {
	n := 0
	pre, _ := settings["hooks"].(map[string]any)["PreToolUse"].([]any)
	for _, e := range pre {
		for _, h := range e.(map[string]any)["hooks"].([]any) {
			if strings.Contains(h.(map[string]any)["command"].(string), hookMarker) {
				n++
			}
		}
	}
	return n
}

func TestSetupClaudeCodeWrite(t *testing.T) {
	const existing = `{
  "model": "opus",
  "env": {"A": "<b>&", "N": 1.50},
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "PreToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "lint.sh"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  }
}`
	tests := []struct {
		name     string
		existing string
		mode     os.FileMode // 0: no file
		wantOut  string      // in stdout
		wantErr  string      // in the error
		changed  bool
	}{
		{name: "fresh install", wantOut: "Created ", changed: true},
		{name: "unrelated hooks kept", existing: existing, mode: 0o644, wantOut: "Added a PreToolUse hook", changed: true},
		{name: "no hooks yet", existing: `{"model": "opus"}`, mode: 0o640, wantOut: "Added a PreToolUse hook", changed: true},
		{name: "already installed", existing: `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/opt/bin/throt hook claude-code --max-wait 50s"}]}]}}`,
			mode: 0o600, wantOut: "already installed"},
		{name: "invalid JSON", existing: `{"model": "opus",}`, mode: 0o600, wantErr: "not valid JSON"},
		{name: "empty file", existing: "", mode: 0o600, wantErr: "not valid JSON"},
		{name: "not an object", existing: `[]`, mode: 0o600, wantErr: "not valid JSON"},
		{name: "trailing data", existing: `{} {}`, mode: 0o600, wantErr: "not valid JSON"},
		{name: "hooks not an object", existing: `{"hooks": []}`, mode: 0o600, wantErr: "not an object"},
		{name: "PreToolUse not an array", existing: `{"hooks": {"PreToolUse": {}}}`, mode: 0o600, wantErr: "not an array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := setupHome(t)
			path := filepath.Join(home, ".claude", "settings.json")
			exists := tt.mode != 0
			if exists {
				writeTestFile(t, path, tt.existing, tt.mode)
			}
			var out bytes.Buffer
			err := runSetup([]string{"claude-code", "--write"}, &out)
			_, berr := os.Stat(path + ".throt-backup")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if got := readTestFile(t, path); got != tt.existing {
					t.Errorf("file changed to %q", got)
				}
				if !os.IsNotExist(berr) {
					t.Errorf("backup written: %v", berr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("stdout %q lacks %q", out.String(), tt.wantOut)
			}
			wantMode := tt.mode
			if !exists {
				wantMode = 0o600
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != wantMode {
				t.Fatalf("stat: %v, %v; want mode %v", fi, err, wantMode)
			}
			data := readTestFile(t, path)
			if !tt.changed {
				if data != tt.existing || !os.IsNotExist(berr) {
					t.Errorf("file changed to %q (backup: %v)", data, berr)
				}
				return
			}
			if exists {
				if b := readTestFile(t, path+".throt-backup"); b != tt.existing {
					t.Errorf("backup %q, want the previous file", b)
				}
			} else if !os.IsNotExist(berr) {
				t.Errorf("backup of a new file: %v", berr)
			}
			got, err := parseJSONObject([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if throtHooks(got) != 1 {
				t.Fatalf("throt hooks in %s", data)
			}
			// Without the added entry, the rest must equal the old file.
			want := map[string]any{}
			if exists {
				want, _ = parseJSONObject([]byte(tt.existing))
			}
			hooks := got["hooks"].(map[string]any)
			pre := hooks["PreToolUse"].([]any)
			if len(pre) == 1 {
				delete(hooks, "PreToolUse")
			} else {
				hooks["PreToolUse"] = pre[:len(pre)-1]
			}
			if len(hooks) == 0 {
				delete(got, "hooks")
			}
			if g, w := fmt.Sprint(got), fmt.Sprint(want); g != w {
				t.Errorf("other settings changed:\n got %s\nwant %s", g, w)
			}
			if exists && strings.Contains(tt.existing, "<b>&") && !strings.Contains(data, `"<b>&"`) {
				t.Error("strings were HTML-escaped")
			}

			out.Reset()
			if err := runSetup([]string{"--write", "claude-code"}, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "already installed") || readTestFile(t, path) != data {
				t.Errorf("second run changed something: %q", out.String())
			}
		})
	}
}

// TestSetupClaudeCodeWriteSymlink checks that a symlinked settings.json,
// as dotfile managers make, is updated in place, not replaced.
func TestSetupClaudeCodeWriteSymlink(t *testing.T) {
	home := setupHome(t)
	target := filepath.Join(home, "dotfiles", "settings.json")
	writeTestFile(t, target, `{"model": "opus"}`, 0o644)
	link := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := runSetup([]string{"claude-code", "--write"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v, %v", fi, err)
	}
	if !strings.Contains(readTestFile(t, target), hookMarker) {
		t.Error("target not updated")
	}
}

func TestSetupCodexWrite(t *testing.T) {
	const block = "[mcp_servers.throt]\ncommand = \"throt\"\nargs = [\"mcp\"]\n"
	const para = agentsParagraph + "\n"
	ptr := func(s string) *string { return &s }
	tests := []struct {
		name                   string
		config, agents         *string // nil: no file
		wantConfig, wantAgents string  // "": unchanged
	}{
		{name: "fresh install", wantConfig: block, wantAgents: para},
		{
			name:       "appends",
			config:     ptr("model = \"o3\"\n\n[mcp_servers.other]\ncommand = \"x\""),
			agents:     ptr("# My rules\nBe brief.\n"),
			wantConfig: "model = \"o3\"\n\n[mcp_servers.other]\ncommand = \"x\"\n\n" + block,
			wantAgents: "# My rules\nBe brief.\n\n" + para,
		},
		{name: "already installed", config: ptr("[mcp_servers.throt]\ncommand = \"/opt/throt\"\n"), agents: ptr("Use `throt run --` for builds.\n")},
		{name: "quoted header", config: ptr("[ mcp_servers.\"throt\" ] # mine\n"), wantAgents: para},
		{name: "subtable", config: ptr("[mcp_servers.throt.env]\nA = \"1\"\n"), wantAgents: para},
		{name: "inline table", config: ptr("[mcp_servers]\nthrot = { command = \"throt\" }\n"), wantAgents: para},
		{name: "dotted key", config: ptr("mcp_servers.throt.command = \"throt\"\n"), wantAgents: para},
		{name: "commented out", config: ptr("# [mcp_servers.throt]\n"), wantConfig: "# [mcp_servers.throt]\n\n" + block, wantAgents: para},
		{name: "other table's key", config: ptr("[profiles]\nmcp_servers.throt = 1\n"), wantConfig: "[profiles]\nmcp_servers.throt = 1\n\n" + block, wantAgents: para},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := setupHome(t)
			dir := filepath.Join(home, ".codex")
			cfg, ag := filepath.Join(dir, "config.toml"), filepath.Join(dir, "AGENTS.md")
			if tt.config != nil {
				writeTestFile(t, cfg, *tt.config, 0o640)
			}
			if tt.agents != nil {
				writeTestFile(t, ag, *tt.agents, 0o644)
			}
			var out bytes.Buffer
			if err := runSetup([]string{"codex", "--write"}, &out); err != nil {
				t.Fatal(err)
			}
			for _, f := range []struct {
				path, want string
				old        *string
				mode       os.FileMode
			}{{cfg, tt.wantConfig, tt.config, 0o640}, {ag, tt.wantAgents, tt.agents, 0o644}} {
				got := readTestFile(t, f.path)
				_, berr := os.Stat(f.path + ".throt-backup")
				switch {
				case f.want == "":
					if got != *f.old || !os.IsNotExist(berr) {
						t.Errorf("%s changed to %q (backup: %v)", f.path, got, berr)
					}
					continue
				case got != f.want:
					t.Errorf("%s = %q, want %q", f.path, got, f.want)
				}
				mode := os.FileMode(0o600)
				if f.old != nil {
					mode = f.mode
					if b := readTestFile(t, f.path+".throt-backup"); b != *f.old {
						t.Errorf("%s backup %q, want %q", f.path, b, *f.old)
					}
				} else if !os.IsNotExist(berr) {
					t.Errorf("%s: backup of a new file: %v", f.path, berr)
				}
				if fi, err := os.Stat(f.path); err != nil || fi.Mode().Perm() != mode {
					t.Errorf("%s: %v, %v; want mode %v", f.path, fi, err, mode)
				}
			}
			if all := tt.wantConfig == "" && tt.wantAgents == ""; all != strings.Contains(out.String(), "already installed") {
				t.Errorf("stdout %q", out.String())
			}
			c1, a1 := readTestFile(t, cfg), readTestFile(t, ag)
			out.Reset()
			if err := runSetup([]string{"codex", "--write"}, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "already installed") || readTestFile(t, cfg) != c1 || readTestFile(t, ag) != a1 {
				t.Errorf("second run changed something: %q", out.String())
			}
		})
	}
}

func TestSetupCodexWriteHonorsCodexHome(t *testing.T) {
	home := setupHome(t)
	dir := filepath.Join(home, "elsewhere")
	t.Setenv("CODEX_HOME", dir)
	if err := runSetup([]string{"codex", "--write"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readTestFile(t, filepath.Join(dir, "config.toml")), "[mcp_servers.throt]") {
		t.Error("config.toml not in $CODEX_HOME")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Errorf("~/.codex touched: %v", err)
	}
}
