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
- Heavy workloads (build, test, container-build) are classified by executable name. A heavy process folds into its topmost same-name, same-owner heavy ancestor, so `go` → `go` shows once.
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

**Pressure states:** `NORMAL → ELEVATED → HIGH → CRITICAL` (plus `UNKNOWN` when no signal is available). Signals are window medians (memory used, CPU, load per CPU) and swap growth rate over a 30s window. Escalation needs 3 consecutive samples; recovery needs every signal below its exit threshold (enter − 0.05 for fractions, half the enter rate for swap growth) for twice the window. Swap growth is the median of rates between adjacent samples, so one jump doesn't count as sustained growth. CPU alone never reaches critical. Thresholds live in `internal/pressure`.

**Workload states:** `DISCOVERED`, `RUNNING`, `QUEUED`, `PAUSED`, `RESUMING`, `EXITED`, `UNKNOWN`.

**Initial policy:**

1. Unknown workloads are never automatically controlled.
2. Interactive/foreground work wins over background work.
3. A configurable number of heavy workloads run concurrently.
4. Under high pressure, delay new heavy jobs rather than suspend long-running ones.
5. Under critical pressure, pause an eligible background workload only if explicitly enabled.
6. Queued work ages to prevent starvation.
7. Resumes use cooldowns to avoid oscillation.

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

## Compatibility

Pre-1.0, CLI/config/protocol may change between minor releases, always documented in the changelog; patch releases don't intentionally break documented behavior. Unknown config keys are errors. Client and daemon negotiate a protocol version and fail with an actionable message on mismatch. `internal/` packages are not a public API.

## Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | Split daemon (`throtd`) and client (`throt`). | Scheduling survives a closed TUI; one source of truth; future clients (MCP) reuse the API. Cost: IPC versioning and a security boundary. |
| 2 | Observe before automatic control. | Wrong attribution could pause unrelated work. v0.1 is observe-only; automatic control follows proven attribution, metrics, and benchmarks. |
| 3 | Local-first privacy. | No outbound telemetry; scheduling never needs prompts, source, output, or environment. |
