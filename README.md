# Throt

[![CI](https://github.com/cfcromn/Throt/actions/workflows/ci.yml/badge.svg)](https://github.com/cfcromn/Throt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Resource-aware scheduler and TUI for local AI coding agents.**

> Run more coding agents without melting your machine.

Throt is a local-first Go daemon and terminal UI that observes coding-agent process trees, detects resource pressure, and coordinates expensive child workloads so you can keep multiple agents productive without making the machine unusable.

## Status

**Pre-alpha.** The daemon samples host resources, reports a pressure level, and attributes processes and heavy workloads to coding agents. The TUI and scheduling are under active development. Expect breaking changes before 1.0.

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

## Planned UX

```text
$ throt

SYSTEM
CPU    ███████████████░░░  74%
MEM    █████████████████░░  82%
SWAP   ███░░░░░░░░░░░░░░  1.4 GB
PRESSURE: HIGH

AGENTS
AGENT       CPU     MEM      CHILDREN   STATE
orca-1      4.1%    1.1 GB   7          RUN
codex-1     7.8%    0.9 GB   5          RUN
claude-1    2.2%    1.0 GB   4          RUN

HEAVY WORKLOADS
OWNER       PROCESS          STATE       REASON
codex-1     gradle test      RUNNING
orca-1      docker build     RUNNING
claude-1    npm build        QUEUED      memory pressure
```

## Usage

Requires Go (see `go.mod`) on macOS or Linux.

```bash
go install github.com/cfcromn/Throt/cmd/throt@latest
go install github.com/cfcromn/Throt/cmd/throtd@latest

throtd &        # start the daemon
throt ping      # check that the client can reach it
throt status    # pressure, host resources, agents, heavy workloads
throt events    # recent daemon events as JSON lines
```

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
