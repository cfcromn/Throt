# Throt

[![CI](https://github.com/cfcromn/Throt/actions/workflows/ci.yml/badge.svg)](https://github.com/cfcromn/Throt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Resource-aware scheduler and TUI for local AI coding agents.**

> Run more coding agents without melting your machine.

Throt is a local-first Go daemon and terminal UI that observes coding-agent process trees, detects resource pressure, and coordinates expensive child workloads so you can keep multiple agents productive without making the machine unusable.

## Status

**Pre-1.0 (v0.4).** Throt samples host resources and foreground latency, reports a stable pressure level, attributes processes and heavy workloads to coding agents, and shows it all in a TUI and over MCP. Manual pause/resume and an opt-in adaptive heavy-slot scheduler (`observe` / `enforce`) are available. Expect breaking changes before 1.0.

## Problem

Modern coding-agent workflows run several agents at once. The agents themselves mostly wait on model responses, but their child processes collide:

- multiple Gradle/Maven/Go/npm builds;
- Docker builds and Compose stacks;
- test suites, language servers, and compilers;
- local databases and supporting services.

The result is CPU saturation, memory pressure, swap growth, thermal throttling, and a sluggish foreground. Throt optimizes **developer responsiveness and useful throughput**, not merely low CPU usage. A lower CPU percentage alone is not success.

## Principles

1. **Observe before controlling.** v0.1 ships attribution and pressure telemetry before any automatic scheduling.
2. **No agent lock-in.** Attribution is adapter-based; the core does not depend on one coding agent.
3. **Local-first.** No telemetry leaves the machine.
4. **Fail open.** If Throt crashes, agents and their work continue normally.
5. **Explain every intervention.** Every queue/pause/resume decision has a human-readable reason.
6. **Measure outcomes.** Scheduler changes require responsiveness and throughput benchmarks.
7. **Small daemon, rich clients.** Scheduling lives in `throtd`; TUI/CLI/MCP are clients.

## UX

`throt` (or `throt top`) opens a terminal UI that refreshes every second. It only observes: quitting it never affects `throtd`.

```text
$ throt
SYSTEM  pressure HIGH  memory_used>=0.9
CPU   ██████████████████████████████░░░░░░░░░░   74%  load 1.12/cpu
MEM   ██████████████████████████████████████░░   95%  30.5 GiB / 32.0 GiB
SWAP  ██████████████░░░░░░░░░░░░░░░░░░░░░░░░░░   35%  1.4 GiB / 4.0 GiB
▸ AGENTS (3)
  AGENT                                 PID     CPU        MEM  CHILDREN
> codex-1                              4120    231%    2.8 GiB         5
  claude-1                             3988    104%    1.8 GiB         7
  claude-2                             5310     12%    921 MiB         2
  HEAVY WORKLOADS (2)
  OWNER               PID  PROCESS           CLASS               CPU        MEM
  codex-1            4312  java              build              190%    2.3 GiB
  claude-1           4471  buildkitd         container-build     60%    420 MiB
TIMELINE
  14:02:52  pressure_changed    pressure elevated -> high
  14:02:13  agent_discovered    agent_discovered codex-1
  14:02:11  daemon_started      listening on /run/user/501/throt/throtd.sock

q quit  tab focus  j/k move  enter details  ? help
```

CPU is a percentage of one core, so a process using two cores shows 200%. Keys: `tab` switches between the agents and heavy-workload tables, `j`/`k` or the arrow keys move the selection, `enter` shows details for the selected row, `p`/`r` pause or resume the selected heavy workload after a `y/n` confirmation, `?` shows help, `q` quits. Metrics the platform cannot provide show as `n/a`, and if `throtd` is not running the TUI says so and reconnects when it starts. Narrow terminals (down to 40×12) drop low-value columns and truncate names.

## Install

Download a release archive for macOS or Linux (amd64/arm64) from [Releases](https://github.com/cfcromn/Throt/releases), verify it against `checksums.txt`, and put `throt` and `throtd` on your `PATH`. Or build from source with the Go version in `go.mod`:

```bash
go install github.com/cfcromn/Throt/cmd/throt@latest
go install github.com/cfcromn/Throt/cmd/throtd@latest
```

### Run at login

On stop, `throtd` resumes anything it paused (SIGTERM and SIGINT both work), so service managers can stop it safely.

macOS (launchd): save as `~/Library/LaunchAgents/dev.throt.throtd.plist`, adjust the path, then run `launchctl load ~/Library/LaunchAgents/dev.throt.throtd.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.throt.throtd</string>
  <key>ProgramArguments</key><array><string>/usr/local/bin/throtd</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
```

Linux (systemd user unit): save as `~/.config/systemd/user/throtd.service`, then run `systemctl --user enable --now throtd`:

```ini
[Unit]
Description=Throt resource-aware scheduler for coding agents

[Service]
ExecStart=%h/.local/bin/throtd
Restart=on-failure

[Install]
WantedBy=default.target
```

### Upgrade and rollback

Stop `throtd` (this resumes any paused work), replace both binaries, and start it again. To roll back, install the previous release the same way. Configuration is backward compatible within a minor series, and unknown keys are reported as errors rather than silently ignored. `throt` and `throtd` must be the same release, and a mismatch is reported with both protocol versions. Published versions are never rewritten; fixes ship as new patch releases.

## Usage

```bash
throtd &        # start the daemon
throt           # terminal UI (same as `throt top`)
throt ping      # check that the client can reach it
throt status    # pressure, host resources, agents, heavy workloads
throt stats     # per-agent CPU time, peak memory, heavy jobs, time queued since throtd started (--json)
throt events    # recent daemon events as JSON lines
throt events -f # keep printing new events until interrupted
throt explain PID # why a heavy workload is running, paused, or queued
throt doctor    # sanitized diagnostics to paste into a bug report
throt pause PID # pause a heavy workload owned by one of your agents
throt resume PID
throt reload    # re-read the config without restarting (or send throtd SIGHUP)
throt run -- npm run build    # wait for a free heavy slot, then run the command
throt completion zsh          # shell completion script (bash, zsh, fish)
```

### Wait before starting: `throt run`

`throt run [--timeout D] [--no-wait] -- CMD [ARGS...]` is the gentle alternative to being paused: it checks capacity *before* the command starts, using the same verdict as `throt_capacity`. It polls `throtd` once per second until a heavy job is OK to start (pressure below high, a free heavy slot, nothing queued, and the calling agent within its `per_agent_limit`), printing a notice to stderr at most every 10s. Then it execs the command in its own place, so the PID, signals, and exit status are the command's own. The agent is the nearest agent process among its ancestors. A missing command exits 127.

- `--timeout D` stops waiting after `D` and runs the command anyway (the default, 0, waits forever).
- `--no-wait` checks once, prints the verdict, and runs regardless.

It fails open: if `throtd` is unreachable, it prints one notice and runs immediately, so a stopped daemon never blocks your work. It never pauses anything. It is also advisory: nothing is reserved, so two `throt run` commands that check at the same moment can both start. With `scheduler.mode` set to `enforce`, `throtd` still queues the extra one.

To have coding agents use it, add a line such as this to your `AGENTS.md` or `CLAUDE.md`:

```markdown
Prefix heavy commands (builds, full test suites, container builds) with `throt run --`, e.g. `throt run -- npm run build`.
```

Shell completion: `source <(throt completion bash)` or `source <(throt completion zsh)` in your shell's rc file, or `throt completion fish | source`. After `pause`, `resume`, and `explain` it completes the heavy-workload PIDs from `throt status`.

## Use with coding agents (MCP)

`throt mcp` runs a read-only [MCP](https://modelcontextprotocol.io) server on stdio. Agents can check pressure and heavy-slot capacity before starting a big build instead of competing with the scheduler. It offers four tools:
- `throt_status`: pressure, resources, agents, and heavy workloads.
- `throt_capacity`: an advisory `ok_to_start` with a reason and suggestion.
- `throt_events`: recent daemon events.
- `throt_stats`: per-agent CPU time, peak memory, heavy jobs, and time queued since throtd started.

None of them pauses, resumes, or changes configuration; control stays with you through the CLI. `throtd` must be running. `throt_capacity` defaults to the agent that launched the server. Tool output, including process names and PIDs, goes into the agent's context, so it reaches your model provider.

```bash
claude mcp add throt -- throt mcp
```

For Codex, add to `~/.codex/config.toml`:

```toml
[mcp_servers.throt]
command = "throt"
args = ["mcp"]
```

## Configuration

Optional, at `~/.config/throt/config.json` (or `$XDG_CONFIG_HOME/throt/config.json`). Unknown keys are errors.

```json
{
  "sample_interval": "2s",
  "profile": "balanced",
  "scheduler": {
    "mode": "observe",
    "heavy_limit": 3,
    "adaptive": true,
    "per_agent_limit": 0,
    "resume_cooldown": "10s",
    "pause_on_critical": false,
    "max_queue_wait": "5m"
  }
}
```

Custom agents can be added with `"agents": [{"name": "myagent", "executables": ["my-agent"]}]`. Generic runtimes such as `node` and build tools such as `go` are refused, because they would match unrelated processes. On Linux, executable names are truncated to 15 characters.

`profile` presets the scheduler; explicit keys override it.
- `performance`: limit = CPUs/2 (at least 2), not adaptive.
- `balanced` (default): limit = CPUs/4 (at least 2), adaptive.
- `cool`: limit 1, adaptive, may pause one workload under critical pressure.

A profile never turns the scheduler on.
- `adaptive` halves the running limit under high pressure and raises it by one per 30s of calm.
- `per_agent_limit` caps heavy workloads per agent, so one agent can't take every slot.

`scheduler.mode` is `off` (default), `observe` (log what it would do as `scheduler_decision` events), or `enforce` (queue new agent-owned heavy workloads with SIGSTOP when `heavy_limit` heavy workloads are already running, and resume them as slots free up). Try `observe` first and watch `throt events`.

## Roadmap

Outcomes, not dates.

| Version | Goal | Highlights | Status |
|---|---|---|---|
| v0.1 Observe | "What is making my machine slow, and which agent owns it?" | host metrics, process graph, agent attribution, TUI, event timeline, `throt doctor` | shipped |
| v0.2 Control | Reduce overlapping heavy workloads without losing work | manual pause/resume, heavy-slot scheduler (opt-in), audit events, crash recovery | shipped |
| v0.3 Adaptive | Keep the machine responsive automatically | latency signal, adaptive limit, per-agent budgets, profiles | shipped |
| v0.4 Ecosystem | Integrate everywhere | Linux, documented local API, MCP server, custom agent adapters | shipped |
| 1.0 | Safe to enable by default under a profile | real-machine benchmarks, scheduler default decided from evidence | next |

**1.0** requires two supported OS families, a stable config/API policy, no known process-loss bug, reproducible benchmarks, and a security review of IPC and process-control boundaries.

## Non-goals

Throt is not another coding agent, a replacement for Orca/Claude Code/Codex/OpenCode, a container orchestrator, a cloud scheduler, or an excuse to kill processes without explainable policy.

## Documentation

- [Architecture & design](docs/ARCHITECTURE.md)
- [Contributing & development](CONTRIBUTING.md)
- [Security & privacy](SECURITY.md)
- [Changelog](CHANGELOG.md)

## License

[MIT](LICENSE)
