# AGENTS.md

Instructions for coding agents working in this repository.

## Mission

Build Throt as a small, safe, explainable resource-aware runtime for local coding agents.

## Read first

Before modifying behavior, read:
1. `README.md`
2. `docs/ARCHITECTURE.md` (architecture, scheduler, platform, decisions)
3. `CONTRIBUTING.md` (development workflow and engineering rules)

## Hard constraints

- Do not add automatic process control to observe-only paths.
- Do not signal/kill/suspend unknown processes.
- Do not add outbound telemetry.
- Do not read/store prompts, source files, command output, or environment variables for metrics.
- Do not introduce elevated-privilege requirements without an approved design.
- Do not couple core scheduling logic to a specific agent vendor.
- Do not make scheduler decisions from a single noisy metric sample.
- Do not hide scheduler interventions from the event log.
- Do not introduce unbounded goroutines, queues, or retained histories.
- Do not commit scratch notes, plans, generated binaries, coverage files, or new top-level documents.

## Workflow

1. Inspect existing code/tests.
2. State the invariant affected by the change.
3. Make the smallest coherent change.
4. Add/adjust tests.
5. Run `make check`.
6. For scheduler behavior, run relevant benchmarks/simulations.
7. Summarize risks and any unsupported platform behavior.

## Architecture

Core domain/scheduler code must be deterministic and testable without real processes.

OS calls belong under platform adapters.

TUI is a client. Do not place scheduling policy in TUI code.

## Testing

Prefer table-driven tests for policy boundaries.

Use fake clocks and deterministic metric streams for pressure/scheduler tests.

Process-control changes require:
- positive case;
- ambiguous ownership case;
- process-exited race;
- cancellation/shutdown case.

## Dependencies

Do not add a dependency only to save a few lines. Explain new runtime dependencies in the PR.

## Documentation

Update docs when changing:
- CLI/config;
- scheduling semantics;
- privacy/security behavior;
- compatibility;
- architecture.

If the implementation contradicts `docs/ARCHITECTURE.md`, update the decision there first rather than silently diverging.
