# Throt

[![CI](https://github.com/cfcromn/Throt/actions/workflows/ci.yml/badge.svg)](https://github.com/cfcromn/Throt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Resource-aware scheduler and TUI for local AI coding agents.**

> Run more coding agents without melting your machine.

Throt is a local-first Go daemon and terminal UI that observes coding-agent process trees, detects resource pressure, and coordinates expensive child workloads so you can keep multiple agents productive without making the machine unusable.

## Status

**Pre-1.0 (v0.7; v0.8 next).** Throt samples host resources, foreground latency and kernel pressure signals, reports a stable pressure level, attributes processes and heavy workloads to coding agents, and shows it all in a TUI and over MCP. Manual pause/resume, an opt-in adaptive heavy-slot scheduler (`observe` / `enforce`), and `throt run` with reserved slots are available. Expect breaking changes before 1.0; the compatibility policy from 1.0 on is in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#compatibility).

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
SYSTEM  pressure HIGH  latency 4ms ▁▁▂▁▁▃▂▁▁▁▂▁▁▁▂▁
  why: memory 90%+ used  (memory_used>=0.9)
CPU   ██████████████████████████████░░░░░░░░░░   74%  ▃▄▄▅▆▆▇▆▅▆▆▇▆▅▆▆  load 1.12/cpu
MEM   ██████████████████████████████████████░░   95%  ▆▆▆▆▇▇▇▇▇▇▇▇▇███  30.5 GiB / 32.0 GiB
SWAP  ██████████████░░░░░░░░░░░░░░░░░░░░░░░░░░   35%  ▁▁▁▁▁▂▅█▃▁▁▁▁▁▁▁  1.4 GiB / 4.0 GiB
▸ AGENTS (3)
  AGENT                                 PID    CPU▼        MEM  CHILDREN
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

q quit  tab focus  j/k move  enter open  s sort  ? help
```

CPU is a percentage of one core, so a process using two cores shows 200%. The line under `SYSTEM` says in plain words which signal drives the pressure level. Sparklines show the last 60 polls of CPU, memory used, swap growth (on the `SWAP` line) and foreground latency (in the header); the TUI keeps this history itself and loses it when it exits or loses `throtd`.

Keys:

- `tab` switches between the agents and heavy-workload tables; `j`/`k` or the arrow keys move the selection.
- `enter` on an agent opens its drilldown: the agent's PID, CPU, memory and children, and every heavy workload it owns with its state (running, `PAUSED`, `QUEUED`). `esc` goes back. On a heavy workload, `enter` toggles its details.
- `p`/`r` pause or resume the selected heavy workload, in the heavy table or a drilldown, after a `y/n` confirmation.
- `s` cycles the table's sort column: CPU, memory, name, then throtd's order. The sorted column is marked `▼` (descending) or `▲` (ascending).
- `?` shows help, `q` quits.

Metrics the platform cannot provide show as `n/a`, and if `throtd` is not running the TUI says so and reconnects when it starts. Narrow terminals drop the sparklines first (below 55 columns), then shrink the bars, drop low-value columns and truncate names; below 40×12 the TUI asks for a larger window.

## Install

Download a release archive for macOS or Linux (amd64/arm64) from [Releases](https://github.com/cfcromn/Throt/releases), verify it (build provenance for `gh attestation verify` is published only while the repository is public), and put `throt` and `throtd` on your `PATH`:

```bash
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
gh attestation verify throt_<version>_<os>_<arch>.tar.gz --repo cfcromn/Throt
```

On macOS the release binaries are not signed or notarized, so Gatekeeper refuses to run them after a browser download. Remove the quarantine attribute once: `xattr -d com.apple.quarantine throt throtd`. Archives fetched with `gh release download` or `curl` don't get that attribute.

Or build from source with the Go version in `go.mod`:

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

```bash
throt update --check             # current and latest version; exit 0 when up to date, 10 when an update is available
throt update                     # download, verify, and replace throt and throtd (asks first; --yes skips the question)
throt update --version v0.8.0-rc.1 # install a specific release, newer or older: also the rollback
throt update --pre               # include prereleases (the default when the running version is one)
```

`throt update` finds the running `throt` (symlinks resolved) and the `throtd` next to it, asks the GitHub API for the newest matching release, downloads the archive for this OS and architecture with `checksums.txt`, and refuses unless its SHA-256 matches. It then writes both new binaries next to the old ones, runs each with its version flag, and only when both report the expected version renames them into place, `throtd` first and `throt` last; any earlier failure leaves the old binaries untouched. When the directory isn't writable it changes nothing and says so; re-run it as a user who can write there. Without `--yes` it asks on a terminal and refuses when stdin is not one. Binaries from `go install` and development builds are left alone: it prints the `go install` commands instead. On macOS the files it writes carry no quarantine attribute, so no `xattr` step is needed.

The repository is private for now, so `throt update` needs a token: `GH_TOKEN` or `GITHUB_TOKEN`, otherwise `gh auth token` when the [GitHub CLI](https://cli.github.com) is installed and logged in (`gh auth login`). It uses the network only when you run it; no Throt command checks for updates on its own.

A running `throtd` keeps the old version until restarted, and `throt update` says so without restarting it: `launchctl kickstart -k gui/$(id -u)/dev.throt.throtd` (launchd), `systemctl --user restart throtd` (systemd), or stop and start it by hand. Stopping `throtd` resumes everything it paused.

To upgrade by hand instead, stop `throtd` (this resumes any paused work), replace both binaries from a verified release archive, and start it again. To roll back by hand, install the previous release the same way. Configuration is backward compatible within a minor series, and unknown keys are reported as errors rather than silently ignored. `throt` and `throtd` must be the same release, and a mismatch is reported with both protocol versions. Published versions are never rewritten; fixes ship as new patch releases.

## Usage

```bash
throtd &        # start the daemon (throtd -version prints its version)
throt           # terminal UI (same as `throt top`)
throt ping      # check that the client can reach it
throt status    # pressure, host resources, agents, heavy workloads, stopped processes
throt stats     # per-agent CPU time, peak memory, heavy jobs, time queued since throtd started (--json)
throt events    # recent daemon events as JSON lines
throt events -f # keep printing new events until interrupted
throt explain PID # why a heavy workload is running, paused, or queued
throt doctor    # sanitized diagnostics to paste into a bug report, incl. stopped processes
throt pause PID # pause a heavy workload owned by one of your agents
throt resume PID          # resume it; also an agent's job left stopped (see below)
throt resume --force PID  # resume any other stopped process of yours, e.g. a Ctrl-Z job
throt reload    # re-read the config without restarting (or send throtd SIGHUP)
throt config check        # validate the config file, show what it changes, warn about risky settings
throt config show         # effective config and where each value came from (--json)
throt version --json      # client, protocol, Go, and throtd versions
throt run -- npm run build    # wait for a free heavy slot, then run the command
throt completion zsh          # shell completion script (bash, zsh, fish)
throt update --check          # is a newer release out? (see Upgrade and rollback)
```

Global flags go before the command: `-config PATH` uses another config file (default below), and `-n N` sets how many events `throt events` prints (default 50). `-h` on `throt`, `throtd`, or a subcommand prints its usage and exits 0.

Both programs find `throtd`'s socket the same way: `socket_path` from the config, otherwise `$XDG_RUNTIME_DIR/throt/throtd.sock`, or, without `XDG_RUNTIME_DIR` (as usual on macOS), `throt-UID/throtd.sock` in the OS temp directory (`$TMPDIR`, else `/tmp`). Give both the same config and environment; `throt doctor` prints the path in use.

### Stopped processes throtd did not pause

A process can be left stopped (SIGSTOP) with no record of it: a `throtd` killed hard before it wrote `paused.json`, a deleted state file, or a stray daemon. A shell job suspended with Ctrl-Z is stopped too, so Throt only reports these processes and never resumes one on its own.

- `throt status` prints `STOPPED   3 processes not paused by throtd (see throt doctor)` when there are any.
- `throt doctor` has an `orphans` section with their count and, for up to 50 of them, the PID, executable base name (never arguments or paths), owning agent, how long they have been observed stopped, and a hint: `likely job control (Ctrl-Z)` when the parent is your shell (`zsh`, `bash`, `fish`, `sh`), or `likely left paused by an earlier throtd` for an agent's process or a heavy workload. With `throtd` down, `doctor` looks for them itself.
- `throtd` logs one `orphan_stopped` event when a heavy workload of an agent is newly found stopped this way. Shell jobs get no event.
- `throt resume PID` resumes such a workload (its stopped processes, children first, each verified as with any resume). Anything else, a shell job included, needs `throt resume --force PID`; for a shell job, `fg` in that shell is usually better. `kill -CONT PID` always works too.

### Wait before starting: `throt run`

`throt run [--timeout D] [--no-wait] -- CMD [ARGS...]` is the gentle alternative to being paused: it asks for a heavy slot *before* the command starts, using the same verdict as `throt_capacity`. It polls `throtd` once per second until a heavy job is OK to start (pressure below high, a free heavy slot, nothing queued, and the calling agent within its `per_agent_limit`), printing a notice to stderr at most every 10s. Then it execs the command in its own place, so the PID, signals, and exit status are the command's own. The agent is the nearest agent process among its ancestors. A missing command exits 127; one that cannot be executed exits 126, and so does one found only through `.` in `PATH`, which `throt run` refuses unlike a shell.

The slot is reserved: `throtd` grants it as a lease, so two `throt run` commands asking at the same moment cannot both take the last slot, and the scheduler does not hand that slot to another newcomer. The lease ends when the command shows up as a heavy workload (which then holds the slot itself), when it exits, or after 60s if it never becomes heavy. A `throt run` inside work that already holds a slot (a line in a make recipe or npm script run under `throt run`, or below a running heavy build) starts at once without reserving another. With `scheduler.mode` off there are no slots, so only pressure makes it wait. `throt status` shows reserved slots (`2 reserved`).

- `--timeout D` stops waiting after `D` and runs the command anyway (default 5m; 0 waits forever).
- `--no-wait` asks once (taking the slot if one is free), prints the verdict, and runs regardless.

It fails open: if `throtd` is unreachable, it prints one notice and runs immediately, so a stopped daemon never blocks your work. It never pauses anything. Against an older `throtd` without leases it falls back to advice only, where two commands checking at the same moment can both start.

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

### Claude Code integration

`throt setup claude-code` prints a `settings.json` hook and the MCP server config to paste; `throt setup codex` prints the Codex `config.toml` block and an `AGENTS.md` paragraph. Neither writes any file.

The hook (`throt hook claude-code --max-wait D`, a [`PreToolUse`](https://code.claude.com/docs/en/hooks) hook on `Bash`) makes Claude Code wait for capacity without being told. When a Bash command is a single heavy tool (by the same names Throt classifies as heavy workloads), optionally after `cd DIR &&`, variable assignments, or `time`, it polls `throtd` once per second for the `throt_capacity` verdict of the calling agent and returns when a heavy job is OK to start or after `D` (default 50s; the setup snippet uses 110s with a 120s hook `timeout`, since a PreToolUse hook that times out blocks the call). It never changes, approves, or denies the command, so it runs exactly as written under your own permission rules. Commands with pipes, `;`, `||`, `&`, redirections, subshells, substitutions, heredocs, or comments, a heavy tool after another command (`make && ./run`), `throt run` itself, and input it cannot parse pass at once without contacting `throtd`; so does everything when `throtd` is down. After it waited it shows you a one-line `systemMessage`; it prints nothing else.

This is advisory admission: the hook exits before the command starts, so it reserves no slot, and two commands let through at the same moment can both start. Prefix a command with `throt run --` for a reserved slot. Do not add permission allow rules for `throt run`: such a rule would allow every command run through it.

## Configuration

Optional, at `~/.config/throt/config.json` (or `$XDG_CONFIG_HOME/throt/config.json`; `-config PATH` on either program overrides it). Unknown keys are errors.

```json
{
  "sample_interval": "2s",
  "profile": "balanced",
  "scheduler": {
    "mode": "observe",
    "heavy_limit": 8,
    "start_limit": 2,
    "adaptive": true,
    "per_agent_limit": 0,
    "resume_cooldown": "10s",
    "pause_on_critical": false,
    "max_queue_wait": "5m"
  }
}
```

Three keys apply only when `throtd` starts; `throt reload` reports a change to them as `requires restart`:
- `socket_path`: the daemon's Unix socket (default: see [Usage](#usage)).
- `sample_interval`: how often metrics are sampled (default `2s`, at least `100ms`).
- `event_history`: how many recent events `throtd` keeps in memory for `throt events` (default 1000, 1 to 100000).

Custom agents can be added with `"agents": [{"name": "myagent", "executables": ["my-agent"]}]`. Generic runtimes such as `node` and build tools such as `go` are refused, because they would match unrelated processes. On Linux, executable names are truncated to 15 characters.

`profile` presets the scheduler; explicit keys override it.
- `performance`: a fixed limit of CPUs/2 (at least 2), not adaptive.
- `balanced` (default): adaptive, starting at CPUs/4 (at least 2) and growing up to one per CPU (`heavy_limit` = CPUs, 2 to 64).
- `cool`: limit 1, adaptive, may pause one workload under critical pressure.

`heavy_limit` is the fixed limit, or with `adaptive` the ceiling. `start_limit` is where the adaptive limit starts (0 to `heavy_limit`, ignored without `adaptive`). Left out or 0, it comes from the profile, capped at `heavy_limit`, so a file that sets only `heavy_limit` keeps working.

A profile never turns the scheduler on.
- `adaptive` starts the running limit at `start_limit`, halves it under high pressure and raises it by one per 30s, up to `heavy_limit`, while the machine copes: at normal pressure, or at elevated pressure caused only by busy CPUs (CPU, load, CPU PSI) with memory, swap and latency calm. Elevated pressure from memory, swap or latency holds it.
- `per_agent_limit` caps heavy workloads per agent, so one agent can't take every slot.

`scheduler.mode` is `off` (default), `observe` (log what it would do as `scheduler_decision` events), or `enforce` (queue new agent-owned heavy workloads with SIGSTOP when `heavy_limit` heavy workloads are already running, and resume them as slots free up). Try `observe` first and watch `throt events`.

Check a file before `throt reload` or a restart with `throt config check [--config PATH]`. It loads the file exactly as throtd does, prints `ok` and each value that differs from the defaults (marked `file` or `profile`), and exits 1 with the error for an invalid or missing file. It also warns, without failing, about `enforce` with a fixed (not adaptive) `heavy_limit` of at least the CPU count, `pause_on_critical` with mode `off` (no effect), and a custom agent executable that shadows a built-in adapter. `throt config show [--json]` prints the effective config (defaults, then the profile, then the file), the profile applied, and the source of every value: `default`, `profile`, or `file`.

throtd logs to stderr. Its flags are `-config PATH`, `-version`, `-log-format text|json` (default `text`) and `-log-level debug|info|warn|error` (default `info`). `info` stays quiet in steady state: daemon lifecycle, agents appearing and leaving, pressure changes, scheduler decisions and control actions. `debug` adds each heavy workload start and exit and each admission lease; failures and processes left stopped are `warn`. The event log (`throt events`) records everything regardless of the log level.

## Roadmap

Outcomes, not dates.

| Version | Goal | Highlights | Status |
|---|---|---|---|
| v0.1 Observe | "What is making my machine slow, and which agent owns it?" | host metrics, process graph, agent attribution, TUI, event timeline, `throt doctor` | shipped |
| v0.2 Control | Reduce overlapping heavy workloads without losing work | manual pause/resume, heavy-slot scheduler (opt-in), audit events, crash recovery | shipped |
| v0.3 Adaptive | Keep the machine responsive automatically | latency signal, adaptive limit, per-agent budgets, profiles | shipped |
| v0.4 Ecosystem | Integrate everywhere | Linux, documented local API, MCP server, custom agent adapters | shipped |
| v0.5 Accurate pressure | Accurate pressure everywhere, and hardening | cgroup-aware Linux, macOS kernel verdict, evidence-driven CPU policy, hot reload, `throt explain`, fuzzing, pinned supply chain | shipped |
| v0.6 Cooperative admission | Let agents wait instead of being paused | `throt run`, `throt stats`, Linux PSI, TUI drilldown and sparklines, real-binary e2e tests | shipped |
| v0.7 Admission leases | Race-free admission, no hidden stopped work | admission leases for `throt run`, reporting of stopped processes Throt did not pause | shipped |
| v0.8 Integration and readiness | Wait for capacity without being told | Claude Code hook, `throt setup`, `throt config check`/`show`, `throt version --json`, log levels, soak test | next |
| 1.0 | Turn the scheduler on confidently under an explicit profile | stable compatibility policy, real-machine benchmarks; the scheduler stays opt-in, as the benchmark evidence decided | planned |

**1.0** means the scheduler is ready to turn on with confidence under an explicit `scheduler.mode` and `profile`; it stays off by default (see "Default decision" in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#results)). It requires two supported OS families, the [compatibility policy](docs/ARCHITECTURE.md#compatibility) in force, no known process-loss bug, reproducible benchmarks, and a security review of IPC and process-control boundaries.

## Non-goals

Throt is not another coding agent, a replacement for Orca/Claude Code/Codex/OpenCode, a container orchestrator, a cloud scheduler, or an excuse to kill processes without explainable policy.

## Documentation

- [Architecture & design](docs/ARCHITECTURE.md)
- [Contributing & development](CONTRIBUTING.md)
- [Security & privacy](SECURITY.md)
- [Changelog](CHANGELOG.md)

## License

[MIT](LICENSE)
