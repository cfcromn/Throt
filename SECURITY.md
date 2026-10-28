# Security & Privacy

## Reporting a vulnerability

**Do not open a public issue for an undisclosed vulnerability.** Use [GitHub private vulnerability reporting](https://github.com/cfcromn/Throt/security/advisories/new).

Include the affected version/commit, impact, reproduction steps, platform, and a suggested mitigation if known. Please don't access data that isn't yours while validating a report.

Before 1.0, fixes target the latest released minor version. From 1.0, security fixes ship as a patch release of the latest minor version of the current major; older minors and majors get no fixes, and upgrading within a major is safe under the compatibility policy in `docs/ARCHITECTURE.md`.

## Security-sensitive areas

Extra review is required for local IPC, process signaling/control, privilege boundaries, config/file permissions, command execution, install/update paths, agent/MCP integration, and any data export.

## Design stance

- Throt runs without elevated privileges and never silently broadens them.
- The local socket lives in an owner-only directory and is mode 0600; client and daemon both verify the directory before use. Process-control methods additionally verify the caller's uid via peer credentials (`SO_PEERCRED` / `LOCAL_PEERCRED`).
- `throt resume --force PID` may resume (SIGCONT) any stopped process the user owns, for example work an earlier throtd left stopped; it is identity-verified and uid-checked like every control call, and stopping (SIGSTOP) can never be forced.
- Process control only targets heavy workloads attributed to one of the user's own agents, and verifies every process's identity (PID, start time, uid) in the same platform call that signals it. On Linux the process is pinned with a pidfd before it is verified, so a reused PID cannot be signaled. Kernels without pidfds and macOS verify and then `kill(2)`, which leaves a window of one `/proc` read or `ps` call; see "Control" in `docs/ARCHITECTURE.md`.
- Paused processes are recorded in an owner-only `paused.json` before they are stopped. If throtd crashes they stay stopped until throtd restarts and resumes them, or the user sends `kill -CONT`.
- Unknown or ambiguously attributed processes are observe-only.

## Security review

**1.0 review of IPC and process control, 2026-10-28**, at `5347e21` (`feat/v08`). Scope: `internal/transport`, `internal/app` (handlers, leases, reload, `paused.json`), `internal/control`, `internal/platform` (signaling, pidfd, `ps` identity, `Identify`), `cmd/throt` (`run`, `hook`), and the CI/release workflows. Threat model: another local user, a malicious same-user process, a crashed or killed daemon, PID reuse, races in the socket directory, malformed protocol input, a hostile `paused.json`. Method: line-by-line reading, targeted regression tests, and adversarial tests on Linux in `golang:1.26.10-alpine` (a real foreign uid against a real server through `SO_PEERCRED`: refused at `connect` by the 0600 socket, and refused at the uid check for `pause` when the socket was left open).

| Boundary | Verdict |
|---|---|
| Socket creation and permissions | Sound after fix 1. The directory is created 0700 and must be a real directory (`Lstat`), owned by the user, and not group/other-writable, checked by both daemon and client; a `/tmp/throt-UID` pre-created by another user is refused (denial of service only). The `flock` instance lock makes a leftover socket provably stale; only a socket is ever removed. Lock and state files are created inside the checked directory. |
| Authorization | Sound. `pause`, `resume`, `reload`, `acquire` require the peer uid (`SO_PEERCRED`, `LOCAL_PEERCRED` with xucred version check) to equal the daemon's; an unreadable uid denies, and unsupported OSes always deny. Unprivileged methods are read-only and emit no events. `acquire` binds the lease to the kernel-reported peer PID where one exists. |
| Signaling | Sound. Every signal names one PID > 1 that is not throtd, and is sent only after PID, start time and uid are verified; a mismatch sends nothing. Linux pins the process with a pidfd before verifying. A failed pause rolls back child-first, and anything not resumed stays recorded and is retried. The kernel independently forbids signaling other users' processes. |
| State files | Sound. `paused.json` is decoded strictly (unknown fields, trailing data, versions, states, PIDs, counts all checked) before any signal; malformed files are moved aside and nothing is signaled. Writes use `CreateTemp` (O_EXCL, 0600) and `rename` in the private directory, so planted symlinks are replaced, not followed. Restore only ever continues. Stats are memory-only. |
| `throt run` | Sound. argv is exec'd directly, never through a shell; `exec.LookPath` refuses commands found through `.` in `PATH`; the environment passes through unchanged. A fake daemon needs the user's own private directory and can at most delay the command until `--timeout` or let it run. Every failure runs the command. |
| DoS | Sound after fix 2. 16 concurrent connections, a 5 s I/O deadline, 1 MiB messages, and bounded events (ring), leases (256), paused records (64 × 4096), orphans (50), stats rows (256), `ps` output (32 MiB) and hook input (1 MiB). Only the user can connect, so log flooding needs a same-user process. |
| Supply chain | Acceptable. Every action is pinned by commit SHA; workflows default to `contents: read`; the Go toolchain is pinned in `go.mod`; CI runs `govulncheck`, CodeQL and race tests; releases ship `checksums.txt` and SBOMs. |

Findings, all fixed with regression tests:
1. *Socket briefly open to others (low).* The socket was created with the process umask and only then set to 0600, and the directory check allows group/other traversal, so under a umask such as 002 another user could connect in between and read `status`, `events` and `stats`. It is now created under a 077 umask.
2. *Hung `ps` could stall the server (medium).* `acquire` called `Identify` with the request context, which has no deadline. A hung `ps` held the handler and its connection slot forever, and repeated `throt run` polls could take all 16, leaving `status`, `pause` and `resume` unserved. Each lookup now has the 2 s per-call deadline every other OS call has.

Accepted residual risks:
- **macOS and pidfd-less Linux verify, then `kill(2)`.** The window is one `ps` call (median 13 ms, p95 about 45 ms measured on macOS 26; at most the 2 s call deadline) or one `/proc` read. Hitting a wrong process needs the PID space (99,999 on macOS) to wrap within it and the new process to be the user's own with a start time in the same second. Documented under "Control" in `docs/ARCHITECTURE.md`.
- **Same-user processes are trusted.** They can already signal the user's processes, so the daemon does not defend against them: they can call privileged methods, reserve leases, forge `paused.json` (making throtd continue up to 64 × 4096 of the user's own stopped processes, or stall startup with a FIFO or a huge file), or edit the config. Where the kernel reports no peer PID (another PID namespace), `acquire` trusts the client-sent PID of a process the user owns.
- **Directory trust is by mode bits.** ACLs are not inspected, and a custom `socket_path` must not sit under a directory other users can write to (the default `$XDG_RUNTIME_DIR`, `/tmp` with its sticky bit, and the macOS per-user `$TMPDIR` are safe).
- **Releases are unsigned** and carry no provenance attestation; verify `checksums.txt` over HTTPS from GitHub. The release job uses `goreleaser` `~> v2` (not an exact version) and the Go module cache.

