package main

import (
	"context"
	"fmt"
	"io"
)

// `throt setup AGENT` prints configuration for the user to paste. It never
// writes files.

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

const setupUsage = "usage: throt setup claude-code|codex"

// cmdSetup is 'throt setup'.
func cmdSetup(_ context.Context, e env, args []string) error {
	return runSetup(args, e.stdout)
}

func runSetup(args []string, w io.Writer) error {
	if len(args) != 1 {
		return usageLine(setupUsage)
	}
	switch args[0] {
	case "claude-code":
		printClaudeCodeSetup(w)
	case "codex":
		printCodexSetup(w)
	default:
		return usageLine(setupUsage)
	}
	return nil
}

func printClaudeCodeSetup(w io.Writer) {
	fmt.Fprintf(w, `Claude Code setup (nothing is written; paste these yourself).

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
	fmt.Fprintf(w, `Codex setup (nothing is written; paste these yourself).

1. MCP server (pressure and capacity tools): add this to ~/.codex/config.toml:

%s

2. Add this paragraph to your AGENTS.md:

%s

Both need throt on PATH, and the MCP tools need throtd running.
`, codexMCPConfig, agentsParagraph)
}
