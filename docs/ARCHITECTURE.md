# Architecture

## Components

| Component | Responsibility |
|---|---|
| `throtd` | Long-running local daemon. Owns monitoring, attribution, pressure, scheduling state, and process-control decisions. |
| `throt` | Disposable client (CLI and TUI). Closing it never stops the daemon. |
| Monitor | Collects host and process metrics through platform adapters. |
| Process graph | Parent/child relationships; attributes descendants to a recognized agent session. |
| Pressure engine | Turns noisy samples into stable pressure states using rolling windows and hysteresis. |
| Classifier | Labels workloads (interactive, background, build, test, container-build, unknown). Evidence-based and overrideable. |
| Scheduler | Decides whether eligible heavy work runs, waits, or resumes. It never owns the agent's reasoning loop. |
| Local protocol | Versioned IPC over a Unix domain socket. Ownership checks precede any process-control method. |

```text
cmd
 └─> app
      ├─> scheduler ─> domain
      ├─> pressure ──> domain
      ├─> monitor ───> platform
      ├─> agent ─────> adapters
      └─> transport ─> local IPC
tui ─────────────────> client API
```

Core scheduling logic must not import the TUI, OS-specific packages, or a particular agent adapter. Core code is deterministic and testable without real processes.

## Attribution

- A process is identified by PID + start time, so a reused PID is a new process.
- Adapters recognize agents by exact executable name, and only among the daemon user's own processes. Generic runtimes such as `node` never match on their own. Names are not authenticated (any program can be called `claude`), so attribution is a hint for observation; process control will need stronger ownership checks.
- A process belongs to the nearest ancestor (or itself) an adapter matches. A nested agent owns its own subtree. Anything else stays unattributed: Throt never guesses.
- Heavy workloads (build, test, container-build) are classified by executable name. A heavy process folds into its topmost heavy ancestor with the same owner, so a build and the compilers and linkers it spawns are one workload.
- Only executable base names are read, never arguments or environment. On macOS, `/bin/ps` runs twice. The first pass has no free-text columns and defines the process set. The second supplies names from argv[0], reduced to the executable's first word so process titles can't leak arguments. A PID with an ambiguous (forged) name line gets no name, which leaves it unattributed and observe-only.

## Control

`throt pause PID` / `throt resume PID` act on a heavy workload shown in `throt status`. The daemon only pauses when all of these hold:
- the caller's uid matches the daemon's (peer credentials);
- the workload is heavy and attributed to an agent;
- every member process belongs to the daemon's user.

Members are the workload's process plus its descendants owned by the same agent. They are stopped parent-first with `SIGSTOP` and resumed child-first with `SIGCONT`. Every signal is sent by one platform call that first verifies the process's identity (PID + start time + uid) and refuses, sending nothing, on a mismatch. A mismatch counts as "gone": the process is skipped, never signaled. If a stop fails, everything already stopped is resumed.

Identity verification and signaling cannot be fully atomic on every OS:
- **Linux** opens a pidfd first, then verifies identity from `/proc`, then signals through the pidfd. The pidfd pins the process, so a PID reused after the pidfd opens can never receive the signal. Kernels without pidfds (before 5.3, or with a seccomp filter that blocks `pidfd_open`) fall back to verify-then-`kill(2)`.
- **macOS** has no pidfd, so it verifies with one `/bin/ps` call and then calls `kill(2)`. The residual risk: if the process exits and the user's own new process reuses its PID within that one `ps` call, with a start time in the same second (`ps` start times are second-granular), the new process gets the signal. PIDs are allocated sequentially, so the PID space would have to wrap within that window.

Throt never forgets work that might still be stopped. A record lists every process it may have stopped and moves through three states: *pausing* (written before the first `SIGSTOP`, listing every intended member), *paused* (the members actually stopped), and *resuming* (members still to be continued). A member leaves a record only after it was continued or the OS confirms it is gone; a process missing from one sample is confirmed with the OS before it is dropped. When a resume, a pause rollback, or a shutdown resume fails for some member (on macOS `/bin/ps` can fail to fork under the memory pressure that prompted the pause), the record stays *resuming* and the daemon retries it every sample tick.