**`throt update`, 2026-10-29** (`internal/update`, `cmd/throt/update.go`). Threat model: a network attacker, a tampered or truncated download, a hostile archive, an interrupted or partial install, an unwritable install directory, and leaking the GitHub token.

| Aspect | Verdict |
|---|---|
| Transport | Sound. Every request, redirects included, must be HTTPS (plain `http` is refused), with Go's default TLS verification against `api.github.com` and the asset host it redirects to; a whole-request timeout and at most 5 redirects. The token is sent only as an `Authorization` header to the API host: Go drops it when the asset download redirects to another host. It is never printed, logged or put in an error. |
| Integrity | Sound. The archive for this OS and architecture is refused unless its SHA-256 equals its entry in the release's `checksums.txt`; a missing `checksums.txt`, a missing or malformed entry, or a mismatch refuses the update before anything is written. Bodies are capped (release list 4 MiB, `checksums.txt` 1 MiB, archive 100 MiB). |
| Archive | Sound. Read in memory; only `throt` and `throtd` are taken. The whole archive is refused when any entry is absolute or contains `..`, or is anything but a regular file or directory (symlinks, hard links, devices, FIFOs), when a binary is missing, duplicated or over 100 MiB, or when it unpacks to more than 256 MiB. |
| Replace | Sound. Each binary is written to an `O_EXCL` temporary file in its destination directory, made 0755 and synced, then run with a 10 s timeout (`throt version`, `throtd -version`); it must print the release version. Only when both pass are they renamed over the old files (atomic within a directory), `throtd` first and `throt` last, so a failure leaves either nothing changed or a new `throtd` with the error saying the two differ. An unwritable directory, a failed check, or any earlier error removes the temporary files and changes nothing. Symlinks to the binaries are resolved, so their targets are replaced and the links kept. |
| Refusals | No install without confirmation: a terminal prompt, or `--yes`; a non-terminal stdin without `--yes` refuses. `go install` and development builds are left to `go install`. It never downgrades unless `--version` asks for it, and never restarts `throtd`. |
| macOS | Files written by Go carry no `com.apple.quarantine` attribute, so no `xattr` step is needed; the binaries are unsigned as before. |

Accepted residual risk: `checksums.txt` and the archive come from the same release, so the check gives integrity (no corruption or tampering in transit, TLS aside) but not authenticity: whoever can publish a release in the repository can publish matching checksums. Once the repository is public, verify the build provenance too: `gh attestation verify throt_<version>_<os>_<arch>.tar.gz --repo cfcromn/Throt`.

## Privacy

Throt is local-first. It collects only the metrics needed to operate, keeps them on the machine, and has **no outbound telemetry**. It does not read or store prompts, model responses, source files, command output, or environment variables.

Diagnostic output (`throt doctor`) is produced only on explicit request. It never collects environment variables or command arguments, names no process except stopped ones throtd did not pause (by executable base name only), and redacts home paths and the user name.

`throt update` is the only command that uses the network, and only when you run it: nothing checks for updates automatically or in the background. It is not telemetry: it is an explicit user action that sends standard HTTPS requests to GitHub (the release list, `checksums.txt` and one archive), with your GitHub token when one is set, and nothing about the machine or its use beyond what any HTTPS client sends (address, `User-Agent: throt-update`).

Any future telemetry requires a public proposal, documented fields, opt-in behavior, and retention/deletion instructions.
