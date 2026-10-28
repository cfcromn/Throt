# Throt

[![CI](https://github.com/cfcromn/Throt/actions/workflows/ci.yml/badge.svg)](https://github.com/cfcromn/Throt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Resource-aware scheduler and TUI for local AI coding agents.**

> Run more coding agents without melting your machine.

Throt is a local-first Go daemon and terminal UI that observes coding-agent process trees, detects resource pressure, and coordinates expensive child workloads so you can keep multiple agents productive without making the machine unusable.

## Status

**Pre-alpha (v0.1 Observe).** Throt samples host resources, reports a stable pressure level, attributes processes and heavy workloads to coding agents, and shows it all in a TUI. Manual pause/resume and an opt-in heavy-slot scheduler (`observe` / `enforce`) are available. Expect breaking changes before 1.0.

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

CPU is a percentage of one core, so a process using two cores shows 200%. Keys: `tab` switches between the agents and heavy-workload tables, `j`/`k` or the arrow keys move the selection, `enter` shows details for the selected row, `?` shows help, `q` quits. Metrics the platform cannot provide show as `n/a`, and if `throtd` is not running the TUI says so and reconnects when it starts. Narrow terminals (down to 40×12) drop low-value columns and truncate names.

## Install

Download a release archive for macOS or Linux (amd64/arm64) from [Releases](https://github.com/cfcromn/Throt/releases), verify it against `checksums.txt`, and put `throt` and `throtd` on your `PATH`. Or build from source with the Go version in `go.mod`:

```bash
go install github.com/cfcromn/Throt/cmd/throt@latest
go install github.com/cfcromn/Throt/cmd/throtd@latest
```

## Usage

```bash
throtd &        # start the daemon
throt           # terminal UI (same as `throt top`)
throt ping      # check that the client can reach it
throt status    # pressure, host resources, agents, heavy workloads
throt events    # recent daemon events as JSON lines
throt doctor    # sanitized diagnostics to paste into a bug report
throt pause PID # pause a heavy workload owned by one of your agents
throt resume PID
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

`profile` presets the scheduler; explicit keys override it.
- `performance`: limit = CPUs/2, not adaptive.
- `balanced` (default): limit = CPUs/4, adaptive.
- `cool`: limit 1, adaptive, may pause one workload under critical pressure.

A profile never turns the scheduler on.
- `adaptive` halves the running limit under high pressure and raises it by one per 30s of calm.
- `per_agent_limit` caps heavy workloads per agent, so one agent can't take every slot.

`scheduler.mode` is `off` (default), `observe` (log what it would do as `scheduler_decision` events), or `enforce` (queue new agent-owned heavy workloads with SIGSTOP when `heavy_limit` heavy workloads are already running, and resume them as slots free up). Try `observe` first and watch `throt events`.

## Roadmap

Outcomes, not dates.

| Version | Goal | Highlights |
|---|---|---|
| v0.1 Observe | "What is making my machine slow, and which agent owns it?" | host metrics, process graph, agent attribution, TUI, event timeline, `throt doctor`; no automatic control |
| v0.2 Control | Reduce overlapping heavy workloads without losing work | manual pause/resume, workload classes, heavy-slot limit, opt-in adaptive scheduler, audit log |
| v0.3 Adaptive | Keep the machine responsive automatically | feedback-based concurrency, hysteresis/cooldowns, per-project budgets, profiles |
| v0.4 Ecosystem | Integrate everywhere | Linux, stable local API, MCP server, adapter SDK |

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