Every per-process OS call has its own deadline (2 s), so a hung `ps` can delay but never freeze the daemon. Shutdown does not cancel resumes. `throt status` reads a copy of the records and never waits for a pause or resume in progress. The tick's retry and cleanup skip a round instead of waiting.

Records are written atomically to `paused.json` in the private socket directory each time they change, before any signal that depends on the change, and the file is removed when no records remain. On shutdown the daemon resumes everything; anything it cannot resume stays in the file. On startup the daemon loads every record in the file as *resuming* and resumes it, continuing only processes whose identity still matches. Restoring never pauses anything. Continuing a member that was never stopped (after a crash mid-pause) is harmless. A file that cannot be parsed is moved to `paused.json.invalid` and nothing is signaled.

## Failure behavior

- daemon crash: paused processes stay stopped until throtd starts again and resumes them from `paused.json`, or until the user runs `kill -CONT PID`. A clean shutdown resumes them;
- TUI crash: daemon continues;
- metrics unavailable: degrade to observe-only;
- attribution uncertain: mark unknown, never guess ownership for control;
- scheduler state lost: rebuild from live process state;
- unsupported platform feature: disable that capability explicitly.

State is reconstructable. Only configuration, bounded history, and diagnostics data are persisted — never command output or repository contents.

## Concurrency

Every long-lived goroutine has an owner, a cancellation path (`context.Context`), defined shutdown behavior, and a bounded channel or documented backpressure policy. No goroutine-per-event designs.

## Events

One structured event model feeds logs, diagnostics, and the TUI timeline: process discovered/lost, attribution changed, pressure transition, workload classified, queued/resumed, policy changed, error/degraded mode.

## Scheduler

**Objective:** maximize useful agent throughput while keeping the machine responsive. This is not a CPU cap.

**Inputs:** CPU utilization and load, memory used/available, swap usage and growth, per-process CPU/RSS, ancestry, number of heavy workloads, workload runtime. Later: platform memory-pressure APIs and responsiveness probes.

