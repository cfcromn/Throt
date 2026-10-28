package main

import (
	"bytes"
	"encoding/json"
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
	for _, args := range [][]string{nil, {"vim"}, {"codex", "x"}} {
		if err := runSetup(args, &bytes.Buffer{}); err == nil {
			t.Errorf("runSetup(%q) succeeded", args)
		}
	}
}
