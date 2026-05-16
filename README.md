# Bosun

> *"The bosun runs the work crew on deck while the captain charts the course."*

Coordinate parallel Claude Code (or any) sessions on isolated git worktrees, with one place to see what's happening and clean merge-back when work is done.

## What it does

```
$ bosun init 4
Created 4 sessions:
  session-1  → ../myproject-bosun-1  (branch: bosun/session-1)
  session-2  → ../myproject-bosun-2  (branch: bosun/session-2)
  session-3  → ../myproject-bosun-3  (branch: bosun/session-3)
  session-4  → ../myproject-bosun-4  (branch: bosun/session-4)

$ bosun status
SESSION    BRANCH               AHEAD  DIRTY  LAST_COMMIT
session-1  bosun/session-1      2      0      23s ago — implement auth handler
session-2  bosun/session-2      1      3      1m ago  — add data layer
session-3  bosun/session-3      0      0      —       — (no commits)
session-4  bosun/session-4      4      0      8s ago  — refactor http routing

$ bosun merge
session-1: ✓ merged (squashed 2 commits)
session-2: ⏭  skipped — has uncommitted changes (`bosun show session-2` to inspect)
session-3: ⏭  skipped — no commits ahead
session-4: ✓ merged (squashed 4 commits)
```

## Why

If you've ever run 3–4 Claude Code sessions in parallel on the same repo, you've hit these problems:

- Either everyone's on `main` (collisions) or each on its own branch with no coordination
- You context-switch between N terminals just to know what's happening
- The integration step at the end reveals conflicts you could have avoided
- When sessions step on each other, recovery costs more than the work would have

Bosun solves all four by giving each session an isolated git worktree, surfacing live state in one place, providing clean merge-back, and (since v0.2) exposing live cross-session coordination as MCP tool calls.

## Install

```
go install github.com/jasondillingham/bosun/cmd/bosun@latest
```

Or build from source — see `Makefile`.

## Commands

```
bosun init [N | label...] [--brief plan.md] [--launch] [--isolate-cache]
                            Create worktrees + branches (numeric N or named
                            labels); optionally drop per-session briefs and
                            spawn agent sessions
bosun launch <session>      Spawn a launcher window for an existing session
bosun status [--with-overlaps] [--watch] [--json] [--summary-only]
                            Print a table of session states + path collisions
bosun show <session> [--json]
                            Inspect one session's brief, claims, recent commits
bosun claim <session> <paths...>
                            Session declares paths it's editing (advisory)
bosun done <session>        Session signals it is ready to merge
bosun merge [<session>...] [--dry-run]
                            Squash-merge DONE sessions back to base
bosun remove <session>      Tear down a session cleanly
bosun cleanup [--orphans]   Reap sessions that no longer have work to keep
bosun list [--ready] [--json]
                            Print session names (--ready for DONE only)
bosun config show|set|...   Inspect or edit .bosun/config.json
bosun mcp [--socket path]   Run the MCP server (foreground)
bosun tui                   Bubbletea control center
bosun serve [--port N]      HTTP dashboard with SSE event stream
```

## Requirements

- Git on PATH
- Go 1.25+ to build (the MCP SDK requires it; v0.1 was Go 1.23+)

Runs on macOS, Linux, and Windows (x86_64 + arm64 binaries available).

## Status

**v0.4.0-rc1 shipped 2026-05-16.** See `RELEASES.md` for the full history, `SPEC.md` for the v0.1 implementation spec, and `CLAUDE.md` if you're a Claude Code session contributing to this codebase.

## Roadmap

- **v0.1** — init/status/show/claim/done/merge/remove/list. Filesystem-based coordination. Optional brief fan-out + session launcher.
- **v0.2** — MCP server interface: `bosun_claim` / `bosun_release` / `bosun_done` / `bosun_stuck` / `bosun_announce` / `bosun_check` tool calls, plus polish (`--summary-only`, `bosun launch`, `cleanup --orphans`, dependency-aware briefs, non-Ghostty tab support).
- **v0.3** — Bubbletea TUI control center (`bosun tui`), HTTP dashboard (`bosun serve`), custom session labels, agent process detection, cross-process claims `flock`.
- **v0.4** *(current)* — Lifecycle hooks scaffolding, `merge --dry-run`, `list/show --json`, web brief preview + events feed, orphan-dir recovery, `bosun config`.
- **v0.5** — Remaining hook call-sites (pre-merge / post-merge / pre-cleanup / post-cleanup / pre-remove), kickoff robustness (per-op timeouts, progress reporting), predictive conflict analysis (heuristic). See `docs/v0.5-roadmap.md`.

## License

MIT — see `LICENSE`.
