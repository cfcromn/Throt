# Changelog

All notable user-visible changes to Throt are documented here.

The format is inspired by Keep a Changelog and versions follow Semantic Versioning.

## [Unreleased]

### Changed
- Memory used still raises pressure to critical (≥ 95%) while a kernel memory signal (macOS verdict or Linux PSI) is present, so memory exhaustion without swap is never hidden.
- `throt run` waits at most 5m by default (`--timeout 0` waits forever).
- Linux memory PSI is read from the same cgroup level as the memory figures (least headroom).

## [0.5.0-rc.1] - 2026-10-28

Accurate pressure everywhere: cgroup-aware Linux, the macOS kernel verdict, and a CPU policy driven by benchmark evidence. Also hardening: fuzzing, a patched toolchain, and a pinned supply chain.

### Added
- Run-at-login (launchd, systemd) and upgrade/rollback instructions.
- `internal/e2e`: end-to-end tests of the built `throtd` and `throt` binaries against real processes (observe, manual pause/resume, crash recovery, graceful shutdown, enforce scheduler and reload, `throt mcp`), run on Linux CI with `THROT_E2E=1`.
- `make bench`: containerized real-kernel benchmark (baseline vs enforce), with results in `docs/ARCHITECTURE.md`.
- macOS kernel memory-pressure verdict (`kern.memorystatus_vm_pressure_level`) as a pressure signal (warning → elevated, critical → high), shown in `throt status` and the TUI.
- Linux PSI (`/proc/pressure`, or the limiting cgroup's `memory.pressure`/`cpu.pressure`) as pressure signals: memory some ≥ 10% → elevated, full ≥ 5% → high, full ≥ 20% → critical; I/O full ≥ 20% → elevated, ≥ 50% → high; CPU some ≥ 50% → elevated. Memory PSI takes precedence over memory used, like the macOS verdict. Shown in `throt status` and the TUI system line when available.
- More heavy tools classified: ninja, cmake, swiftc, kotlinc, dotnet, msbuild, zig, ghc, scalac, sbt, bazelisk, buck2, turbo, nx, rollup, webpack-cli, next, nuxt, rspack (build); rspec, mocha, playwright, cypress, phpunit, ctest (test); buildctl, buildah, kaniko (container build).
- Hot config reload via `SIGHUP` or `throt reload` (privileged `reload` method): applies `scheduler.*`, `profile` and `agents` live, reports restart-only keys, and changes nothing on an invalid file.
- Events carry a `seq`, and the `events` method takes an optional `since` for incremental polling (protocol stays v1).
- `throt events -f` follows new events, and notices when throtd restarts.
- `throt explain PID`: why a heavy workload is running, paused, or queued, with its recent events.
- `throt stats` (and `--json`), the `stats` protocol method, and the `throt_stats` MCP tool: per-agent CPU time, peak memory, heavy jobs started, and time queued by the scheduler since throtd started, with all-agent and unattributed totals. Kept in memory for at most 256 agent instances.
- TUI: `p`/`r` pause or resume the selected heavy workload after a `y/n` confirmation. The timeline now fetches only new events.
- `throt run [--timeout D] [--no-wait] -- CMD`: cooperative admission control. Waits until throtd reports capacity for a heavy job (the `throt_capacity` verdict, for the nearest agent ancestor), then execs the command in place. Advisory and fail-open: it never pauses anything, runs immediately when throtd is unreachable, and reserves nothing, so two runs checking at once can both start.
- `throt completion bash|zsh|fish` prints a shell completion script; PIDs complete after `pause`, `resume`, and `explain`.
- TUI: sparklines of the last 60 polls for CPU, memory, swap growth and latency; a plain-words line naming the signal behind the pressure level; an agent drilldown (`enter` on an agent) listing its heavy workloads with their state, where `p`/`r` work too; `s` sorts each table by CPU, memory or name.

### Changed
- LICENSE copyright holder romanized.
- Linux host pressure is cgroup v2-aware: CPU (including throttling), memory and swap are measured against the tightest `cpu.max`, `memory.max` and `memory.swap.max` limits of the daemon's cgroup and its ancestors, falling back to host-wide figures per metric; the host-wide load average is reported missing under a CPU quota.
- CPU and load alone now cap pressure at elevated; high and critical come from memory, swap, and foreground latency. Saturated build cores no longer halve the adaptive limit.
- On macOS, memory used no longer drives pressure while the kernel verdict is available; it sat at 80–90% on idle machines and held pressure at elevated.

### Fixed
- Parsers reject overflowing, negative, NaN, and control-character input found by fuzzing (ps, procfs, loadavg, meminfo); MCP matches JSON-RPC member names exactly and rejects non-scalar ids.

### Security
- Built with Go 1.26.10, which fixes 20 standard-library vulnerabilities present in 1.26.4 (none reachable from Throt's code according to govulncheck).
- GitHub Actions pinned to commit SHAs; `govulncheck` runs in CI.
- Native fuzz tests for every parser and decoder of untrusted input (15 targets).

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
