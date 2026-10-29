package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// `throt setup AGENT` prints configuration for the user to paste. With
// --write it installs it in the user's home directory instead: it only
// adds, never removes, and backs up a file before changing it.

// claudeHookSettings is the settings.json fragment that installs the hook
// (https://code.claude.com/docs/en/hooks). "timeout" is in seconds; a
// PreToolUse hook that times out blocks the tool call, so --max-wait stays
// well below it.
const claudeHookSettings = `{
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
}`

// claudeMCPConfig is the .mcp.json fragment for the MCP server
// (https://code.claude.com/docs/en/mcp). Claude Code reads MCP servers from
// .mcp.json or ~/.claude.json, not from settings.json.
const claudeMCPConfig = `{
  "mcpServers": {
    "throt": {
      "command": "throt",
      "args": ["mcp"]
    }
  }
}`

// codexMCPConfig is the ~/.codex/config.toml block for the MCP server.
const codexMCPConfig = `[mcp_servers.throt]
command = "throt"
args = ["mcp"]`

// agentsParagraph tells an agent to prefix heavy commands itself.
const agentsParagraph = "Prefix heavy commands (builds, full test suites, container builds) with `throt run --`, " +
	"e.g. `throt run -- npm run build` or `cd web && throt run -- make`. " +
	"It waits for a free heavy-job slot and then runs the command unchanged; if throtd is down it runs at once."

const setupUsage = "usage: throt setup claude-code|codex [--write]"

// cmdSetup is 'throt setup'.
func cmdSetup(_ context.Context, e env, args []string) error {
	return runSetup(args, e.stdout)
}

// runSetup handles `throt setup AGENT [--write]`. --write uses $HOME (and
// $CODEX_HOME for Codex, as Codex does).
func runSetup(args []string, w io.Writer) error {
	write := false
	var rest []string
	for _, a := range args {
		if a == "--write" || a == "-write" {
			write = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 || rest[0] != "claude-code" && rest[0] != "codex" {
		return usageLine(setupUsage)
	}
	if !write {
		if rest[0] == "claude-code" {
			printClaudeCodeSetup(w)
		} else {
			printCodexSetup(w)
		}
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return exitError{code: 1, msg: "setup: " + err.Error()}
	}
	if rest[0] == "claude-code" {
		err = installClaudeCode(home, w)
	} else {
		err = installCodex(home, w)
	}
	if err != nil {
		return exitError{code: 1, msg: "setup: " + err.Error()}
	}
	return nil
}

// hookMarker identifies an installed throt hook, whatever its flags.
const hookMarker = "throt hook claude-code"

// installClaudeCode adds the PreToolUse hook to ~/.claude/settings.json
// unless some hook already runs `throt hook claude-code`. It keeps every
// other setting; only key order and indentation may change.
func installClaudeCode(home string, w io.Writer) error {
	f, err := readSetupFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return err
	}
	root := map[string]any{}
	if f.exists {
		if root, err = parseJSONObject(f.data); err != nil {
			return fmt.Errorf("%s is not valid JSON (%v); nothing changed. Fix it, or paste the hook from 'throt setup claude-code'", f.path, err)
		}
	}
	hooks := map[string]any{}
	if v, ok := root["hooks"]; ok {
		if hooks, ok = v.(map[string]any); !ok {
			return fmt.Errorf(`"hooks" in %s is not an object; nothing changed`, f.path)
		}
	}
	if hasCommand(hooks, hookMarker) {
		fmt.Fprintf(w, "Claude Code hook already installed in %s; nothing changed.\n", f.path)
		return nil
	}
	var pre []any
	if v, ok := hooks["PreToolUse"]; ok {
		if pre, ok = v.([]any); !ok {
			return fmt.Errorf(`"hooks.PreToolUse" in %s is not an array; nothing changed`, f.path)
		}
	}
	var snippet struct {
		Hooks struct{ PreToolUse []any } `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(claudeHookSettings), &snippet); err != nil || len(snippet.Hooks.PreToolUse) != 1 {
		return fmt.Errorf("internal error: bad hook snippet: %v", err)
	}
	hooks["PreToolUse"] = append(pre, snippet.Hooks.PreToolUse[0])
	root["hooks"] = hooks
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}
	backup, err := f.save(buf.Bytes())
	if err != nil {
		return err
	}
	if backup == "" {
		fmt.Fprintf(w, "Created %s with a PreToolUse hook for Bash:\n", f.path)
	} else {
		fmt.Fprintf(w, "Added a PreToolUse hook for Bash to %s:\n", f.path)
	}
	fmt.Fprintf(w, "  throt hook claude-code --max-wait 110s (timeout 120s)\n")
	if backup != "" {
		fmt.Fprintf(w, "Other settings are kept, but keys are now sorted and re-indented.\nThe previous file is %s.\n", backup)
	}
	fmt.Fprint(w, "Restart running Claude Code sessions to use it. For the MCP server, run:\n  claude mcp add --scope user throt -- throt mcp\n")
	return nil
}

// parseJSONObject parses data as one JSON object, keeping numbers as
// written.
func parseJSONObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the top-level value")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the top-level value is not an object")
	}
	return m, nil
}

// hasCommand reports whether any "command" string anywhere in v contains
// marker.
func hasCommand(v any, marker string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "command" && strings.Contains(s, marker) {
				return true
			}
			if hasCommand(e, marker) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasCommand(e, marker) {
				return true
			}
		}
	}
	return false
}

// installCodex adds the MCP server to config.toml and the throt run
// paragraph to AGENTS.md (Codex's global instructions) in $CODEX_HOME,
// ~/.codex by default, each unless already there.
func installCodex(home string, w io.Writer) error {
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".codex")
	}
	cfg, err := readSetupFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		return err
	}
	agents, err := readSetupFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		return err
	}
	changed := false
	for _, step := range []struct {
		f       setupFile
		present bool
		add     string
		what    string
	}{
		{cfg, hasCodexThrotServer(string(cfg.data)), codexMCPConfig, "[mcp_servers.throt]"},
		{agents, strings.Contains(string(agents.data), "throt run --"), agentsParagraph, "the 'throt run --' paragraph"},
	} {
		if step.present {
			fmt.Fprintf(w, "%s already has %s; unchanged.\n", step.f.path, step.what)
			continue
		}
		backup, err := step.f.save(appendBlock(step.f.data, step.add))
		if err != nil {
			return err
		}
		changed = true
		if backup == "" {
			fmt.Fprintf(w, "Created %s with %s.\n", step.f.path, step.what)
		} else {
			fmt.Fprintf(w, "Appended %s to %s; the previous file is %s.\n", step.what, step.f.path, backup)
		}
	}
	if !changed {
		fmt.Fprintln(w, "Codex setup already installed; nothing changed.")
	} else {
		fmt.Fprintln(w, "Restart running Codex sessions to use it.")
	}
	return nil
}

// appendBlock appends block to data, after a blank line when data is not
// empty.
func appendBlock(data []byte, block string) []byte {
	out := bytes.Clone(data)
	if len(out) > 0 {
		if out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, '\n')
	}
	return append(append(out, block...), '\n')
}

// hasCodexThrotServer reports whether TOML text already defines the throt
// MCP server in any common form: a [mcp_servers.throt] table (or a
// subtable), a throt key in [mcp_servers], or a dotted key. It reads text
// only, without a TOML parser, so it errs towards "present": appending a
// second definition would make the file invalid.
func hasCodexThrotServer(text string) bool {
	table := ""
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.NewReplacer(" ", "", "\t", "", `"`, "", "'", "", "\r", "").Replace(line)
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(line, "[]")
			if table == "mcp_servers.throt" || strings.HasPrefix(table, "mcp_servers.throt.") {
				return true
			}
			continue
		}
		key := table + "." + line
		if table == "" {
			key = line
		}
		if strings.HasPrefix(key, "mcp_servers.throt=") || strings.HasPrefix(key, "mcp_servers.throt.") ||
			strings.HasPrefix(key, "mcp_servers=") && strings.Contains(key, "throt") {
			return true
		}
	}
	return false
}

