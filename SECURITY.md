# Security & Privacy

## Reporting a vulnerability

**Do not open a public issue for an undisclosed vulnerability.** Use [GitHub private vulnerability reporting](https://github.com/cfcromn/Throt/security/advisories/new).

Include the affected version/commit, impact, reproduction steps, platform, and a suggested mitigation if known. Please don't access data that isn't yours while validating a report.

Before 1.0, fixes target the latest released minor version.

## Security-sensitive areas

Extra review is required for local IPC, process signaling/control, privilege boundaries, config/file permissions, command execution, install/update paths, agent/MCP integration, and any data export.

## Design stance

- Throt runs without elevated privileges and never silently broadens them.
- The local socket is owner-only; process-control methods require the caller to own the target process.
- Unknown or ambiguously attributed processes are observe-only.

## Privacy

Throt is local-first. It collects only the metrics needed to operate, keeps them on the machine, and has **no outbound telemetry**. It does not read or store prompts, model responses, source files, command output, or environment variables.

Diagnostic output (`throt doctor`) is produced only on explicit request and redacts home paths, usernames, environment variables, and command arguments likely to contain secrets.

Any future telemetry requires a public proposal, documented fields, opt-in behavior, and retention/deletion instructions.
