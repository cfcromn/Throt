# Changelog

All notable user-visible changes to Throt are documented here.

The format is inspired by Keep a Changelog and versions follow Semantic Versioning.

## [Unreleased]

### Changed
- The `reason` attr of every `control_refused` event is now a stable code. Refusals throtd decides itself used to put a sentence there; they now use `start_mismatch`, `gone`, `peer_mismatch`, `unidentified`, `paused_member`, `needs_force`, `job_control` or `not_stopped` (or the controller's `not_heavy`, `foreign_uid`, `not_paused`). The sentence stays in the event message and the error the client sees.

### Security
- macOS process names replace control characters with `?`, as on Linux, so a process cannot put terminal escapes into names that `throt doctor`, `throt status` or events print.

## [0.9.0-rc.1] - 2026-10-29

Evidence from real machines and fixes from it: CI benchmarks and soak, `throt update`, a scheduler that no longer serializes admission, memory-pressure fixes, a fuzzed and hardened input surface, and a TUI that fits every terminal.

### Added
- `throt update [--check] [--version vX.Y.Z] [--pre] [--yes]` installs a GitHub release in place: it locates the running `throt` and the `throtd` next to it, picks the newest release (prereleases with `--pre`, or when the running version is one; any release, older ones included, with `--version`), downloads the archive for this platform, refuses it unless its SHA-256 matches `checksums.txt`, checks that both new binaries run and report the expected version, then renames them over the old ones, `throtd` first. Any failure before that changes nothing, as does an unwritable directory. It asks for confirmation on a terminal and refuses on a non-terminal stdin without `--yes`. `--check` only reports, exiting 0 when up to date and 10 when an update is available. The private repository needs a token from `GH_TOKEN`, `GITHUB_TOKEN` or `gh auth token`. `go install` and development builds get the `go install` commands instead. A running `throtd` is never restarted; the command to restart it is printed. This is the only network access in Throt, and only when you run it.
- Real-machine benchmarks: the `Bench` workflow (`gh workflow run bench.yml`, also weekly) runs `internal/bench` on hosted Linux and macOS runners, sized to the runner (4× its CPUs per scenario), and writes the medians of three runs to the job summary and an artifact. The benchmark now runs on macOS (CI only) as well as in a Linux container.
- `throt mcp` speaks MCP `2026-07-28`, the stateless revision, as well as the `initialize` handshake revisions (`2025-11-25`, `2025-06-18`, `2024-11-05`), on the same stream. A request whose `_meta` names `io.modelcontextprotocol/protocolVersion` is served statelessly: it must name `2026-07-28` (otherwise `-32022` with the supported versions) and send `io.modelcontextprotocol/clientCapabilities` (otherwise `-32602`). Its results carry `resultType` and the server's identity in `_meta`, and `tools/list` adds `ttlMs` and `cacheScope`. `server/discover` lists the supported versions, capabilities and instructions. `ping` answers only legacy requests, since the new revision removed it. `initialize` always negotiates a legacy revision, so Claude Code, Codex and other handshake clients see no change. Resources, prompts, `subscriptions/listen`, multi round-trip requests, progress notifications and logging are not offered.
- MCP tool `throt_wait_for_capacity` (`agent`, `max_wait_seconds` 1–600, default 60): checks the `throt_capacity` verdict once a second until a heavy job is OK to start or the time is up, then returns the verdict, `waited_seconds`, `checks` and `timed_out`. An agent can use it to wait without the hook. It runs in the background, so other requests are still answered and `notifications/cancelled` stops it with no response. At most 4 waits run at once, and closing stdin stops them. It fails open when throtd is unreachable, and it reserves nothing.
- MCP tool `throt_explain` (`pid`): what `throt explain PID` shows (state, owner, scheduler limits, pressure, the workload's last 10 events, and a one-line summary), as text and structured content. Times are RFC 3339. The summary refers to the resume cooldown without a value, because throtd does not report it.
- Memory-pressure benchmarks: `TestBenchMemory` runs 8 jobs of 22% of RAM each (176% together), at once and staggered, with the scheduler off and enforcing the `balanced` profile, and adds peak swap and swap growth to the table. They need `THROT_BENCH_MEMORY=1` as well as `THROT_BENCH=1`, run only on CI runners or in a container with a memory limit (sized from the cgroup limit), and cap every job at 3 minutes. The `Bench` workflow runs them after the CPU scenarios.
- Soak on real machines: the `Soak` workflow (`gh workflow run soak.yml`, also weekly) runs the `internal/e2e` soak test for 10 minutes on hosted Linux and macOS runners and writes its results to the job summary and an artifact (with throtd's log). The soak test now runs on macOS (CI only) as well as in a Linux container, with the same checks; on macOS it reads process state from `sysctl`, and throtd's RSS, threads and file descriptors from `ps` and `lsof`.
- TUI: a `SCHEDULER` line with the mode, the current heavy limit (for an adaptive scheduler with its start and ceiling), the per-agent cap, the queued workloads and the reserved `throt run` leases, and a `STOPPED` line counting stopped processes throtd did not pause, pointing to `throt doctor`.
- TUI: `/` filters the timeline by event kind (case-insensitive substring, at most 32 characters), `esc` clears it. Control, scheduler and failure events are tagged `CTL`, `SCHED` and `FAIL` as well as colored.
- TUI: `NO_COLOR` and `TERM=dumb` turn off all ANSI styling.
- Observe mode shows what it would have queued: the `status` method's `scheduler` result adds `would_queue` (the workloads the latest observe round would have queued), and `throt status` shows them as `running (would queue)`.
- Built-in agents `amp` (Amp) and `copilot` (GitHub Copilot CLI), matched by exact executable name like the others. Arguments are still never read, so an agent run as `node script.js` or `python3 -m …` stays unattributed.

### Changed
- In `enforce` mode, a heavy slot freed by an exit is refilled within about 250 ms instead of at the next sample (up to `sample_interval`, 2 s by default): while work is queued, throtd checks every 250 ms whether a running heavy workload's top process exited (`kill(pid, 0)` on macOS, `/proc` on Linux, never `ps`) and samples at once when one did.
- TUI: colors adapt to a light or dark terminal background, so warnings keep their contrast on light themes. The scheduler moved from the `SYSTEM` header to its own line.
- TUI: the "terminal too small" message is cut to the window, so even a 1×1 window gets no overflowing lines.
- Every `throt mcp` tool description now tells the model when to call the tool, and the server instructions mention the new tools.
- On CI runners the benchmark measures the shipped defaults (the `balanced` profile, 2 s sampling, 10 s cooldown) instead of a fixed `heavy_limit`; the container run keeps its fixed settings for comparison.
- Adaptive scheduling grows the limit (by one per 30s, up to `heavy_limit`) at elevated pressure caused only by throughput signals (CPU, load, CPU PSI `some`) while memory, swap, latency, the kernel memory verdict and memory and I/O PSI stay below their elevated exit thresholds. Before, saturated CPUs held pressure at elevated and the limit never grew, so on the first real-machine benchmark cores sat idle. Elevated pressure from memory, swap or latency still holds the limit, and high or critical still halves it. The increase is a `scheduler_limit` event with reason `pressure_elevated_throughput`.
- Adaptive scheduling also grows the limit at once, not one per 30s, while pressure is normal or elevated by throughput only and work waits for a slot: by up to one slot per waiting job, as far as the memory headroom below the elevated memory threshold fits a slot per the largest heavy workload's RSS, and only once that RSS has stopped growing. On the 5-CPU macOS runner a burst of 20 short CPU-bound jobs took 1.8× the unthrottled time because the limit sat at 3; memory-heavy bursts still take the slow path.
- The `balanced` profile's adaptive ceiling is now the CPU count (2 to 64), and its limit starts at CPUs/4 (at least 2), the old fixed value, through the new `scheduler.start_limit` (0 to `heavy_limit`; left out or 0 derives it from the profile, capped at `heavy_limit`, so files that set only `heavy_limit` keep working). Before, `balanced` capped a 5-CPU machine at 2 heavy jobs. `performance` stays a fixed CPUs/2: it is not adaptive, so its ceiling is its limit, and 2× CPUs would hardly ever queue. `cool` stays at 1. The `status` method's `scheduler` result adds `adaptive`, `start_limit` and `max_limit` for an adaptive scheduler.
- `throt config check` warns about a `heavy_limit` of at least the CPU count only when `adaptive` is off; an adaptive ceiling of every CPU is the `balanced` default.
- macOS: throtd reads the names of newly seen processes in-process from `sysctl kern.procargs2` instead of running `ps -p` for each, falling back to `ps` only for other users' processes. Names are unchanged (argv[0], as `ps` prints it). A burst of new PIDs no longer costs a full 50 ms `ps` names pass, and a process sample drops from about 60–100 ms to 50–85 ms of CPU on a busy Mac with 1,200 processes. The remaining cost is one `ps -axww` (about 33 ms), which stays because only setuid `ps` can read other users' CPU time. `THROT_OVERHEAD=60s go test ./internal/platform -run Overhead` measures it.
- README reorganized with a quickstart and troubleshooting.
- Docs: the README status and roadmap show v0.8.0-rc.1 shipped and v0.9 in progress; `go install` documents `GOPRIVATE='github.com/cfcromn/*'` and the Git credentials a private repository needs; `sample_interval` documents its 10s maximum; the launchd and systemd sections explain why they share the shell's socket path and point to `socket_path` when they don't; the architecture notes that the adaptive limit resets at unknown pressure only after pressure was known once, and lists `orphans` in `status` and the new MCP tools and exit code 10 in the compatibility surface.
- Linux: a process sample reads `/proc/<pid>/status` only for processes new since the last sample (by PID and start time) and a rolling 1/15 of the rest, reads `stat` with three system calls into a reused buffer, and parses it without allocating. Results are unchanged, except that a process that changes its uid in place (`setuid`) shows the new one within 15 samples instead of the next; the controller still re-reads it before every signal. Measured in a container at a 2 s interval, a sample drops from about 32 to 11 ms of CPU with 1,000 processes and from 63 to 35 ms with 3,000 (3.1% to 1.7% of one core), and from 28 to 3 allocations per process. `THROT_OVERHEAD=60s go test ./internal/platform -run Overhead` measures it on Linux too.
- CLI: an invalid config file no longer locks out client commands (`status`, `ping`, `pause`, `resume`, `events`, `explain`, `stats`, `run`, `hook`, `top`, `mcp`): they warn once and use the default socket, and `throt run` no longer blames an unreachable throtd; `config check`/`show` still fail. Usage errors print one line and exit 2, and commands that take no arguments reject them. `throt status --json` prints the status as JSON; `status` shows its `SCHEDULER` line before the tables, and `status`/`explain` show `limit N (adaptive, start S, max M)` or `limit N`, with no running/limit ratio while the scheduler is off. `config check` without a file exits 0 ("defaults apply"). `throt`/`top` without a terminal suggest `throt status`. The hook's stdin read is bounded by `--max-wait` (it fails open), and `THROT_HOOK_DEBUG=1` explains ignored events on stderr. `update --check` reports the latest release on development and `go install` builds, and the `go install` hint mentions `GOPRIVATE` and git credentials.
- When throtd is down, clients say `throtd is not running (no socket at PATH); start it with \`throtd\`` (or `nothing listening on PATH` for a stale socket) instead of an `lstat` error.
- Control refusals read as sentences (`pause refused: it is already paused, alone or as part of another workload`); the `control_refused` event's `reason` attr keeps the code (`already_paused`). `control_paused` and `control_resumed` events carry an `action` attr (`pause`, `resume`).
- Process graph events read as sentences: `discovered heavy workload make (pid 123)`, `heavy workload make (pid 123) exited`, `agent claude-1 started (pid 10)`, `agent claude-1 exited (pid 10)`.
- Config errors name the line and column of a JSON syntax or type error, and the section of an unknown key (`unknown key "heavy_limt" in scheduler; did you mean "heavy_limit"?`).
- `scheduler.resume_cooldown` is capped at 10m, since a larger value froze the queue.
- The pressure window is 30s or ten sample intervals, whichever is longer, instead of always 30s, so a long `sample_interval` still has several samples per window (up to 100s at the 10s maximum).
- `throt doctor` lists host CPU under `host_metrics.derived` ("derived from process CPU by throtd") rather than as missing when throtd derives it from the process table, as on macOS.
- The shutdown event says how many paused or queued processes it resumed.

### Fixed
- Queued work no longer waits out `max_queue_wait` because it fills RAM. Beside Linux memory PSI, critical from near-full memory now needs RAM and swap together at 95% (`memory_and_swap_used`): RAM full of paused jobs while swap had room held pressure at critical, which blocks every resume, although PSI showed no stall. Beside the macOS verdict, memory used no longer reaches critical at all, as documented; critical needs swap growth there. The memory benchmark's job cap no longer counts time spent queued, and each run waits for the previous one's pressure to settle.
- The scheduler no longer serializes admission behind `resume_cooldown`. Free slots under the limit are refilled in the same round, several at once, when pressure is normal or elevated by CPU/load only, or when a running job's process exited (not when it only stopped being heavy, e.g. regrouped into another workload or reclassified as light) (decision reasons `pressure_normal`, `pressure_elevated_throughput`, `workload_exited`). The cooldown still spaces resumes at high or critical pressure, at pressure elevated by memory, swap or latency (except refilling exited jobs' slots), and for one cooldown after a preemption or after pressure was last high or critical. Before, 8 jobs of 5 s on 2 CPUs took 1m11s under the `balanced` profile against 21 s without Throt.
- The `Bench` workflow runs the memory scenarios on macOS only; on a bare Linux runner they exhausted RAM and the job was killed.
- `throt resume PID` on an orphan, repeated before the next sample, reported a second resume from the stale snapshot. throtd now checks the process's live state and refuses one that is gone or not stopped.
- Resume retries no longer stall behind one record whose resume keeps failing: each round starts after the record that failed last.
- A daemon handler error with an empty message no longer reaches the client as success.
- A heavy job first seen unattributed (its agent missing from one sample) is credited to its agent in `throt stats` once attribution appears, instead of staying unattributed.
- After a sampling gap longer than the pressure window, recovery waits the full dwell again instead of de-escalating on the first sample.
- A pause checks every member's live uid before its first signal, so a stale sampled uid (Linux) is refused with nothing signaled instead of stopping and rolling back the top process every round. The shutdown event counts only processes actually continued. Resuming an orphan whose top process was resumed by hand continues its stopped children. `would_queue` is cleared on a tick whose process sample fails.
- Config errors locate a bad duration by line, column and key (before: no location, or line 1, column 1 for a non-string), and an unknown key inside a section spelled in other case, such as `"Scheduler"`, instead of blaming the section.
- TUI: fits every window from 40×10 up. Wide (CJK) characters and emoji no longer overflow a line. The `SCHEDULER` line wraps instead of losing its queued count, and it uses `throt status` wording: `enforce, limit 3 (adaptive, start 2, max 8), 1 queued, 2 reserved`, or `1 would queue` in observe mode. On a short terminal, SWAP, the why line, MEM and CPU give way so a table stays visible. Work that observe mode would queue shows as `running (would queue)`. With no daemon, the TUI says ``throtd is not running — start it with `throtd` `` and keeps retrying. Timeline events show their message without the kind in front, and `/` matches message text as well as kind. The footer always lists `? help` and `q quit`, plus `p/r` when the heavy table has focus. Quitting stops in-flight polls and timers.

## [0.8.0-rc.1] - 2026-10-28

Claude Code integration, fair admission, a security review, reproducible releases, and macOS end-to-end tests: the 1.0 groundwork.

### Added
- `internal/e2e` soak test (`THROT_SOAK=1`, containers only, not run in CI): 10 minutes of agent job churn, PID reuse, manual pause/resume, `throt run`, mode reloads and a `throtd` crash and restart, checking for stranded stopped work, flat memory, goroutines, threads and file descriptors, `throt status` p99 under 1s, event `seq`/`instance` integrity, and the heavy limit.
- One FIFO line for `throt run` lease waiters and scheduler-queued workloads, ordered by wait time, with `max_queue_wait` aging and per-agent budgets applying to both. Previously `acquire` refused every lease while anything was queued, so under steady agent load `throt run` waited for its timeout and then ran outside the limit. The scheduler no longer resumes a queued workload, or admits a newcomer, while an older waiter is in line. throtd remembers at most 256 waiters and forgets one after 2s without a poll or when it exits.
- `throt hook claude-code [--max-wait D]`: a Claude Code `PreToolUse` hook on `Bash`. When the command's command word is a heavy tool (alone, or after `cd DIR &&`, variable assignments, or `time`), it polls `throtd`'s capacity verdict for the calling agent and returns once a heavy job is OK to start or after `--max-wait` (default 50s). It never changes, approves, or denies the command, so your permission rules apply to it unchanged. Advisory: it takes no lease. It fails open: light or unparsable commands, malformed input, or an unreachable `throtd` return at once. Shell parsing lives in the new `internal/shellwords`.
- `throt setup claude-code` and `throt setup codex` print the hook, MCP server, and `AGENTS.md` snippets to paste; they write no files.
- `throt config check [--config PATH]` validates the config file with the same loader as throtd, prints `ok` and what it changes from the defaults (each marked `file` or `profile`), and exits 1 on an invalid or missing file. It warns, without failing, about `enforce` with `heavy_limit` at least the CPU count, `pause_on_critical` with mode `off`, and custom agent executables that shadow a built-in adapter.
- `throt config show [--json]` prints the effective config (defaults, then profile, then file), the profile applied, and the source of each value (`default`, `profile`, or `file`).
- `throt version --json` prints the client version, protocol version, Go version, and throtd's ping result when it is reachable (`daemon` is null with a `daemon_error` otherwise).
- `throtd -log-format text|json` (default `text`) and `-log-level debug|info|warn|error` (default `info`), documented in `throtd -h`.

### Changed
- Releases: reproducible builds (`-trimpath`, file times from the commit), `CHANGELOG.md` in every archive, release notes taken from this file's section for the tag (a release without one fails), and build provenance attestations for the archives and `checksums.txt` (`gh attestation verify`, published while the repository is public).
- Builds without release ldflags (`go install`, `go build`) report the module version, or `dev-<revision>` with `-dirty` for uncommitted changes, instead of `dev`, in `throt version`, `throtd -version` and `ping`.
- `throt hook claude-code` lets version, help and dry-run invocations, and read-only subcommands such as `go env`, `npm view` and `cargo metadata`, through without waiting.
- The `internal/e2e` real-binary tests now run on macOS CI as well as Linux (on macOS only with `CI=true`), covering the `ps` sampler, peer credentials, verify-then-kill, orphans and Ctrl-Z jobs end to end. The soak test stays Linux-only.
- macOS: programs that exec in place (`throt`, `env`, `nice`, `nohup`, `timeout`) are re-named every sample, so `throt run -- npm run build` is recognized as `npm` immediately.
- throtd logs each event at a level that fits it, with its attributes: heavy workloads starting and exiting and admission leases are now `debug` (they come and go with every build), failures, refusals and processes left stopped are `warn`, and the rest stays `info`. The event log is unchanged.
- macOS: throtd caches process names by PID and start time and asks `ps` only about newly seen processes, with a full names pass every 15th sample, roughly halving the CPU cost of a process sample (about 85 ms to 45 ms with 1,150 processes). The process graph allocates its table once per update instead of once per process.
- Docs: a compatibility policy for 1.0 (stable surface, additive and breaking changes, deprecations, what is not covered) and supported versions after 1.0; the 1.0 goal is an opt-in scheduler turned on confidently under an explicit profile, not a default; the architecture no longer describes unimplemented classes, workload states and fairness inputs; `socket_path`, `event_history`, the socket path's `XDG_RUNTIME_DIR` fallback, the global `-config` and `-n` flags, `throtd -version`, and the unsigned macOS binaries (Gatekeeper quarantine) are documented. `throt -h` no longer calls `run` advisory.

### Fixed
- `-h` on `throt`, `throtd`, or a subcommand (`throt config check -h`, `throt run -h`, …) prints the usage to stdout and exits 0, instead of printing `flag: help requested` and exiting 1.
- `SECURITY.md` records the 1.0 security review of IPC and process control: scope, method, a verdict per boundary, findings, and accepted residual risks.
- Security: throtd creates its socket with group and other permissions masked, instead of relying on the process umask until the `chmod` to 0600. Under a lax umask (such as 002) in a socket directory others may traverse, another local user could connect in that window and read `status`, `events` and `stats`. Privileged methods were never reachable.
- `acquire` gives each OS process lookup (one `ps` call on macOS) its own 2 s deadline. Before, a hung `ps` held the request, and one of the 16 connection slots, forever; repeated `throt run` polls could fill every slot and stop throtd from serving `status`, `pause` and `resume`.

## [0.7.0-rc.1] - 2026-10-28

Admission leases make `throt run` race-free, and stopped processes Throt did not pause are reported.

### Added
- Admission leases: `throt run` is no longer advisory. It polls the new privileged `acquire` protocol method, and `throtd` grants a lease only when the `throt_capacity` verdict, counting active leases as running heavy work, says ok, so of two runs racing for the last slot exactly one starts. The scheduler counts leases as running, and a lease's own workload takes its slot without queueing. A lease ends when the command exits, becomes a heavy workload, or after 60s; at most 256 exist. `acquire` checks that the caller's kernel-reported PID (Linux `SO_PEERCRED`, macOS `LOCAL_PEERPID`) is the one it reserves for, and resolves the caller's start time and parent from the OS rather than trusting the client. A `throt run` below a heavy workload or a lease holder (in a make recipe or npm script) is granted at once without a lease (`nested`). With `scheduler.mode` off, leases bind only on pressure. Events `lease_granted` and `lease_released` (`exited`, `became_workload`, `expired`).
- `status` reports `scheduler.leases`; `throt status` shows `N reserved` and each reserved PID; the capacity verdict has `reserved`, which the `throt_capacity` text mentions.
- Report stopped processes throtd did not pause (no paused record), such as work left stopped by a throtd killed before it wrote `paused.json`, or a Ctrl-Z shell job. They are reported, never resumed automatically: `throt status` prints a `STOPPED` line, `throt doctor` gains an `orphans` section (count, plus PID, base name, owner, time observed stopped and a hint for up to 50), and throtd logs one `orphan_stopped` event per newly found heavy workload of an agent (not for shell jobs). `throt resume PID` resumes such a workload after verifying identity; `throt resume --force PID` resumes any other stopped process of yours.
- Process samples carry the process state (`ps state` on macOS, `/proc/<pid>/stat` on Linux).

### Changed
- `throt run` against a `throtd` without `acquire` falls back to the previous advisory check; the other fail-open rules (daemon down runs at once, 5m default timeout, `--no-wait`) are unchanged.
- The scheduler holds stopped processes it did not pause, like manual pauses, so it never queues one and resumes it later.

## [0.6.0-rc.1] - 2026-10-28

Cooperative admission, per-agent accounting, Linux PSI, a richer TUI, and real-binary end-to-end tests.

### Added
- `internal/e2e`: end-to-end tests of the built `throtd` and `throt` binaries against real processes (observe, manual pause/resume, crash recovery, graceful shutdown, enforce scheduler and reload, `throt mcp`). They run on Linux CI with `THROT_E2E=1`, skip outside CI or a container, and use a uniquely named stub agent through a custom adapter.
- Linux PSI (`/proc/pressure`, or the limiting cgroup's `memory.pressure`/`cpu.pressure`) as pressure signals: memory some ≥ 10% → elevated, full ≥ 5% → high, full ≥ 20% → critical; I/O full ≥ 20% → elevated, ≥ 50% → high; CPU some ≥ 50% → elevated. Memory PSI takes precedence over memory used, like the macOS verdict. Shown in `throt status` and the TUI system line when available.
- `throt stats` (and `--json`), the `stats` protocol method, and the `throt_stats` MCP tool: per-agent CPU time, peak memory, heavy jobs started, and time queued by the scheduler since throtd started, with all-agent and unattributed totals. Kept in memory for at most 256 agent instances.
- `throt run [--timeout D] [--no-wait] -- CMD`: cooperative admission control. Waits until throtd reports capacity for a heavy job (the `throt_capacity` verdict, for the nearest agent ancestor), then execs the command in place. Advisory and fail-open: it never pauses anything, runs immediately when throtd is unreachable, and reserves nothing, so two runs checking at once can both start.
- `throt completion bash|zsh|fish` prints a shell completion script; PIDs complete after `pause`, `resume`, and `explain`.
- TUI: sparklines of the last 60 polls for CPU, memory, swap growth and latency; a plain-words line naming the signal behind the pressure level; an agent drilldown (`enter` on an agent) listing its heavy workloads with their state, where `p`/`r` work too; `s` sorts each table by CPU, memory or name.

### Changed
- Memory used still raises pressure to critical (≥ 95%) while a kernel memory signal (macOS verdict or Linux PSI) is present, so memory exhaustion without swap is never hidden.
- `throt run` waits at most 5m by default (`--timeout 0` waits forever).
- Linux memory PSI is read from the same cgroup level as the memory figures (least headroom).

## [0.5.0-rc.1] - 2026-10-28

Accurate pressure everywhere: cgroup-aware Linux, the macOS kernel verdict, and a CPU policy driven by benchmark evidence. Also hardening: fuzzing, a patched toolchain, and a pinned supply chain.

### Added
- Run-at-login (launchd, systemd) and upgrade/rollback instructions.
- `make bench`: containerized real-kernel benchmark (baseline vs enforce), with results in `docs/ARCHITECTURE.md`.
- macOS kernel memory-pressure verdict (`kern.memorystatus_vm_pressure_level`) as a pressure signal (warning → elevated, critical → high), shown in `throt status` and the TUI.
- More heavy tools classified: ninja, cmake, swiftc, kotlinc, dotnet, msbuild, zig, ghc, scalac, sbt, bazelisk, buck2, turbo, nx, rollup, webpack-cli, next, nuxt, rspack (build); rspec, mocha, playwright, cypress, phpunit, ctest (test); buildctl, buildah, kaniko (container build).
- Hot config reload via `SIGHUP` or `throt reload` (privileged `reload` method): applies `scheduler.*`, `profile` and `agents` live, reports restart-only keys, and changes nothing on an invalid file.
- Events carry a `seq`, and the `events` method takes an optional `since` for incremental polling (protocol stays v1).
- `throt events -f` follows new events, and notices when throtd restarts.
- `throt explain PID`: why a heavy workload is running, paused, or queued, with its recent events.
- TUI: `p`/`r` pause or resume the selected heavy workload after a `y/n` confirmation. The timeline now fetches only new events.

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

[Unreleased]: https://github.com/cfcromn/Throt/compare/v0.9.0-rc.1...HEAD
[0.9.0-rc.1]: https://github.com/cfcromn/Throt/compare/v0.8.0-rc.1...v0.9.0-rc.1
[0.8.0-rc.1]: https://github.com/cfcromn/Throt/compare/v0.7.0-rc.1...v0.8.0-rc.1
[0.7.0-rc.1]: https://github.com/cfcromn/Throt/compare/v0.6.0-rc.1...v0.7.0-rc.1
[0.6.0-rc.1]: https://github.com/cfcromn/Throt/compare/v0.5.0-rc.1...v0.6.0-rc.1
[0.5.0-rc.1]: https://github.com/cfcromn/Throt/compare/v0.4.0-rc.2...v0.5.0-rc.1
[0.4.0-rc.2]: https://github.com/cfcromn/Throt/compare/v0.2.0-rc.1...v0.4.0-rc.2
[0.2.0-rc.1]: https://github.com/cfcromn/Throt/compare/v0.1.0-rc.1...v0.2.0-rc.1
[0.1.0-rc.1]: https://github.com/cfcromn/Throt/releases/tag/v0.1.0-rc.1
