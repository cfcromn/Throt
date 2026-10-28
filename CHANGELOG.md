# Changelog

All notable user-visible changes to Throt are documented here.

The format is inspired by Keep a Changelog and versions follow Semantic Versioning.

## [Unreleased]

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
