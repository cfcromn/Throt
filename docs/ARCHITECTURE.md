# Architecture

## Components

| Component | Responsibility |
|---|---|
| `throtd` | Long-running local daemon. Owns monitoring, attribution, pressure, scheduling state, and process-control decisions. |
| `throt` | Disposable client (CLI and TUI). Closing it never stops the daemon. |
| Monitor | Collects host and process metrics through platform adapters. |
| Process graph | Parent/child relationships; attributes descendants to a recognized agent session. |
| Pressure engine | Turns noisy samples into stable pressure states using rolling windows and hysteresis. |
| Classifier | Labels processes by exact executable name: agent, build, test, container-build, or unknown (`internal/agent`). The heavy-tool table is built in; config can add agents (`agents`) but cannot relabel or override a class. |
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
- Only executable base names are read, never arguments or environment. On macOS, one `/bin/ps` pass with no free-text columns defines the process set. Names come from argv[0], reduced to the executable's first word so process titles can't leak arguments: for the user's own processes it's read from the kernel (`sysctl kern.procargs2`, no fork), and for other users' processes from a second, defensively parsed `ps` pass, where a PID with an ambiguous (forged) name line gets no name and stays unattributed and observe-only. Names are cached by PID and start time.

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
- stopped processes no paused record explains (a daemon killed before it wrote `paused.json`, a deleted state file, a stray daemon, or a Ctrl-Z shell job): reported, never resumed automatically. The sampler reads each process's state (`ps state` on macOS, `/proc/<pid>/stat` field 3 on Linux; `T` is stopped, traced `t` is not). Each tick lists the user's stopped, unrecorded processes (at most 50) in `status` and `throt doctor`, with a hint: a shell parent outside any agent, of the process or of any ancestor reached through stopped processes (Ctrl-Z stops the whole job), means likely job control and resume needs `--force`; an agent's process or a heavy workload means likely left by an earlier throtd. A newly found attributed heavy one, stopped in two consecutive samples, gets one `orphan_stopped` event. The scheduler holds orphans like manual pauses, so it never queues and later resumes one. `throt resume PID` resumes an attributed heavy orphan (identity-verified SIGCONT, children first, with an event); anything else needs `--force`;
- TUI crash: daemon continues;
- metrics unavailable: degrade to observe-only;
- attribution uncertain: mark unknown, never guess ownership for control;
- scheduler state lost: rebuild from live process state;
- unsupported platform feature: disable that capability explicitly.

State is reconstructable. Only configuration, bounded history, and diagnostics data are persisted — never command output or repository contents.

## State

Besides `paused.json`, the daemon keeps per-agent resource accounting (`internal/stats`, shown by `throt stats`). It is updated on the sampling goroutine from every successful process snapshot:
- CPU seconds: each agent's CPU rate (itself plus owned descendants) × the time since the previous snapshot, the same interval the rate was measured over;
- peak memory: the largest RSS of the agent and its owned descendants in one sample;
- heavy jobs: distinct heavy workloads (PID + start time) seen with that owner, including ones already running when the daemon started; one first seen unattributed (its agent missing from a sample) moves from the unattributed bucket to its agent once attribution appears, and counts once;
- time queued: a workload the scheduler holds paused after a round (queued or preempted, never manual pauses) is charged the interval until the next snapshot;
- first and last seen.

Rows are keyed by the agent process's identity, because labels such as `claude-1` are reused after an instance exits; `throt stats` shows the label and PID. At most 256 instances are retained: beyond that the one seen least recently is evicted (among equals, the one first seen most recently, so more than 256 live agents cannot churn rows with history). An *all agents* total, which still includes evicted instances, and an *unattributed* bucket for heavy workloads no agent owns are kept alongside.

Accounting is in memory only and starts with the daemon: the window resets on restart. Persisting it would need a file format, versioning and recovery for a convenience figure, and it would outlive the processes it describes. It holds nothing beyond what `status` already shows (labels, PIDs, start times, and numbers). CPU time is integrated from per-sample rates, so processes that start and exit between two samples (most single compiler invocations) are not counted: treat it as a lower bound and a ranking, not an exact bill.

## Concurrency

Every long-lived goroutine has an owner, a cancellation path (`context.Context`), defined shutdown behavior, and a bounded channel or documented backpressure policy. No goroutine-per-event designs.

## Events

One structured event model feeds logs, diagnostics, and the TUI timeline: daemon started/stopped, agent and heavy workload discovered/lost, pressure changes, scheduler decisions and limit changes, control actions (paused, resumed, refused, failed), recovery, leases, stopped processes Throt did not pause, config reloads, and degraded mode.

## Scheduler

**Objective:** maximize useful agent throughput while keeping the machine responsive. This is not a CPU cap.

