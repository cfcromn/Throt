# CLAUDE.md

Claude Code instructions for Throt.

Follow `AGENTS.md` as the canonical repository-wide agent policy.

Additional guidance:
- Before implementing a broad request, identify the smallest milestone that can be tested independently.
- Do not “improve” unrelated code while completing a focused issue.
- Never weaken process-control safety checks to make a test pass.
- Treat macOS/Linux behavior as capability-based, not assumed identical.
- When uncertain about a process's ownership, choose observe-only behavior.
- Keep TUI presentation separate from daemon policy.
- After changes, report commands run and tests that were not run.
