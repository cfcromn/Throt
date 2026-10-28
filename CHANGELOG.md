# Changelog

All notable user-visible changes to Throt are documented here.

The format is inspired by Keep a Changelog and versions follow Semantic Versioning.

## [Unreleased]

### Added
- Run-at-login (launchd, systemd) and upgrade/rollback instructions.
- `make bench`: containerized real-kernel benchmark (baseline vs enforce), with results in `docs/ARCHITECTURE.md`.
- macOS kernel memory-pressure verdict (`kern.memorystatus_vm_pressure_level`) as a pressure signal (warning → elevated, critical → high), shown in `throt status` and the TUI.
- More heavy tools classified: ninja, cmake, swiftc, kotlinc, dotnet, msbuild, zig, ghc, scalac, sbt, bazelisk, buck2, turbo, nx, rollup, webpack-cli, next, nuxt, rspack (build); rspec, mocha, playwright, cypress, phpunit, ctest (test); buildctl, buildah, kaniko (container build).

### Changed
- LICENSE copyright holder romanized.
- On macOS, memory used no longer drives pressure while the kernel verdict is available; it sat at 80–90% on idle machines and held pressure at elevated.

## [0.4.0-rc.2] - 2026-10-28

Adaptive scheduling and agent integration. The scheduler remains opt-in. (The `v0.4.0-rc.1` tag was never released: a flaky test failed the release build.)

### Added
- Foreground latency probe (timer lateness) as a pressure signal; shown in `throt status` and the TUI.
- Custom agent adapters via the `agents` config key.
- Documented local protocol (methods, params, versioning) in `docs/ARCHITECTURE.md`.
- Adaptive (AIMD) heavy limit, per-agent heavy budgets, and `performance` / `balanced` / `cool` profiles.
- `throt mcp`: a read-only stdio MCP server (`throt_status`, `throt_capacity`, `throt_events`) so coding agents can check pressure and heavy-slot capacity before starting heavy work.

## [0.2.0-rc.1] - 2026-10-28

First control release candidate. Automatic scheduling is opt-in and off by default.

### Added
- `throt pause PID` / `throt resume PID`: identity-verified SIGSTOP/SIGCONT of agent-owned heavy workloads, with audit events, peer-uid authorization, resume on shutdown, and crash recovery.
- Paused state in `throt status` and the TUI.
- Opt-in heavy-slot scheduler (`scheduler.mode`: `off` / `observe` / `enforce`) with pressure-aware queueing, owner fairness, aging, cooldowns, and a `scheduler_decision` event for every decision.

### Changed
- Heavy descendants (compilers, linkers) now fold into their topmost heavy ancestor, so a build is one workload.

## [0.1.0-rc.1] - 2026-10-28

First observe-only release candidate. Throt does not control any process.

### Added
- Repository foundations: architecture, contribution, and security policies; CI.
- `throtd` daemon skeleton with versioned local IPC and `throt ping`.
- Host metrics sampling (macOS, Linux) and a hysteresis-based pressure engine; `throt status`.
- Process graph with agent attribution (Claude Code, Codex, OpenCode, Gemini, Aider, Cursor, Goose, Orca) and heavy-workload detection.
- Terminal UI (`throt` / `throt top`) built on Bubble Tea.
- `throt doctor` sanitized diagnostics.
- Release automation: macOS/Linux archives, checksums, and SBOMs via GoReleaser.
