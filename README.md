# Throt

[![CI](https://github.com/cfcromn/Throt/actions/workflows/ci.yml/badge.svg)](https://github.com/cfcromn/Throt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Resource-aware scheduler and TUI for local AI coding agents.**

> Run more coding agents without melting your machine.

- [What it is](#what-it-is)
- [Quickstart](#quickstart)
- [Install and update](#install-and-update)
- [Using Throt](#using-throt)
- [Configuration](#configuration)
- [How it decides](#how-it-decides)
- [Troubleshooting](#troubleshooting)
- [Roadmap](#roadmap)
- [Contributing and license](#contributing-and-license)

## What it is

Several coding agents at once mostly wait on model responses, but their child processes collide: Gradle/Maven/Go/npm builds, Docker builds and Compose stacks, test suites, language servers, local databases. The result is CPU saturation, memory pressure, swap growth and a sluggish foreground.

Throt is a local Go daemon (`throtd`) and client (`throt`: CLI, terminal UI, MCP server). It watches the process trees of your coding agents, attributes heavy workloads (builds, tests, container builds) to the agent that started them, measures resource pressure, and, if you turn it on, limits how many heavy workloads run at once. It optimizes developer responsiveness and useful throughput, not a low CPU number.

Principles:
- **Observe before controlling.** The scheduler is off by default; turn it on in `observe` mode first.
- **No agent lock-in.** Agents are recognized by adapters; Claude Code, Codex, OpenCode, Gemini, Aider, Cursor Agent, Goose, Amp, GitHub Copilot CLI and Orca are built in, and anything else gets a one-line custom rule (see [An agent is not detected](#an-agent-is-not-detected)).
- **Local-first.** No telemetry. The only network access is `throt update`, when you run it.
- **Fail open.** If Throt crashes or is stopped, agents and their work continue.
- **Explain every intervention.** Every queue, pause and resume has a reason you can read.

Throt is not a coding agent, a container orchestrator, or a cloud scheduler, and it never kills processes: it only pauses and resumes them.

**Status: pre-1.0.** The latest release is v0.9.0-rc.1, a release candidate. macOS and Linux (amd64/arm64). Expect breaking changes before 1.0; the policy from 1.0 on is in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#compatibility).

## Quickstart

1. **Install** the newest release for your platform (`darwin_arm64`, `darwin_amd64`, `linux_amd64` or `linux_arm64`) with the [GitHub CLI](https://cli.github.com), verify it, and put both binaries on your `PATH`:

   ```bash
   V=$(gh release list -R cfcromn/Throt -L 1 --json tagName -q '.[0].tagName')
   gh release download "$V" -R cfcromn/Throt -p 'throt_*_darwin_arm64.tar.gz' -p checksums.txt
   shasum -a 256 --ignore-missing -c checksums.txt   # Linux: sha256sum --ignore-missing -c checksums.txt
   tar -xzf throt_*_darwin_arm64.tar.gz throt throtd
   mkdir -p ~/.local/bin && mv throt throtd ~/.local/bin/   # any directory on your PATH
   ```

2. **Start the daemon** in its own terminal (or [at login](#run-at-login)). It only observes until you turn the scheduler on.

   ```bash
   throtd
   ```

3. **Look around** from another terminal:

   ```bash
   throt          # terminal UI: pressure, agents, heavy workloads; q quits
   throt status   # the same as plain text
   throt doctor   # diagnostics: socket path, config, daemon, missing metrics
   ```

4. **Turn on the scheduler in observe mode.** It logs what it would do as `scheduler_decision` events and acts on nothing:

   ```bash
   mkdir -p ~/.config/throt
   echo '{"scheduler": {"mode": "observe"}}' > ~/.config/throt/config.json
   throt config check   # ok, and: scheduler.mode off -> observe (file)
   throt reload         # applied: scheduler.mode off -> observe
   throt events -f      # watch; decisions appear when agents' heavy jobs compete for slots
   ```

5. **Enforce** once the decisions look right: set `"mode": "enforce"` in the file, then run `throt config check` and `throt reload` again. New heavy workloads beyond the limit are now paused (SIGSTOP) until a slot frees up. Stopping `throtd` resumes everything it paused.

## Install and update

### Install

Release archives for macOS and Linux (amd64/arm64) are on [Releases](https://github.com/cfcromn/Throt/releases); the [Quickstart](#quickstart) shows the download. The repository is private for now, so downloads need a GitHub login (`gh auth login`).

Verify an archive against the release's `checksums.txt`:

```bash
shasum -a 256 --ignore-missing -c checksums.txt   # Linux: sha256sum --ignore-missing -c checksums.txt
gh attestation verify throt_<version>_<os>_<arch>.tar.gz --repo cfcromn/Throt   # only once the repository is public
```

Build provenance for `gh attestation verify` is published only while the repository is public.

**macOS Gatekeeper.** The binaries are not signed or notarized. After a browser download macOS refuses to open them; remove the quarantine attribute once with `xattr -d com.apple.quarantine throt throtd`. Archives fetched with `gh release download` or `curl`, and binaries installed by `throt update`, don't get that attribute.

To build from source with the Go version in `go.mod`:

```bash
GOPRIVATE='github.com/cfcromn/*' go install github.com/cfcromn/Throt/cmd/throt@latest
GOPRIVATE='github.com/cfcromn/*' go install github.com/cfcromn/Throt/cmd/throtd@latest
```

While the repository is private, `go install` needs both of these:
- `GOPRIVATE='github.com/cfcromn/*'` (or `go env -w GOPRIVATE='github.com/cfcromn/*'` once), so Go fetches the module directly instead of through the public module proxy and checksum database, which cannot see it;
- Git credentials that can read the repository, because Go fetches it with `git`. With the GitHub CLI logged in, `gh auth setup-git` sets them up.

Without them `go install` fails, typically with `not found` from the proxy or `terminal prompts disabled` from Git.

`throt` and `throtd` must come from the same release.

### Update and roll back

| Command | What it does |
|---|---|
| `throt update --check` | Print the current and latest version. Exit 0 when up to date, 10 when an update is available. |
| `throt update` | Download, verify and replace `throt` and `throtd`. Asks first; `--yes` skips the question. |
| `throt update --version v0.9.0-rc.1` | Install a specific release, newer or older (this is the rollback). |
| `throt update --pre` | Include prereleases (the default when the running version is one). |

`throt update` finds the running `throt` (symlinks resolved) and the `throtd` next to it, downloads the archive for this OS and architecture with `checksums.txt`, and refuses unless the SHA-256 matches. It runs both new binaries to check their version, then renames them into place, `throtd` first and `throt` last. Any earlier failure, or a directory you can't write, leaves the old binaries untouched. Without `--yes` it refuses when stdin is not a terminal. Binaries from `go install` and development builds are left alone; it prints the `go install` commands instead.

It needs a GitHub token while the repository is private: `GH_TOKEN` or `GITHUB_TOKEN`, otherwise `gh auth token` when the GitHub CLI is logged in. No Throt command checks for updates on its own.

**Restart `throtd` after an update.** The running daemon keeps the old version, and `throt update` prints the command to restart it: `launchctl kickstart -k gui/$(id -u)/dev.throt.throtd` (launchd), `systemctl --user restart throtd` (systemd), or stop it and start it again. Stopping `throtd` resumes everything it paused.

To update by hand, stop `throtd`, replace both binaries from a verified archive, and start it again. Published versions are never rewritten; fixes ship as new releases. Configuration is backward compatible within a minor series.

### Run at login

`throtd` resumes anything it paused when it gets SIGTERM or SIGINT, so a service manager can stop it safely.

**macOS (launchd):** save as `~/Library/LaunchAgents/dev.throt.throtd.plist`, replace both paths with your own (launchd does not expand `~`), then run `launchctl load ~/Library/LaunchAgents/dev.throt.throtd.plist`. The plist needs no environment variables for the socket: a launch agent in your login session gets the same per-user `TMPDIR` as your shell (the `confstr(_CS_DARWIN_USER_TEMP_DIR)` directory under `/var/folders`), and neither sets `XDG_RUNTIME_DIR`, so both use the same default socket.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.throt.throtd</string>
  <key>ProgramArguments</key><array><string>/Users/you/.local/bin/throtd</string></array>
  <key>StandardErrorPath</key><string>/Users/you/Library/Logs/throtd.log</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
```

**Linux (systemd user unit):** save as `~/.config/systemd/user/throtd.service`, then run `systemctl --user enable --now throtd` (logs: `journalctl --user -u throtd`). The systemd user manager gives the unit `XDG_RUNTIME_DIR=/run/user/UID`, the value `pam_systemd` also sets for your login sessions, so both use the same socket there.

```ini
[Unit]
Description=Throt resource-aware scheduler for coding agents

[Service]
ExecStart=%h/.local/bin/throtd
Restart=on-failure

[Install]
WantedBy=default.target
```

If `throt` still cannot reach a `throtd` started at login (your shell sets its own `XDG_RUNTIME_DIR` or `TMPDIR`, or you work over SSH, `su` or `sudo`), set an absolute `socket_path` in the config file rather than more environment variables: `throtd` and every client read the same file, including a `throt mcp` started by an app that never reads your shell's rc files. See ["cannot reach throtd"](#cannot-reach-throtd).

## Using Throt

### Commands

| Command | What it does |
|---|---|
| `throt`, `throt top` | Terminal UI. Quitting it never affects `throtd`. |
| `throt status [--json]` | Pressure and its reason, host resources, agents, heavy workloads, scheduler state, stopped processes. |
| `throt stats [--json]` | Per agent: CPU time, peak memory, heavy jobs and time queued since `throtd` started. |
| `throt events [-f] [-n N]` | Recent daemon events as JSON lines (default 50); `-f` keeps printing new ones. |
| `throt explain PID` | Why a heavy workload is running, paused or queued. |
| `throt pause PID` | Pause (SIGSTOP) a heavy workload owned by one of your agents. |
| `throt resume [--force] PID` | Resume it. Also resumes stopped processes Throt did not pause; see [Troubleshooting](#a-process-is-stuck-stopped-state-t). |
| `throt run [--timeout D] [--no-wait] -- CMD` | Wait for a free heavy slot, then run `CMD`. See [`throt run`](#wait-before-starting-throt-run). |
| `throt doctor` | Sanitized diagnostics to paste into a bug report. Works without `throtd`. |
| `throt ping` | Check that `throtd` is reachable; prints its version and protocol. |
| `throt reload` | Make `throtd` re-read its config (same as sending it SIGHUP). |
| `throt config check` | Validate the config file, show what it changes, warn about risky settings. |
| `throt config show [--json]` | The effective config and where each value came from. |
| `throt version [--json]` | Client version; `--json` adds the protocol, Go and `throtd` versions. |
| `throt update` | Install a newer (or older) release. See [Update and roll back](#update-and-roll-back). |
| `throt mcp` | Read-only MCP server on stdio for coding agents. |
| `throt setup claude-code`, `throt setup codex` | Print the configuration to paste into a coding agent. |
| `throt hook claude-code [--max-wait D]` | The Claude Code hook. |
| `throt completion bash\|zsh\|fish` | Shell completion script. |

Global flags go before the command: `-config PATH` uses another config file, and `-n N` sets how many events `throt events` prints. `throt -h` lists every command, and `-h` after any command prints that command's usage. `throtd -h` lists the daemon's flags, and `throtd -version` prints its version.

Exit codes: 0 on success, 1 on an error, 2 on a usage error, 10 from `throt update --check` when an update is available; `throt run` exits with the command's status (126, 127 when it cannot run). `throt hook claude-code` always exits 0. The JSON outputs, event kinds, reason codes and exit codes are listed under [Compatibility](docs/ARCHITECTURE.md#compatibility).

Shell completion: add `source <(throt completion zsh)` (or `bash`) to your shell's rc file, or run `throt completion fish | source`. It completes heavy-workload PIDs after `pause`, `resume` and `explain`.

### Terminal UI

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

The `why:` line names the signal behind the pressure level. CPU is a percentage of one core (two busy cores show 200%). Sparklines show the last 60 refreshes (one per second) of CPU, memory, swap growth and foreground latency; the TUI keeps that history itself and loses it when it exits. Metrics the platform cannot provide show as `n/a`. If `throtd` is not running, the TUI says so and reconnects when it starts. Narrow terminals drop the sparklines first (below 55 columns), then columns; below 40×12 it asks for a larger window.

When the scheduler is on, a `SCHEDULER` line shows its mode, the current adaptive limit with its start and ceiling, the per-agent cap, and queued and reserved counts; a `STOPPED` line counts processes throtd did not pause (see `throt doctor`). Timeline events are tagged `CTL`, `SCHED` and `FAIL`. Every state is shown in text as well as color, and `NO_COLOR` or `TERM=dumb` turns color off entirely.

| Key | Action |
|---|---|
| `tab` | Switch between the agents and heavy-workload tables. |
| `j`/`k`, `↓`/`↑` | Move the selection. |
| `enter` | On an agent: open its drilldown (PID, CPU, memory, children, and each heavy workload it owns with its state: running, `PAUSED`, `QUEUED`). On a heavy workload: toggle its details. |
| `esc` | Leave the drilldown, help, or filter. |
| `p` / `r` | Pause or resume the selected heavy workload, after a `y/n` confirmation. |
| `s` | Cycle the sort column: CPU, memory, name, then `throtd`'s order (`▼` descending, `▲` ascending). |
| `/` | Filter the timeline by event kind (case-insensitive substring); `enter` keeps it, `esc` clears it. |
| `?` | Help. |
| `q`, `ctrl+c` | Quit. |

### Coding agents: MCP, Claude Code hook, Codex

**MCP.** `throt mcp` is a read-only [MCP](https://modelcontextprotocol.io) server on stdio, so agents can check pressure and capacity before starting a big build. Its tools: `throt_status` (pressure, resources, agents, heavy workloads), `throt_capacity` (an advisory `ok_to_start` with a reason and suggestion, for the agent that launched the server by default), `throt_events`, `throt_stats`, `throt_explain` (why a PID is running, paused or queued), and `throt_wait_for_capacity` (polls the capacity verdict once a second for up to `max_wait_seconds`, 1–600, default 60; cancellable). None of them pauses, resumes or changes configuration. `throtd` must be running. Tool output, process names and PIDs included, goes into the agent's context and so reaches your model provider. The server speaks MCP `2026-07-28` (per-request version in `_meta`, `server/discover`) as well as the `initialize` revisions `2025-11-25`, `2025-06-18` and `2024-11-05`; it has tools only. Every tool returns text and structured content, described by its `outputSchema` ([fields](docs/ARCHITECTURE.md#compatibility)).

```bash
claude mcp add --scope user throt -- throt mcp
```

**Claude Code hook.** `throt setup claude-code` prints a `settings.json` hook and the MCP config to paste; it writes no file. The hook (`throt hook claude-code --max-wait D`, a [`PreToolUse`](https://code.claude.com/docs/en/hooks) hook on `Bash`) makes Claude Code wait for capacity without being told:
- It waits only when the Bash command is a single heavy tool (by the names Throt classifies as heavy), optionally after `cd DIR &&`, variable assignments or `time`. It then polls `throtd` once per second until a heavy job is OK to start for the calling agent, or `D` passes (default 50s; the setup snippet uses 110s with a 120s hook `timeout`, because a hook that times out blocks the call).
- Everything else passes at once without contacting `throtd`: pipes, `;`, `||`, `&`, redirections, subshells, substitutions, heredocs, comments, a heavy tool after another command (`make && ./run`), `throt run` itself, input it cannot parse, and [light invocations](#the-claude-code-hook-delays-commands) such as `--version` or `go env`. So does everything when `throtd` is down.
- It never changes, approves or denies the command, so your permission rules apply unchanged. After it waited, it shows a one-line `systemMessage`.
- It is advisory: it reserves no slot, so two commands let through at the same moment can both start. Use `throt run --` for a reserved slot. Do not add permission allow rules for `throt run`: such a rule would allow every command run through it.

**Codex.** `throt setup codex` prints the block for `~/.codex/config.toml` and an `AGENTS.md` paragraph:

```toml
[mcp_servers.throt]
command = "throt"
args = ["mcp"]
```

### Wait before starting: `throt run`

`throt run -- CMD [ARGS...]` asks for a heavy slot *before* the command starts, the gentle alternative to being paused. It polls `throtd` once per second until a heavy job is OK to start (pressure below high, a free slot, nobody ahead in line, and the calling agent within its `per_agent_limit`), printing a notice to stderr at most every 10s. Then it execs the command in its own place, so the PID, signals and exit status are the command's own.

- **The slot is reserved.** `throtd` grants a lease, so two `throt run` commands cannot both take the last slot. The lease ends when the command shows up as a heavy workload (which then holds the slot), when it exits, or after 60s. A `throt run` inside work that already holds a slot (a make recipe under `throt run`) starts at once. `throt status` shows reserved slots.
- `--timeout D` stops waiting after `D` and runs anyway (default 5m; 0 waits forever). `--no-wait` asks once, takes a free slot if there is one, and runs regardless.
- **It fails open.** If `throtd` is unreachable it prints one notice and runs immediately. It never pauses anything. With `scheduler.mode` off there are no slots, so only high pressure makes it wait.
- A missing command exits 127. One that cannot be executed exits 126, and so does one found only through `.` in `PATH`, which `throt run` refuses, unlike a shell.

To have agents use it, add a line like this to `AGENTS.md` or `CLAUDE.md`:

```markdown
Prefix heavy commands (builds, full test suites, container builds) with `throt run --`, e.g. `throt run -- npm run build`.
```

## Configuration

The config file is optional: `~/.config/throt/config.json`, or `$XDG_CONFIG_HOME/throt/config.json`; `-config PATH` on either program overrides it. Unknown keys are errors. A typical file:

```json
{
  "profile": "balanced",
  "scheduler": {
    "mode": "observe",
    "per_agent_limit": 2
  }
}
```

### Profiles

`profile` presets the scheduler limits; keys you set explicitly win. A profile never turns the scheduler on.

Turn on `enforce` (with `balanced`) when agents' builds together can run out of memory: in the [benchmarks](docs/ARCHITECTURE.md#benchmarks) that is where it finished every job while the baseline swapped until jobs hit their cap or the OOM killer took them. For CPU-only contention, `observe` is enough; enforcing costs a few percent of build time there.

| Profile | Heavy limit | Adaptive |
|---|---|---|
| `performance` | Fixed at CPUs/2 (at least 2). | No |
| `balanced` (default) | Starts at CPUs/4 (at least 2), grows up to one per CPU (2 to 64). | Yes |
| `cool` | 1, and may pause one workload under critical pressure (`pause_on_critical`). | Yes |

### Keys

| Key | Default | Meaning |
|---|---|---|
| `scheduler.mode` | `off` | `off`: no decisions. `observe`: log decisions as `scheduler_decision` events, act on nothing. `enforce`: pause new agent-owned heavy workloads when the limit is reached, and resume them as slots free up. |
| `profile` | `balanced` | See [Profiles](#profiles). |
| `scheduler.heavy_limit` | from the profile | The fixed limit, or with `adaptive` the ceiling (1 to 64). |
| `scheduler.start_limit` | from the profile | Where the adaptive limit starts (0 to `heavy_limit`; 0 means from the profile, capped at `heavy_limit`). Ignored without `adaptive`. |
| `scheduler.adaptive` | from the profile | Halve the limit under high pressure; raise it by one per 30s, up to `heavy_limit`, while the machine copes, or at once for waiting jobs that fit in free memory. |
| `scheduler.per_agent_limit` | `0` (no cap) | Most heavy workloads one agent may run, so one agent can't take every slot (0 to 64). |
| `scheduler.resume_cooldown` | `10s` | Minimum spacing between resumes (and preemptions) while pressure is high, critical, or elevated by memory, swap or latency, and after a pressure pause; free slots are refilled at once at normal or CPU-only pressure, or when a job exited. At most `10m`. |
| `scheduler.max_queue_wait` | `5m` | Work queued this long gets the next free slot even under high pressure, so the queue can't starve (at least 1s). |
| `scheduler.pause_on_critical` | `false` | Under critical pressure, pause the youngest running heavy workload, never the last one. |
| `agents` | none | Custom agents: `[{"name": "myagent", "executables": ["my-agent"]}]`. Up to 32 agents; `name` is 1 to 32 lowercase letters, digits or dashes, with 1 to 16 executable base names each. |
| `socket_path` | see below | The daemon's Unix socket. Restart only. |
| `sample_interval` | `2s` | How often metrics are sampled, from `100ms` to `10s` (the 30s pressure window must hold at least three samples). Restart only. |
| `event_history` | `1000` | Events `throtd` keeps in memory for `throt events` (1 to 100000). Restart only. |

Custom `agents` match exact executable names among your own processes. Generic runtimes such as `node` and build tools such as `go` are refused, because they would match unrelated processes. On Linux, executable names are truncated to 15 characters.

**Socket path.** Both programs find the socket the same way: `socket_path` from the config, otherwise `$XDG_RUNTIME_DIR/throt/throtd.sock`, otherwise (as usual on macOS) `throt-UID/throtd.sock` in the OS temp directory (`$TMPDIR`, else `/tmp`). Its directory must be yours and not writable by others. `throt doctor` prints the path in use.

### Check and reload

- `throt config check [--config PATH]` loads the file exactly as `throtd` does, prints `ok` and each value that differs from the defaults (marked `file` or `profile`), and exits 1 with the error for an invalid or missing file. It warns, without failing, about `enforce` with a fixed `heavy_limit` of at least the CPU count, `pause_on_critical` with mode `off`, and a custom agent that shadows a built-in one.
- `throt config show [--json]` prints the effective config and the source of every value: `default`, `profile` or `file`.
- `throt reload` (or SIGHUP) applies `scheduler.*`, `profile` and `agents` live and prints each `applied:` change. `socket_path`, `sample_interval` and `event_history` are reported as `requires restart`. A file that fails to load changes nothing. Leaving `enforce` resumes everything the scheduler queued, never your manual pauses.

**Logging.** `throtd` logs to stderr. Flags: `-config PATH`, `-version`, `-log-format text|json` (default `text`), `-log-level debug|info|warn|error` (default `info`). `info` covers daemon lifecycle, agents appearing and leaving, pressure changes, scheduler decisions and control actions; `debug` adds each heavy workload start and exit and each admission lease. The event log (`throt events`) records everything regardless of the log level.

## How it decides

- **Attribution.** A process belongs to the nearest ancestor (or itself) that an agent adapter matches by exact executable name; anything else stays unattributed, and unattributed work is never controlled. Heavy workloads (build, test, container-build) are recognized by executable name, and a build folds in the compilers it spawns. Only executable names are read, never arguments or environment.
- **Pressure.** Window medians of memory, swap growth, CPU, load and foreground latency (how late a 10ms timer fires), plus the kernel's own signal where there is one (the memory-pressure verdict on macOS, PSI on Linux), give `normal`, `elevated`, `high` or `critical`, with hysteresis so a blip doesn't count. Busy CPUs alone never go above elevated: saturated cores are throughput, and the latency probe measures their effect on you.
- **Scheduling** (only with `scheduler.mode` set). Throt can only act on processes that exist, so a new heavy workload that appears while every slot is taken is paused within one sample interval and resumed when a slot opens. Work already running is never queued. High pressure blocks new starts; queued work and `throt run` callers share one first-come line. Every decision is an event with a reason code.
- **Control.** Pause and resume act only on heavy workloads of your own agents, and each signal is sent only after the process's identity (PID, start time, uid) is verified. Paused processes are recorded in `paused.json` before they are stopped, so a restarted `throtd` resumes them.

The scheduler stays off by default: in the benchmarks it trades some burst throughput for much better tail latency, which is a choice to make per workload. Details, benchmark results and the local protocol are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), see [Scheduler](docs/ARCHITECTURE.md#scheduler), [Control](docs/ARCHITECTURE.md#control) and [Results](docs/ARCHITECTURE.md#results).

## Troubleshooting

Start with `throt doctor`: it shows the socket path, the config in use, whether `throtd` is reachable, missing metrics and stopped processes, and it is safe to share.

### "cannot reach throtd"

**Symptom.** The TUI shows `cannot reach throtd at PATH: start it with throtd`, or a command fails with `connect to throtd at PATH (is it running?): ...`.

**Cause.** `throtd` is not running, or it listens on a different socket. The socket path depends on `socket_path`, `XDG_RUNTIME_DIR` and `TMPDIR`, so a `throtd` started from a service manager, `sudo`, SSH or cron can end up on a different path than your shell.

**Fix.**
- `... socket dir: lstat ...: no such file or directory` or `connection refused`: start `throtd`.
- If it is running, compare the path in the error (or `throt doctor`'s `socket_path`) with the one `throtd` logged at startup (`listening on PATH`, also the first `daemon_started` event). Give both the same environment, or set the same absolute `socket_path` in the config both read.
- `socket dir PATH is owned by uid N, not the current user`, `is writable by other users (mode ...)`, or `is not a directory (symlinks are not allowed)`: the socket directory failed the private-directory check, which protects the socket from other users. If the directory is yours, `chmod 700` it or remove it and restart `throtd`; otherwise set `socket_path` to a directory only you can write to.
- `throtd is already running on PATH` when starting it: another `throtd` holds the socket. Use that one or stop it first.

### Protocol mismatch after an update

**Symptom.** A command fails with `protocol mismatch: client speaks vX, daemon speaks vY; upgrade throt and throtd to the same release`, or `throt version --json` shows a `daemon.version` different from its own `version`.

**Cause.** `throt update` replaced the binaries but never restarts the running `throtd`, which keeps the old version until restarted.

**Fix.** Restart `throtd` with the command `throt update` printed (`launchctl kickstart -k gui/$(id -u)/dev.throt.throtd`, `systemctl --user restart throtd`, or stop and start it). Stopping it resumes everything it paused. If the error persists, `throt` and `throtd` on your `PATH` come from different releases: reinstall both from one archive.

### A process is stuck stopped (state `T`)

**Symptom.** `throt status` prints `STOPPED   N processes not paused by throtd (see throt doctor)`, or `ps` shows a build in state `T`.

**Cause.** A process was stopped (SIGSTOP) and no pause record explains it: a `throtd` killed hard before it wrote `paused.json`, a deleted state file, a stray daemon, or a shell job suspended with Ctrl-Z. Throt reports these and never resumes them on its own. (Work `throtd` itself paused before a crash is resumed when `throtd` starts again.)

**Fix.**
- `throt doctor` lists them under `orphans` (up to 50: PID, executable name, owning agent, how long, and a hint). It works with `throtd` down too.
- Hint `likely left paused by an earlier throtd`: run the command the hint names. `throt resume PID` resumes a heavy workload's stopped processes, children first; an agent's other processes need `--force`.
- Hint `likely job control (Ctrl-Z)`: run `fg` in that shell. `throt resume PID` refuses with `likely a shell job stopped with Ctrl-Z; use fg in its shell, or throt resume --force`.
- Anything else: `throt resume --force PID` resumes any stopped process of yours (it never forces a stop), or `kill -CONT PID`, which always works.

### An agent is not detected

**Symptom.** Your agent is missing from `AGENTS` in `throt status` or the TUI, and its builds show no owner.

**Cause.** Agents are matched by exact executable name, only among your own processes. Built in: `claude`, `codex`, `opencode`, `gemini`, `aider`, `cursor-agent`, `goose`, `amp`, `copilot`, `orca`. Arguments are never read, so an agent started under another name, or as `node script.js` or `python3 -m aider`, matches none of them and stays unattributed.

**Fix.** Add a custom agent with the executable name its process actually has (the last path element of `ps -o comm= -p PID`; on Linux at most 15 characters), then `throt config check` and `throt reload`:

```json
{"agents": [{"name": "myagent", "executables": ["my-agent"]}]}
```

Generic runtimes and build tools are refused, for example `agents.myagent: "node" is a generic runtime and would match unrelated processes` or `... is a build tool, not an agent`: they would claim unrelated processes. Run the agent through a wrapper or binary with its own name instead.

### Pressure is always elevated on macOS

**Symptom.** `throt status` shows `PRESSURE  ELEVATED (kernel_memory_pressure>=warning)` and `MEM ... (kernel: warning)`; the TUI says `why: kernel memory pressure warning`.

**Cause.** On macOS, Throt uses the kernel's own memory-pressure verdict (`sysctl kern.memorystatus_vm_pressure_level`: 1 normal, 2 warning, 4 critical) instead of the memory-used percentage, because macOS keeps memory full on purpose. `warning` means the kernel itself sees memory as tight (it is compressing and reclaiming), so Throt reports elevated; `critical` maps to high.

**Fix.** Usually nothing: it is an accurate reading, and elevated does not block new starts (only high does). It does keep the adaptive limit from growing. To lower it, free memory (quit idle apps, stop unused containers or agents). Other reasons mean other things: `cpu>=0.85` or `load>=1` is busy CPUs, which never goes above elevated and does not hold the limit; `memory_used>=0.8` on macOS means the verdict could not be read (`throt doctor` lists `memory_pressure` under `host_metrics.missing`).

### macOS refuses to open throt or throtd

**Symptom.** After downloading a release in a browser, macOS blocks `throt` or `throtd` with a dialog saying it cannot verify the developer.

**Cause.** Gatekeeper: the binaries are not signed or notarized, and browser downloads carry the `com.apple.quarantine` attribute.

**Fix.** `xattr -d com.apple.quarantine throt throtd`, once. Downloads with `gh release download` or `curl`, and `throt update`, don't set the attribute.

### `throt update` fails with 404

**Symptom.** `GitHub returned 404 for cfcromn/Throt: the repository is private, so the request needs a token`, or `... the token in use cannot read it`.

**Cause.** GitHub answers 404 for a private repository it won't show you. `throt update` found no token, or one without access.

**Fix.** Set `GH_TOKEN` (or `GITHUB_TOKEN`) to a token that can read the repository, or install the GitHub CLI and run `gh auth login`; `throt update` then uses `gh auth token`. `GitHub rejected the token (401)` means the token itself is invalid or expired: check it with `gh auth status`.

### The Claude Code hook delays commands

**Symptom.** Bash commands in Claude Code take a while to start, followed by `throt: waited 40s for a heavy-job slot` or `throt: no heavy-job slot after 110s (REASON); running anyway`.

**Cause.** The hook waits, up to `--max-wait`, whenever the command is a lone heavy tool and the capacity verdict says no: pressure is high, all heavy slots are in use, jobs are already queued, or the agent hit its `per_agent_limit`. The reason in the message says which.

**Fix.**
- Run `throt status` to see the pressure and slots. With `scheduler.mode` off only high pressure makes it wait.
- Shorten the wait in the hook command, for example `throt hook claude-code --max-wait 30s` (keep it below the hook `timeout`), or set `--max-wait 0` to never wait.
- Light invocations never wait: a first argument such as `-v`, `--version`, `version`, `-h`, `--help`, `help`, `-n` or `--dry-run`, and read-only subcommands such as `go env`, `go list`, `go mod`, `npm view`, `npm ls`, `cargo metadata` and `gradle tasks`. Other read-only subcommands still wait.

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
| v0.8 Integration and readiness | Wait for capacity without being told | Claude Code hook, `throt setup`, `throt config check`/`show`, `throt version --json`, log levels, soak test | shipped (v0.8.0-rc.1) |
| v0.9 Evidence and upkeep | Measure on real machines and stay current | `throt update`, real-machine CPU and memory benchmarks, soak on CI runners, an adaptive limit that grows under CPU-only load, `start_limit`, MCP `2026-07-28` with `throt_wait_for_capacity` and `throt_explain`, TUI scheduler line and event filter | shipped (v0.9.0-rc.1) |
| 1.0 | Turn the scheduler on confidently under an explicit profile | stable compatibility policy, real-machine benchmarks; the scheduler stays opt-in, as the benchmark evidence decided | planned |

**1.0** means the scheduler is ready to turn on with confidence under an explicit `scheduler.mode` and `profile`; it stays off by default (see "Default decision" in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#results)). It requires two supported OS families, the [compatibility policy](docs/ARCHITECTURE.md#compatibility) in force, no known process-loss bug, reproducible benchmarks, and a security review of IPC and process-control boundaries.

## Contributing and license

- [Contributing and development](CONTRIBUTING.md): building, tests, and the rules for process-control changes.
- [Security and privacy](SECURITY.md): reporting a vulnerability, the security review, what Throt collects.
- [Architecture and design](docs/ARCHITECTURE.md)
- [Changelog](CHANGELOG.md)

Throt is released under the [MIT License](LICENSE).