**Pressure states:** `NORMAL → ELEVATED → HIGH → CRITICAL` (plus `UNKNOWN` when no signal is available). Signals are window medians (memory used, CPU, load per CPU, and foreground latency, measured as how late a 10ms timer fires each tick; inside a CPU-quota cgroup the daemon's own throttling also reads as lateness) and the swap growth rate, over a 30s window. Escalation needs 3 consecutive samples; recovery needs every signal below its exit threshold (enter − 0.05 for fractions, half the enter rate for swap growth, 0.75× for latency) for twice the window. Swap growth is the median of rates between adjacent samples, so one jump doesn't count as sustained growth. CPU alone never reaches critical. Thresholds live in `internal/pressure`.

**Workload states:** `DISCOVERED`, `RUNNING`, `QUEUED`, `PAUSED`, `RESUMING`, `EXITED`, `UNKNOWN`.

**Initial policy:**

1. Unknown workloads are never automatically controlled.
2. Interactive/foreground work wins over background work.
3. A configurable number of heavy workloads run concurrently.
4. Under high pressure, delay new heavy jobs rather than suspend long-running ones.
5. Under critical pressure, pause an eligible background workload only if explicitly enabled.
6. Queued work ages to prevent starvation.
7. Resumes use cooldowns to avoid oscillation.

**As implemented** (`internal/scheduler`, opt-in via `scheduler.mode`):
- `off` (default) makes no decisions. `observe` emits every decision as a `scheduler_decision` event marked observed and never acts. `enforce` executes decisions through the same verified controller as `throt pause`.
- Throt can only act on processes that already exist, so *queue* means a heavy workload that appears while no slot is free is paused within one sample interval and resumed when a slot opens. Workloads already running when the daemon starts, or when the policy changes, are never queued.
- High pressure blocks new starts and resumes, except that work queued longer than `max_queue_wait` still gets a free slot, one per cooldown, so the queue can't starve. Critical pressure may *preempt* (pause) the youngest running workload when `pause_on_critical` is set, but never the last one. Resumes and preemptions are spaced by `resume_cooldown`.
- Unknown pressure (degraded metrics) fails open: nothing new is queued, and work queued longer than `max_queue_wait` is resumed.
- New arrivals queue behind already-waiting work, so short jobs can't starve a long queued one.
- Unattributed workloads count toward the limit but are never controlled. The controller records whether a pause was manual or the scheduler's, and the scheduler never touches manual pauses.
- **Adaptive** (AIMD): the effective limit starts at `heavy_limit`. It rises by 1 per 30s at normal pressure, halves (floor 1) at most once per 30s at high or critical pressure, holds at elevated, and resets at unknown pressure. Lowering it never preempts; it only delays new starts. Changes are `scheduler_limit` events.
- **Per-agent budget:** with `per_agent_limit`, an agent's new heavy workload waits (`owner_budget_exhausted`) while that agent already runs that many, even if global slots are free.
- If a queued workload's top process exits while stopped, its remaining members are resumed (`top_exited`). The scheduler only queues workloads of up to 256 processes.

**Fairness:** priority combines user override, workload class, queue age, project fairness, and previous preemption count. No project may hold every heavy slot indefinitely.

**Explainability:** every decision emits the decision, target, reason code, observed pressure, relevant policy, and timestamp:

```json
{"decision":"queue","workload":"pid:4312","reason":"heavy_slots_exhausted","pressure":"high","running_heavy":2,"heavy_limit":2}
```

**Safety invariant:** the scheduler never signals a process unless ownership confidence satisfies the control policy, the process is eligible, the feature is enabled, and the platform supports the signal semantics.

## Platforms

Platform code implements *capabilities* (host metrics, process enumeration, ancestry, per-process resources, suspend/resume, priority) instead of pretending every OS is identical. The scheduler queries capabilities and degrades explicitly.

- **macOS** — first target: enumeration, ancestry, CPU/RSS, memory/swap, process-group handling. No undocumented APIs on the stable path. Throt avoids cgo, so per-process data comes from `/bin/ps`, and host CPU busy time is derived from per-process CPU deltas. That figure reads low when many processes live shorter than one sample interval.
- **Linux** — native procfs/cgroup facilities, not macOS semantics.
- **Windows** — not committed before 1.0.

## Benchmarks

A scheduler can make graphs calmer while making real work slower, so changes to default behavior need evidence.

- **Responsiveness:** foreground probe latency (p95/p99), time in severe pressure, swap growth.
- **Throughput:** total wall-clock, heavy jobs/hour, build/test durations.
- **Scenarios:** 1 agent + 1 build; 4 agents with light work; 6–8 agents with overlapping builds; Docker + JVM + JS builds; memory-heavy; long job mixed with short jobs (starvation).
- **Method:** fixed fixtures, warm-up, repetitions, same machine for baseline vs. Throt, report medians and tails.

### Results

`make bench` runs `internal/bench` in a `golang` Alpine container matching `go.mod` (the results below were measured with Go 1.26.4), limited with `--cpus=2`, where the PID namespace keeps the enforce-mode daemon away from the host's agents. It builds three helpers: an agent stub named `claude`, a job named `make` that burns one core for about 5 s of calibrated CPU work, and a probe that sleeps 10 ms in a loop and records how late it wakes. The in-process daemon uses the real Linux samplers and controller with a 500 ms sample interval. Baseline is `scheduler.mode` off; Throt is enforce with `heavy_limit` 2 and `resume_cooldown` 1s, other settings default. Makespan runs from the first job start to the last exit; peak running is sampled every 100 ms from `/proc`; interventions are scheduler decisions (queue and resume) in the event log.

Medians of three runs on an Apple M5 Pro (macOS 26.5.1), Docker Desktop 29.5.3, linuxkit 6.12.76 arm64 VM with 10 CPUs and 7.75 GiB:

| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Peak running | Interventions |
|---|---|---|---|---|---|---|---|
| 8 jobs at once | baseline | 1.5ms | 70.5ms | 79.8ms | 17.1s | 8 | 0 |
| 8 jobs at once | Throt | 1.8ms | 2.3ms | 2.6ms | 22.4s | 8 | 12 |
| 10 jobs, one per 1s | baseline | 1.6ms | 66.9ms | 79.8ms | 23.7s | 10 | 0 |
| 10 jobs, one per 1s | Throt | 1.8ms | 2.3ms | 2.6ms | 27.7s | 3 | 16 |

Run to run, p95 and makespan varied by about 10% at most. Pressure stayed normal throughout: `/proc/stat` and the load average describe the whole 10-CPU VM, not the 2-CPU quota, and the daemon's median latency stayed low. Peak running is 8 for the burst because Throt can only queue work that already exists, so all 8 jobs run for up to one sample interval before 6 are paused. The probe also measures cgroup throttling (see Scheduler): with 8 runnable jobs the container spends its 200 ms of quota early in each 100 ms period and then everything, the probe included, waits for the next one. Most of the baseline tail is that throttling, so a plain 2-core host would likely show a smaller gap. Baseline finishes 40 s of nominal CPU work in 17.1 s, under the 20 s the quota implies, so the quota and the calibration are approximate: compare modes, not absolute times.

Under this setup Throt cuts probe p95 and p99 from about 70–80 ms to under 3 ms, costs about 0.3 ms at p50, and makes the batch 17–31% slower, because two jobs alone leave no slack and each slot handoff waits up to one sample interval plus the cooldown.

## Local protocol

Clients reach `throtd` over its Unix socket (`throt doctor` prints the path). Each connection carries one request and one response, both newline-terminated JSON of at most 1 MiB:

```json
{"protocol": 1, "method": "status", "params": {}}
{"protocol": 1, "result": {...}}            // or {"protocol": 1, "error": "..."}
```

A request for any other protocol version is rejected with an error naming both versions. `protocol` is bumped on any incompatible change; additive fields are not breaking, so clients must ignore unknown fields.

| Method | Params | Result | Privileged |
|---|---|---|---|
| `ping` | – | `version`, `protocol`, `pid`, `uptime` | no |
| `status` | – | `pressure`, `reason`, `host`, `agents`, `heavy`, `paused`, `scheduler`, `degraded` | no |
| `events` | `limit` (0 or > 1000 means 1000) | recent events, oldest first: `time`, `kind`, `message`, `attrs` | no |
| `pause` | `pid` | – | yes |
| `resume` | `pid` | – | yes |

Privileged methods also require the caller's uid (from peer credentials) to match the daemon's. The Go types in `internal/app` are the reference definitions; `throt mcp` exposes the read-only methods to coding agents.

## Compatibility

Pre-1.0, CLI/config/protocol may change between minor releases, always documented in the changelog; patch releases don't intentionally break documented behavior. Unknown config keys are errors. Client and daemon negotiate a protocol version and fail with an actionable message on mismatch. `internal/` packages are not a public API.

## Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | Split daemon (`throtd`) and client (`throt`). | Scheduling survives a closed TUI; one source of truth; future clients (MCP) reuse the API. Cost: IPC versioning and a security boundary. |
| 2 | Observe before automatic control. | Wrong attribution could pause unrelated work. v0.1 is observe-only; automatic control follows proven attribution, metrics, and benchmarks. |
| 3 | Local-first privacy. | No outbound telemetry; scheduling never needs prompts, source, output, or environment. |
