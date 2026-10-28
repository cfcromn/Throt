# Security & Privacy

## Reporting a vulnerability

**Do not open a public issue for an undisclosed vulnerability.** Use [GitHub private vulnerability reporting](https://github.com/cfcromn/Throt/security/advisories/new).

Include the affected version/commit, impact, reproduction steps, platform, and a suggested mitigation if known. Please don't access data that isn't yours while validating a report.

Before 1.0, fixes target the latest released minor version.

## Security-sensitive areas

Extra review is required for local IPC, process signaling/control, privilege boundaries, config/file permissions, command execution, install/update paths, agent/MCP integration, and any data export.

## Design stance

- Throt runs without elevated privileges and never silently broadens them.
- The local socket lives in an owner-only directory and is mode 0600; client and daemon both verify the directory before use. Process-control methods additionally verify the caller's uid via peer credentials (`SO_PEERCRED` / `LOCAL_PEERCRED`).
- Process control only targets heavy workloads attributed to one of the user's own agents, and verifies every process's identity (PID, start time, uid) in the same platform call that signals it. On Linux the process is pinned with a pidfd before it is verified, so a reused PID cannot be signaled. Kernels without pidfds and macOS verify and then `kill(2)`, which leaves a window of one `/proc` read or `ps` call; see "Control" in `docs/ARCHITECTURE.md`.
- Paused processes are recorded in an owner-only `paused.json` before they are stopped. If throtd crashes they stay stopped until throtd restarts and resumes them, or the user sends `kill -CONT`.
- Unknown or ambiguously attributed processes are observe-only.

## Privacy

Throt is local-first. It collects only the metrics needed to operate, keeps them on the machine, and has **no outbound telemetry**. It does not read or store prompts, model responses, source files, command output, or environment variables.

Diagnostic output (`throt doctor`) is produced only on explicit request and redacts home paths, usernames, environment variables, and command arguments likely to contain secrets.

Any future telemetry requires a public proposal, documented fields, opt-in behavior, and retention/deletion instructions.