**Inputs:** CPU utilization and load, memory used/available, swap usage and growth, per-process CPU/RSS, ancestry, number of heavy workloads, workload runtime, foreground latency, and the kernel's own pressure measurement where the platform exposes one (the memory-pressure verdict on macOS, PSI on Linux).

**Pressure states:** `NORMAL → ELEVATED → HIGH → CRITICAL` (plus `UNKNOWN` when no signal is available). Signals are window medians (memory used, CPU, load per CPU, and foreground latency, measured as how late a 10ms timer fires each tick; inside a CPU-quota cgroup the daemon's own throttling also reads as lateness) and the swap growth rate, over a 30s window. Escalation needs 3 consecutive samples; recovery needs every signal below its exit threshold (enter − 0.05 for fractions, half the enter rate for swap growth, 0.75× for latency) for twice the window, and a sampling gap longer than the window restarts that wait, so one sample after the gap cannot de-escalate. Swap growth is the median of rates between adjacent samples, so one jump doesn't count as sustained growth. CPU and load alone never go above elevated: saturated cores are throughput, and their effect on foreground responsiveness is measured directly by the latency probe, which (with memory and swap) drives high and critical. Two consequences are deliberate: CPU or load overload alone never blocks new starts (only the slot limit applies), and on macOS, while the kernel verdict is available, memory alone stops at high; critical then needs fast swap growth (≥ 32 MiB/s). Thresholds live in `internal/pressure`.

**Kernel memory pressure:** on macOS the kernel's own verdict (`kern.memorystatus_vm_pressure_level`: normal, warning, critical) is a window-median signal too: warning maps to elevated and critical to high; it never reaches critical on its own. Because the verdict is ordinal, entering a level needs the median at that verdict (a majority of the window) and holding it needs at least half the window there (exit half a step below enter), plus the usual recovery dwell, so one blip or a flapping verdict doesn't escalate. macOS keeps memory full on purpose (compressor, file cache), so memory used sits at 80–90% on an idle machine; while a verdict is present in the window it replaces memory used for the memory dimension, unless `Thresholds.MemoryUsedWhenKernelSignal` is set. Without a verdict (a failed read) memory used applies as before. Swap growth is unaffected.

**Pressure stall information (Linux):** PSI reports the share of recent wall time in which some task (`some`), or every non-idle task at once (`full`), waited on a resource. Throt uses `avg10 / 100` of each line as a fraction, and window medians of those like any other signal; exit thresholds are half the enter ones (`StallExitRatio` 0.5), since the smallest threshold (5%) leaves no room for the absolute 0.05 margin. Memory: `some` ≥ 10% → elevated, `full` ≥ 5% → high, `full` ≥ 20% → critical (the whole machine losing a fifth of its time to reclaim is thrashing). I/O: `full` ≥ 20% → elevated, ≥ 50% → high, never critical. CPU: `some` ≥ 50% → elevated only, like the other CPU signals. While memory PSI is present in the window it takes precedence over memory used exactly as the macOS verdict does (same `MemoryUsedWhenKernelSignal` switch), because the page cache keeps Linux memory nearly full too; I/O and CPU PSI add to, and never replace, the other signals. On kernels without PSI (before 4.20, or booted with `psi=0`) the three signals are missing and memory used applies.

**Workload states:** a heavy workload in `status` is `running`, `QUEUED` (paused by the scheduler, queued or preempted), or `PAUSED` (paused manually); one that exits leaves the table. Pause records have their own states (*pausing*, *paused*, *resuming*; see Control). There are no discovered, resuming or exited workload states.

**Policy:**

1. Unknown workloads are never automatically controlled.
2. Foreground responsiveness is protected through the latency probe, not by classifying processes: there are no interactive or background classes (not implemented).
3. A configurable number of heavy workloads run concurrently.
4. Under high pressure, delay new heavy jobs rather than suspend long-running ones.
5. Under critical pressure, pause the youngest running heavy workload only if explicitly enabled (`pause_on_critical`).
6. Queued work ages to prevent starvation.
7. Resumes use cooldowns to avoid oscillation.

**As implemented** (`internal/scheduler`, opt-in via `scheduler.mode`):
- `off` (default) makes no decisions. `observe` emits every decision as a `scheduler_decision` event marked observed and never acts. `enforce` executes decisions through the same verified controller as `throt pause`.
- Throt can only act on processes that already exist, so *queue* means a heavy workload that appears while no slot is free is paused within one sample interval and resumed when a slot opens. Workloads already running when the daemon starts, or when the policy changes, are never queued.
- High pressure blocks new starts and resumes, except that work queued longer than `max_queue_wait` still gets a free slot, one per cooldown, so the queue can't starve. Critical pressure may *preempt* (pause) the youngest running workload when `pause_on_critical` is set, but never the last one. Resumes and preemptions are spaced by `resume_cooldown`.
- Unknown pressure (degraded metrics, or the pressure engine still warming up after a start) fails open: nothing new is queued, and work queued longer than `max_queue_wait` is resumed.
- New arrivals queue behind already-waiting work, so short jobs can't starve a long queued one.
- **One line for queued work and lease waiters:** workloads the scheduler queued and `throt run` callers refused a lease wait in one FIFO, ordered by wait time (`scheduler.Policy.FirstInLine`): anything waiting at least `max_queue_wait` comes first, then the longer wait; ties go to the queued workload. A free slot goes to whichever is first, among entries whose agent is within `per_agent_limit`. The scheduler gets the waiters as an input: it does not resume a queued workload while a waiter is ahead of it (at normal, elevated or unknown pressure, when the waiter could take the slot), and a newcomer queues (`queue_waiting`) while a waiter is in line. throtd remembers a waiter from its first refused `acquire`, by its process key, with that time; it forgets it once granted, after 2s without a poll (twice `throt run`'s interval), or when it exits. At most 256 are remembered; others are answered as newcomers. Under high pressure waiters are refused like new starts, while aged queued work still gets a slot.
- Unattributed workloads count toward the limit but are never controlled. The controller records whether a pause was manual or the scheduler's, and the scheduler never touches manual pauses.
- **Admission instead of pausing:** `throt run -- CMD` polls the `acquire` method until throtd grants it an *admission lease*, then execs `CMD` in place, so cooperative callers never need SIGSTOP. throtd grants a lease only when the `throt_capacity` verdict says ok while counting active leases as running heavy work (overall and for the caller's agent), except that queued work does not refuse by itself: the caller must be first in the line it shares with queued workloads (below), and it checks and records the lease under the same lock as the status snapshot, so of two callers racing for the last slot exactly one gets it. throtd resolves the caller itself: its process key (start time) and real parent come from the OS through the platform controller (`/proc/<pid>/stat` on Linux, `ps` on macOS), for the kernel-verified peer PID, so a wall-clock step cannot make client and daemon disagree on the start time, and the client cannot pick its agent. Ancestors missing from the latest sample are looked up the same way (at most 8 per call). A caller below a heavy workload or a lease holder (a `throt run` line in a make recipe or npm script) is granted at once without a lease (`nested`), since its work is already counted. The lease is keyed by that process key, which the exec keeps, and ends (`lease_released`) when that process exits (`exited`), when it or a descendant appears as a heavy workload (`became_workload`; the workload then holds the slot itself and is admitted into it, never queued), or after 60s (`expired`), for commands that are never heavy. The scheduler counts active leases as running, so it queues a newcomer rather than overcommit a reserved slot. At most 256 leases exist (`too_many_leases`); they live in memory, so a restarted daemon forgets them. Fail-open rules are unchanged: if throtd is unreachable `CMD` runs at once, the wait is bounded by `--timeout` (default 5m), and against a throtd without `acquire` the client falls back to the advisory status verdict, where two callers checking at once can both start. With `scheduler.mode` off, `Assess` ignores slots, so leases bind only on pressure: they are still granted and shown, but any number may start while pressure is below high. The MCP `throt_capacity` tool stays advisory and reports reserved slots.
- **Adaptive** (AIMD): `heavy_limit` is the ceiling and the effective limit starts at `start_limit` (when it is left out or 0, the profile's start capped at `heavy_limit`: CPUs/4, at least 2, for `balanced`, whose ceiling is the CPU count). Without `adaptive`, `start_limit` is ignored and `heavy_limit` is the fixed limit. Turning the scheduler off, or a reload that turns `adaptive` on, starts again at `start_limit`; a reload that stays adaptive keeps the current limit, capped at the new ceiling. `status` reports the effective `heavy_limit` and, when adaptive, `start_limit` and `max_limit` (the ceiling). The limit rises by 1 per 30s at normal pressure, halves (floor 1) at most once per 30s at high or critical pressure, and resets to the ceiling at unknown pressure (degraded metrics fail open), but only once a scheduler round has seen a known pressure. The pressure engine reports unknown (`no_samples`) for its first few samples after every daemon start, and those warm-up rounds hold the limit, so a restart keeps `start_limit` instead of jumping to the ceiling. At elevated it depends on the cause. Elevated *by throughput only* means CPU, load or CPU PSI `some` holds elevated (at or above its elevated exit threshold), nothing reaches high, and every other signal (memory used, swap growth, latency, the kernel memory verdict, memory and I/O PSI) is below its elevated *exit* threshold, so a memory signal that is recovering but not yet clear still counts (`pressure.Engine.ThroughputOnly`; the pressure reason string is unchanged). Then the limit rises by 1 per 30s as at normal (reason `pressure_elevated_throughput`): CPU and load cap at elevated, so a machine whose cores are saturated by builds would otherwise never let the limit grow, although the latency probe shows the foreground is fine; the first real-machine benchmark (5-CPU macOS runner, CPU-only jobs) left 3 cores idle that way. Elevated caused by memory, swap or latency holds the limit. Lowering it never preempts; it only delays new starts. Changes are `scheduler_limit` events.
- **Per-agent budget:** with `per_agent_limit`, an agent's new heavy workload waits (`owner_budget_exhausted`) while that agent already runs that many, even if global slots are free.
- If a queued workload's top process exits while stopped, its remaining members are resumed (`top_exited`). The scheduler only queues workloads of up to 256 processes.
- **Reload:** `SIGHUP` or `throt reload` re-reads the config file. `scheduler.*`, `profile` and `agents` apply live; `socket_path`, `sample_interval` and `event_history` keep their running values and are reported as requiring a restart. A file that fails to load or validate changes nothing (`config_reload_failed`); otherwise `config_reloaded` lists the changed keys. Adapters apply at the next sample, and agents still matched by a same-named adapter keep their labels. The new policy is handed to the sampling goroutine through a one-slot channel and applied at its next round, where running workloads count as pre-existing. Leaving `enforce` resumes everything the scheduler queued (`enforce_disabled`), never manual pauses.

**Fairness:** the next queued workload to resume is chosen by, in order: aged past `max_queue_wait` first, then the owning agent with the fewest running heavy workloads (its admission leases included), then the fewest previous preemptions, then the longest queued. `per_agent_limit` keeps one agent from holding every heavy slot. There is no user override, priority by workload class, or per-project fairness (not implemented).

**Explainability:** every decision emits the decision, target, reason code, observed pressure, relevant policy, and timestamp:

```json
{"decision":"queue","workload":"pid:4312","reason":"heavy_slots_exhausted","pressure":"high","running_heavy":2,"heavy_limit":2}
```

**Safety invariant:** the scheduler never signals a process unless ownership confidence satisfies the control policy, the process is eligible, the feature is enabled, and the platform supports the signal semantics.

## Platforms

Platform code implements *capabilities* (host metrics, process enumeration, ancestry, per-process resources, suspend/resume, priority) instead of pretending every OS is identical. The scheduler queries capabilities and degrades explicitly.

- **macOS** — first target: enumeration, ancestry, CPU/RSS, memory/swap, process-group handling. No undocumented APIs on the stable path. Throt avoids cgo, so per-process data comes from `/bin/ps`, and host CPU busy time is derived from per-process CPU deltas. That figure reads low when many processes live shorter than one sample interval. Names (`comm`) are cached by PID and start time and read for newly seen processes in-process from `sysctl kern.procargs2` (argv[0], the string `ps` prints; about 25 µs each, no fork). Only other users' processes, which that sysctl refuses, still cost a `ps -p` each (a full names pass when more than 16 are new), and every 15th sample re-reads every name so an `exec` is noticed. Daemon overhead, measured with the samplers called directly (`THROT_OVERHEAD=60s go test ./internal/platform -run Overhead`) on an Apple M5 Pro (15 CPUs) with about 1,200 processes and other agents active: one `ps -axww` costs about 33 ms of CPU however few columns or processes it is asked for (it reads every process either way; 66 ms at 3,150 processes) and is about 60–70% of a sample. A process sample costs 50–85 ms of CPU, ps included, which is 10–17% of one core at a 500 ms interval and 3–4% at the default 2 s (60–100 ms before names moved to `kern.procargs2`, when a burst of more than 16 new PIDs cost a 52 ms names pass instead of under 10 ms). The stats pass stays on `/bin/ps` because it is setuid root: `proc_pidinfo` is reachable without cgo through the raw `proc_info` syscall, but refuses CPU time and RSS of other users' processes (about 250 here, `launchd`, `WindowServer` and the like), which the host CPU estimate needs. On 15 CPUs the samplers moved the 10 ms probe's p95 and p99 by less than the run-to-run noise (p95 1.1–2.0 ms idle, about 10 ms with every CPU busy, either way). The kernel memory-pressure verdict is read with `sysctl kern.memorystatus_vm_pressure_level` (xnu `NOTE_MEMORYSTATUS_PRESSURE_*`: 1 normal, 2 warning, 4 critical); a failed read or unexpected value marks it missing.
- **Linux** — native procfs/cgroup facilities, not macOS semantics. Host metrics come from `/proc/stat`, `/proc/loadavg` and `/proc/meminfo`. Cgroup v2 limits apply only when the daemon is containerized, which Throt takes to mean that its `0::` path in `/proc/self/cgroup` is `/`, the root of a private cgroup namespace, where the agents share its limits. That cgroup is resolved against the cgroup2 mount in `/proc/self/mountinfo`, and its limits replace the host figures. With a `cpu.max` quota below the host CPU count, CPU busy is the larger of `cpu.stat` usage over quota capacity and the fraction of enforcement periods throttled (`nr_throttled`/`nr_periods`). A container throttled every period therefore reads fully busy, and one throttled only now and then reads its usage. Load is reported missing under such a quota: the load average is host-wide (Linux keeps none per cgroup), and divided by the quota's CPUs it would count work outside the cgroup. With `memory.max`, each limited level's headroom is its limit minus `memory.current` less `inactive_file`. The level with the least headroom wins, with its limit (capped at host RAM) as total and its headroom (capped at host available) as available, so a nearly full parent binds before a roomier child with a smaller limit. Swap works the same way with `memory.swap.max` and `memory.swap.current`. The cgroup is located once, and the limits are re-read every sample. Under systemd, a unit's own `CPUQuota=`, `MemoryMax=` or similar hardening confines throtd alone, not the agents it watches, so a daemon in a unit cgroup such as `/system.slice/throtd.service` keeps host-wide figures. The same holds for a container without its own cgroup namespace. Any cgroup read failure falls back to the host figure for that metric. `cpuset` limits and cgroup v1 are not read. There is no kernel memory-pressure verdict yet (reported missing). PSI is read from `/proc/pressure/{memory,io,cpu}`; under the same rule, the least-headroom `memory.max` level (the one the memory figures use) supplies `memory.pressure` and the cgroup whose `cpu.max` quota binds supplies `cpu.pressure`, falling back to the host file on a read failure. I/O has no cgroup limit rule, so its PSI is always host-wide (inside a container `/proc/pressure` is not namespaced, so it is the host's or the VM's). Processes come from `/proc/<pid>/stat` each sample; `/proc/<pid>/status` (the uid) is read only for processes new by PID and start time, plus a rolling 1/15 of the rest so a `setuid` is noticed. Daemon overhead, measured in a container (`golang:1.26.10-alpine`, `docker run --init`, Docker Desktop on an Apple M5 Pro with 10 VM CPUs) with the samplers called directly (`THROT_OVERHEAD=60s go test ./internal/platform -run Overhead`, which tops the table up with idle `sleep` processes): a sample costs about 11 ms of CPU at 1,000 processes and 35 ms at 3,000, which is 0.5% and 1.7% of one core at the default 2 s (32 ms and 63 ms, 1.5% and 3.1%, when `status` was read for every process each sample), with about 3 allocations per process (28 before). The 10 ms probe's p95 and p99 stayed within noise (2.5 ms and 3–3.5 ms either way).
- **Windows** — not committed before 1.0.

## Benchmarks

A scheduler can make graphs calmer while making real work slower, so changes to default behavior need evidence.

- **Responsiveness:** foreground probe latency (p95/p99), time in severe pressure, swap growth.
- **Throughput:** total wall-clock, heavy jobs/hour, build/test durations.
- **Scenarios:** 1 agent + 1 build; 4 agents with light work; 6–8 agents with overlapping builds; Docker + JVM + JS builds; memory-heavy; long job mixed with short jobs (starvation).
- **Method:** fixed fixtures, warm-up, repetitions, same machine for baseline vs. Throt, report medians and tails.

### Results

`make bench` runs `internal/bench` in a `golang` Alpine container matching `go.mod` (the results below were measured with Go 1.26.10), limited with `--cpus=2`, where the PID namespace keeps the enforce-mode daemon away from the host's agents. It builds three helpers: an agent stub named `claude`, a job named `make` that burns one core for about 5 s of calibrated CPU work, and a probe that sleeps 10 ms in a loop and records how late it wakes. The in-process daemon uses the real Linux samplers and controller with a 500 ms sample interval. Baseline is `scheduler.mode` off; Throt is enforce with `heavy_limit` 2 and `resume_cooldown` 1s, other settings default. Makespan runs from the first job start to the last exit; peak running is sampled every 100 ms from `/proc`; interventions are scheduler decisions (queue and resume) in the event log.

Medians of three runs on an Apple M5 Pro (macOS 26.5.1), Docker Desktop 29.5.3, linuxkit 6.12.76 arm64 VM with 10 CPUs and 7.75 GiB, with cgroup-aware sampling and CPU/load capped at elevated:

| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Peak running | Interventions | Max pressure |
|---|---|---|---|---|---|---|---|---|
| 8 jobs at once | baseline | 1.7ms | 72.0ms | 80.4ms | 19.0s | 8 | 0 | elevated |
| 8 jobs at once | Throt | 1.8ms | 4.5ms | 5.7ms | 22.2s | 8 | 12 | elevated |
| 10 jobs, one per 1s | baseline | 1.6ms | 73.1ms | 83.4ms | 27.7s | 10 | 0 | elevated |
| 10 jobs, one per 1s | Throt | 1.7ms | 4.2ms | 5.1ms | 27.2s | 3 | 16 | elevated |

Makespan varied by up to about 30% between runs (single runs ranged 17.8–19.7 s and 18.4–25.1 s for the burst, 25.9–34.1 s and 23.2–34.5 s staggered), so only the medians are meaningful. Probe percentiles were stable within about 1 ms. Peak running is 8 for the burst because Throt can only queue work that already exists, so all 8 jobs run for up to one sample interval before 6 are paused. Most of the baseline tail is cgroup throttling: with 8 runnable jobs the container spends its 200 ms of quota early in each 100 ms period and then everything, the probe included, waits. A plain 2-core host would likely show a smaller gap. Compare modes, not absolute times; the quota and the job calibration are approximate.

Under this setup Throt cuts probe p95 and p99 about 15×, from 70–85 ms to 4–6 ms, with no measurable change at p50. Staggered arrivals (closer to agents starting builds over time) finish in about the same time. A burst of simultaneous jobs takes about 17% longer, because each slot handoff waits up to one sample interval plus the cooldown.

An earlier policy let saturated CPU alone raise pressure to high. Under a real quota, two jobs on two CPUs then read as high, the adaptive limit halved to 1, and queued work waited minutes (burst 2m26s, staggered 11m7s in one run). CPU and load now cap at elevated, and the latency probe carries responsiveness.

**Default decision:** the scheduler stays `off` by default. It trades some burst throughput for much better tail responsiveness, and whether that trade is right depends on the workload, so turning it on is an explicit choice (`scheduler.mode`, optionally with a `profile`).

### Real machines

The container above shares a VM with Docker Desktop and enforces its CPU limit through a cgroup quota. To measure without either, the `Bench` workflow (`.github/workflows/bench.yml`) runs the same benchmark on hosted `ubuntu-latest` and `macos-latest` runners, which are disposable VMs, so the enforce-mode daemon is safe there and macOS gets a real XNU kernel. It runs only on demand and weekly, never on push or pull request:

```bash
gh workflow run bench.yml
gh run watch    # then read the job summary, or download the bench-<os> artifacts
```

Outside a container the benchmark runs only with `CI=true`, and sizes the contention to the runner, since there is no CPU limit: the burst launches 4× `runtime.NumCPU()` jobs at once, and the staggered stream launches as many at intervals of 5 s / (2 × CPUs), so its arrival rate asks for about twice the machine. `heavy_limit` is max(2, CPUs/2). On macOS, peak running comes from `ps -o state=,comm=` instead of `/proc`. Each row is still the median of three runs, and the table header gives the OS, kernel, CPU count and `heavy_limit`. `THROT_BENCH_JOBS` and `THROT_BENCH_REPS` shrink a run for a quick check.

First CI run ([workflow run 40710901573](https://github.com/cfcromn/Throt/actions/runs/40710901573), GitHub-hosted runners, CPU-only jobs, enforce with the default `balanced` profile):

linux/amd64, Linux 6.17.0-1022-azure, 2 CPUs, heavy_limit 2, 5s of CPU per job, medians of 3 runs

| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Makespan range | Peak running | Interventions | Max pressure |
|---|---|---|---|---|---|---|---|---|---|
| 8 at once | baseline | 0.9ms | 3.0ms | 4.9ms | 21.3s | 21.3s–21.5s | 8 | 0 | elevated |
| 8 at once | Throt | 0.1ms | 1.0ms | 2.0ms | 23.1s | 23s–23.2s | 8 | 12 | elevated |
| 8 staggered 1.25s | baseline | 0.1ms | 2.1ms | 3.9ms | 22.4s | 21.9s–22.7s | 8 | 0 | elevated |
| 8 staggered 1.25s | Throt | 0.1ms | 1.0ms | 2.0ms | 23.6s | 23.5s–23.6s | 3 | 12 | elevated |

darwin/arm64, Darwin 25.6.0, 5 CPUs, heavy_limit 2, 5s of CPU per job, medians of 3 runs

| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Makespan range | Peak running | Interventions | Max pressure |
|---|---|---|---|---|---|---|---|---|---|
| 20 at once | baseline | 1.0ms | 1.1ms | 2.2ms | 20.6s | 20.4s–21s | 20 | 0 | elevated |
| 20 at once | Throt | 1.0ms | 3.9ms | 6.5ms | 52.2s | 51.2s–52.7s | 20 | 36 | elevated |
| 20 staggered 500ms | baseline | 2.0ms | 2.5ms | 4.9ms | 23.7s | 23.5s–24.5s | 19 | 0 | elevated |
| 20 staggered 500ms | Throt | 3.7ms | 8.1ms | 8.1ms | 46.7s | 46.6s–48.2s | 3 | 36 | elevated |

These results are less favorable than the container run, and they matter more:
- **Linux runner:** without cgroup throttling the baseline tail is already small. Throt halves p99 (4.9 → 2.0 ms burst) for 5–8% more makespan. That's a small benefit at a small cost.
- **macOS runner: Throt is worse.** Makespan is 2.5× longer (20.6 → 52.2 s), because `heavy_limit` 2 leaves 3 of 5 cores idle while the macOS scheduler handles CPU oversubscription well. Probe tails are higher too (p99 2.2 → 6.5 ms), most likely the daemon's own `ps` sampling at the 500 ms bench interval.
- **The adaptive limit never grew.** Saturated CPU holds pressure at elevated, and AIMD only raises the limit at normal pressure.
- **Only CPU contention was measured.** Memory pressure, Throt's main target, is covered by the memory scenarios below.

This strengthens the default decision: the scheduler stays off, and CPU-only contention on a bare-metal kernel is not a reason to enable it.

### Memory pressure

The scenarios above are CPU-only. Throt's main target is memory: several agents' builds that together exceed RAM, so the machine swaps and the foreground freezes. `TestBenchMemory` measures that case. Its job (the same `make` helper, with `-mem`) allocates 22% of the memory limit, writes every word with incompressible pseudo-random data (so neither zswap nor the macOS compressor can shrink it), then writes one word per page in a loop for about 6 s of uncontended work, calibrated alone before the runs, and exits. A swapped-out or paused job does the same work, only later. Eight such jobs want 176% of the limit, launched at once ("memory burst") or one per second ("memory staggered"). Baseline is `scheduler.mode` off; Throt is enforce with the default `balanced` profile (`heavy_limit` max(2, CPUs/4), adaptive, `resume_cooldown` 10s), only the sample interval set to 500 ms as above. The columns are those above plus peak swap used and swap growth (peak minus the last sample before the first job), both from the daemon's own host sampler, which is cgroup-aware in a container, and the jobs that hit their cap or were killed by a signal (the OOM killer).

These scenarios deliberately exhaust memory, so they are guarded more tightly than the CPU ones:
- They need `THROT_BENCH_MEMORY=1` as well as `THROT_BENCH=1`, and run only on a CI runner (`CI=true`) or in a Linux container with a memory limit. They never run on a developer machine outside a container.
- In a container they size from the cgroup limit (`memory.max`, or `memory.limit_in_bytes` on cgroup v1), never the host, and skip when there is none. On CI they size from `hw.memsize` (macOS) or `MemTotal` (Linux).
- Every job exits by itself after 3 minutes of wall time (status 3, counted as capped), so a lost job cannot hold memory for long; a job paused past its cap exits as soon as it is resumed. A run fails after 7 minutes, which leaves room for `max_queue_wait`. Cleanup kills the agent stub's whole process group, jobs included, stopped or not.

The `Bench` workflow runs them after the CPU scenarios. On a hosted runner they push the VM into swap and possibly the OOM killer; that is acceptable only because the runner is thrown away.

**Container results**, not a real machine: two runs on the same Docker Desktop VM as above, with `docker run --memory=1g --memory-swap=2g --cpus=2 --init` (1 GiB of RAM and 1 GiB of swap for the container, 225 MiB per job). With two runs, each column shows the higher of the two:

| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Makespan range | Peak running | Interventions | Max pressure | Peak swap | Swap growth | Capped / killed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| memory burst, 8 at once | baseline | 1.7ms | 2.5ms | 22.2ms | 3m0.4s | 3m0.2s–3m0.4s | 8 | 0 | critical | 1023 MiB | 1023 MiB | 7 / 1 |
| memory burst, 8 at once | Throt | 1.6ms | 3.6ms | 6.9ms | 2m53.3s | 1m7.9s–2m53.3s | 8 | 16 | high | 1 MiB | 0 MiB | 0 / 0 |
| memory staggered, 8 one per 1s | baseline | 1.6ms | 7.4ms | 52.0ms | 1m2.1s | 44.3s–1m2.1s | 8 | 0 | critical | 867 MiB | 865 MiB | 0 / 0 |
| memory staggered, 8 one per 1s | Throt | 1.8ms | 2.4ms | 3.9ms | 1m8.4s | 1m7.1s–1m8.4s | 3 | 12 | elevated | 788 MiB | 784 MiB | 0 / 0 |

In the burst, the baseline fills the container's swap and thrashes: in both runs no job finished its work, seven hit the 3-minute cap and the OOM killer took the eighth, so its 3m0s makespan is the cap, not completion. With Throt every job finished, with almost no swap, but in 1m8s in one run and 2m53s in the other. Staggered, both modes finish; Throt cuts probe p99 from 52 ms to 4 ms for about 10% more makespan, but swap barely drops, because a paused job keeps its memory and the kernel swaps it out instead. Two runs in one container are not enough to settle the variance, and a container's memory limit is not a whole machine running out of RAM: the probe was never swapped out and stayed responsive at p50 in every mode. The CI runs are the real-machine test.

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
| `status` | – | `pressure`, `reason`, `host`, `agents`, `heavy`, `paused`, `scheduler`, `degraded`, `orphans` and `orphan_count` (stopped processes throtd did not pause, at most 50 listed) | no |
| `stats` | – | `since` (window start: the first process sample since the daemon started), `agents` (per agent instance, most CPU first: `agent`, `process`, `cpu_seconds`, `peak_rss`, `heavy_started`, `queued_seconds`, `first_seen`, `last_seen`; at most 256), `total`, `unattributed` (same fields) | no |
| `events` | `limit` (0 or > 1000 means 1000); optional `since` (a `seq`) | without `since`, the newest `limit` events, oldest first: `seq`, `instance`, `time`, `kind`, `message`, `attrs`. With `since`, up to `limit` events with a greater `seq`, oldest first; poll again with the last `seq` to get the rest. `seq` starts at 1 and grows by one per event; it restarts with the daemon, so a `since` beyond the newest event returns the newest events and the client sees `seq` go down. `instance` identifies the daemon run; a change means it restarted, even if the new run already passed the old `seq` | no |
| `pause` | `pid`; optional `start` (RFC 3339), refused if the process now has a different start time | – | yes |
| `resume` | `pid`; optional `start`, as for `pause` | – | yes |
| `reload` | – | `changed`, `requires_restart` (each `"key old -> new"`) | yes |
| `acquire` | `pid`: the caller's own PID. throtd checks it against the peer PID and resolves the caller's start time and parent from the OS; `start` and `ppid`, sent by earlier clients, are ignored | `granted`, `reason` (`nested` for a caller below a heavy workload or a lease holder, granted without a lease; `too_many_leases` when 256 leases exist), `lease_id`, `verdict` (as `throt_capacity`). Granting records an admission lease, shown in `status` as `scheduler.leases` (`id`, `process`, `agent`, `granted`); a repeated call by a holder returns its lease. Refused when the caller cannot be identified, has no start time, or belongs to another user | yes |

Privileged methods also require the caller's uid (from peer credentials) to match the daemon's. For them the server also reads the caller's PID from the kernel (`SO_PEERCRED` on Linux, `LOCAL_PEERPID` on macOS) and `acquire` refuses a `pid` other than the caller's own, so a client cannot reserve a slot for another process. Where no PID is available (other platforms, or a peer in another PID namespace, which Linux reports as 0) that check is skipped and the client-sent `pid` is resolved instead, which must name a live process of the daemon's user; the uid check still applies. The Go types in `internal/app` are the reference definitions; `throt mcp` exposes the read-only methods to coding agents.

## Compatibility

Before 1.0, the CLI, config and protocol may change between minor releases, always documented in the changelog; patch releases don't intentionally break documented behavior.

From 1.0, the stable surface is:
- `throt` and `throtd` commands, subcommands and flags;
- config keys, their meaning and defaults (unknown keys stay errors);
- local protocol v1: its methods, params and result fields;
- `--json` output (`throt stats`, `throt config show`, `throt version`) and the JSON lines of `throt events`;
- MCP tool names (`throt_status`, `throt_capacity`, `throt_events`, `throt_stats`, `throt_explain`, `throt_wait_for_capacity`) and their input and output schemas;
- exit codes: 0 on success (including `-h`), 1 on an error, 10 from `throt update --check` when an update is available, and for `throt run` the command's own status, 126 when it cannot be executed (or is found only through `.` in `PATH`) and 127 when it is not found.

Rules:
- Additive changes are allowed in minor releases: new commands, flags, config keys, protocol methods, optional params, result and JSON fields, MCP tools and optional tool inputs. Clients and scripts must ignore unknown fields.
- A breaking change (removing or renaming any of the above, changing a meaning, default or type, or making something optional required) needs a new major version. For the protocol it can instead be a protocol bump with negotiation: today a version mismatch is only reported with both versions, so a v2 must add negotiation and keep serving v1 clients.
- A deprecation lasts at least one minor release, during which the old form keeps working and prints a warning (on stderr, in `throt config check`, or as an event), and the changelog names the replacement.

Not covered: `internal/` packages (not a public API), log text, event message wording (event kinds and attrs are part of the protocol), and the TUI layout. `throt` and `throtd` must be the same release.

## Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | Split daemon (`throtd`) and client (`throt`). | Scheduling survives a closed TUI; one source of truth; future clients (MCP) reuse the API. Cost: IPC versioning and a security boundary. |
| 2 | Observe before automatic control. | Wrong attribution could pause unrelated work. v0.1 is observe-only; automatic control follows proven attribution, metrics, and benchmarks. |
| 3 | Local-first privacy. | No outbound telemetry; scheduling never needs prompts, source, output, or environment. |
