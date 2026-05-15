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

Bosun solves the first three by giving each session an isolated git worktree, surfacing live state in one place, and providing clean merge-back. (Live cross-session coordination is coming in v0.2.)

## Install

```
go install github.com/jasondillingham/bosun/cmd/bosun@latest
```

Or build from source — see `Makefile`.

## Commands

```
bosun init [N] [--brief plan.md] [--launch] [--isolate-cache]
                            Create N worktrees + branches; optionally drop
                            per-session briefs and spawn agent sessions
bosun status [--with-overlaps]
                            Print a table of session states + path collisions
bosun show <session>        Inspect one session's brief, claims, recent commits
bosun claim <session> <paths...>
                            Session declares paths it's editing (advisory)
bosun done <session>        Session signals it is ready to merge
bosun merge [<session>...]  Squash-merge DONE sessions back to base
bosun remove <session>      Tear down a session cleanly
bosun list [--ready]        Print session names (--ready for DONE only)
```

## Requirements

- Git on PATH
- Go 1.23+ to build

Runs on macOS, Linux, and Windows (x86_64 + arm64 binaries available).

## Status

**v0.1 in development.** See `SPEC.md` for the full implementation spec and `CLAUDE.md` if you're a Claude Code session contributing to this codebase.

## Roadmap

- **v0.1** *(current)* — init/status/show/claim/done/merge/remove/list. Filesystem-based coordination (claims + state). Optional brief fan-out + session launcher. Stdlib + Cobra.
- **v0.2** — MCP server replaces `.bosun/claims/` and `.bosun/state/` with structured tool calls. Same workflow shape.
- **v0.3** — Web dashboard for live monitoring.

## License

MIT — see `LICENSE`.
