# Contributing to Throt

Thanks for helping improve Throt. Be respectful: discuss code and ideas, not people. Harassment is not tolerated, and maintainers may remove content or restrict participation to protect the community.

## Before coding

Small bug fixes: open a PR directly.

Behavior changes, scheduler policy, platform support, protocol changes, or new dependencies: open an issue first and agree on the problem and acceptance criteria. Architectural decisions are recorded in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

Good first contributions: tests, diagnostics, TUI usability, process-classification fixtures, platform capability detection, reproducible bug cases. Please don't start with a scheduler rewrite.

## Development

Requirements: the Go version in `go.mod`, Git, Make. macOS is the primary platform; Linux is supported where noted.

```bash
make setup   # download modules
make check   # vet + test + build
make race    # tests with the race detector
make ci      # the unit-test part of CI (gofmt, vet, race tests, build)
make bench   # real-kernel benchmark, in Docker only
gh workflow run bench.yml   # the benchmark on Linux and macOS CI runners
gh workflow run soak.yml    # the soak test on Linux and macOS CI runners, and 30s of each fuzz target
```

**Never run an enforce-mode `throtd`, the e2e suite, or the benchmark on a machine with real agents.** It pauses their real builds. The e2e suite and the benchmark refuse to run outside CI or a container; on macOS, which has no container, they run only on CI. For real-machine numbers, use the `Bench` workflow (on demand and weekly, see Benchmarks in `docs/ARCHITECTURE.md`). Locally, run them in Docker, building the test binary first, because under `go test` the `go` process would own every job:

```bash
docker run --rm --init -v "$PWD":/src -w /src golang:1.26.10-alpine sh -c \
  'go test -c -o /tmp/e2e.test ./internal/e2e && THROT_E2E=1 /tmp/e2e.test -test.v'
```

The soak test (`internal/e2e/soak_test.go`) churns an enforce-mode `throtd` for 10 minutes (`THROT_SOAK_DURATION` to change it) and checks for stranded work, leaks, latency and the heavy limit. It is too long for every push, so the `Soak` workflow runs it on demand and weekly; locally, run it the same way with `docker run --rm --init --cpus=2 …` and `THROT_SOAK=1 /tmp/e2e.test -test.run=TestSoak -test.v -test.timeout=30m`.

The memory-pressure scenarios (`TestBenchMemory`) deliberately drive the machine past its RAM, so they also need `THROT_BENCH_MEMORY=1`, and outside CI they run only in a container with a memory limit, which they size from: `docker run --rm --init --cpus=2 --memory=1g --memory-swap=2g -v "$PWD":/src -w /src golang:1.26.10-alpine sh -c 'go test -c -o /tmp/bench.test ./internal/bench && THROT_BENCH=1 THROT_BENCH_MEMORY=1 exec /tmp/bench.test -test.run=TestBenchMemory -test.v -test.timeout=90m'`. Never run them on the host.

Fuzz targets run their seeds in `go test`; to fuzz one, use `go test -run='^$' -fuzz='^FuzzParsePS$' -fuzztime=60s ./internal/platform`. Don't commit generated `testdata/fuzz` corpora: turn a crasher into a table test instead.

Layout:

```text
cmd/throt      client (CLI, TUI, MCP server, run, completion)
cmd/throtd     daemon
internal/      app, agent, capacity, config, control, doctor, domain, mcp, monitor,
               platform, pressure, process, scheduler, shellwords, stats,
               transport, tui;
               bench and e2e (Docker/CI only)
docs/          ARCHITECTURE.md
```

## Engineering rules

- `gofmt`; errors add context and wrap causes (`%w`);
- no `panic` for recoverable failures; no package-level mutable state;
- interfaces live near consumers and stay small;
- every goroutine has an owner and a cancellation path; channels are bounded;
- scheduler/pressure tests are deterministic (fake clocks, scripted metrics), table-driven at policy boundaries;
- OS-specific code lives under `internal/platform` with capability tests;
- process-control changes need tests for: the positive case, ambiguous ownership, the process exiting mid-action, and shutdown;
- prefer the standard library; a new dependency must justify maintenance health, license, supply-chain and binary cost.

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org/): `feat(tui): add pressure timeline`, `fix(darwin): preserve ancestry after parent exit`.

A PR explains the problem, the approach, trade-offs, tests, user-visible changes, and — for process control — risk and rollback. Scheduler-default changes need benchmark evidence. PRs are squash-merged; CI must pass.

## Review checklist

- Is the change in scope and as small as it can be? Are failure modes explicit?
- Scheduler: what invariant changes? Can it starve or oscillate? What happens with missing/noisy metrics? Is the decision explainable?
- Process control: is ownership certain? What if the PID exits or is reused between observation and action? What if signaling fails?
- Privacy: is new data collected or exported? Are arguments, environment, or secrets exposed?

## AI-assisted code

Welcome, under the same standard as human-written code: don't submit code you can't explain. Agents follow [AGENTS.md](AGENTS.md).

## License

Contributions are accepted under the [MIT License](LICENSE). No CLA.