// setupFile is a config file that --write may change.
type setupFile struct {
	path   string // as named: for messages and the backup
	real   string // symlinks resolved: the file written in place
	data   []byte
	mode   fs.FileMode
	exists bool
}

// readSetupFile reads path, following symlinks. A missing file is not an
// error; it would be created with mode 0600.
func readSetupFile(path string) (setupFile, error) {
	f := setupFile{path: path, real: path, mode: 0o600}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return f, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return f, err
	}
	if !info.Mode().IsRegular() {
		return f, fmt.Errorf("%s is not a regular file", path)
	}
	if f.data, err = os.ReadFile(real); err != nil {
		return f, err
	}
	f.real, f.mode, f.exists = real, info.Mode().Perm(), true
	return f, nil
}

// save replaces the file with data atomically, keeping its mode. An
// existing file is first copied to path.throt-backup, whose path it
// returns.
func (f setupFile) save(data []byte) (backup string, err error) {
	if !f.exists {
		if err := os.MkdirAll(filepath.Dir(f.real), 0o700); err != nil {
			return "", err
		}
	} else {
		backup = f.path + ".throt-backup"
		if err := writeAtomic(backup, f.data, f.mode); err != nil {
			return "", err
		}
	}
	return backup, writeAtomic(f.real, data, f.mode)
}

// writeAtomic writes data to a temporary file next to path and renames it
// over path, so readers see the old file or the new one, never a part.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

func printClaudeCodeSetup(w io.Writer) {
	fmt.Fprintf(w, `Claude Code setup (nothing is written; paste these yourself, or run
"throt setup claude-code --write" to add the hook to ~/.claude/settings.json).

1. Hook: add this to ~/.claude/settings.json (all projects) or
   .claude/settings.json (one project). If the file already has "hooks",
   add the PreToolUse entry to them instead of replacing them.

%s

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

%s

Both need throt on PATH, and the MCP tools need throtd running.
`, claudeHookSettings, claudeMCPConfig)
}

func printCodexSetup(w io.Writer) {
	fmt.Fprintf(w, `Codex setup (nothing is written; paste these yourself, or run
"throt setup codex --write" to add both to ~/.codex).

1. MCP server (pressure and capacity tools): add this to ~/.codex/config.toml:

%s

2. Add this paragraph to ~/.codex/AGENTS.md (all projects) or a project AGENTS.md:

%s

Both need throt on PATH, and the MCP tools need throtd running.
`, codexMCPConfig, agentsParagraph)
}
