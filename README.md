# Bosun

> *"The bosun runs the work crew on deck while the captain charts the course."*

Coordinate parallel Claude Code (or any) sessions on isolated git worktrees, with one place to see what's happening and clean merge-back when work is done.

## Try this in 5 minutes

```sh
git clone https://github.com/jasondillingham/bosun.git ~/bosun-demo
cd ~/bosun-demo && go build -o ~/bin/bosun ./cmd/bosun
bosun tour
```

`bosun tour` walks you through `init`, parallel edits, `predict`, `merge`, and `cleanup` on a throwaway repo — no setup, no agent launching, no Anthropic API key. About 5 minutes.

[demo screencast →](TBD-asciinema-link)

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

## Safety contract — what bosun does to your repo

Bosun runs alongside your normal git workflow, on the same checkout you already use. The rules below describe exactly what it touches without being asked, what it does only when you ask, and what it never does at all.

**Without explicit command, bosun:**

- Creates branches under the `bosun/` prefix (e.g. `bosun/session-1`, `bosun/auth`). Your existing branches and `main` are not touched.
- Creates worktrees as sibling directories of your repo, named `<repo>-bosun-<session>` (e.g. `myproj-bosun-1`). Nothing is created inside the repo root other than `.bosun/`.
- Writes coordination state under `.bosun/` in your repo root: per-session claim files, DONE/STUCK markers, an MCP socket at `.bosun/mcp.sock`, and `.bosun/init.state` while `bosun init` is in progress. `.bosun/` is auto-added to `.gitignore`.

**Only on explicit command, bosun:**

- `bosun merge` squash-merges DONE sessions back to your base branch — the only action that touches `main`.
- `bosun remove <session>` and `bosun cleanup` delete the session's worktree and branch. Both default to safe-delete (`git branch -d`); `--force` switches to `-D`.
- `bosun cleanup --purge` discards committed work that hasn't been merged. Loud on purpose: this is the only path that can drop session commits.
- `bosun merge --undo` resets `main` to a prior SHA, and only when `main` hasn't advanced past it.

**Never, regardless of command, bosun:**

- Touches `main` outside of `bosun merge`.
- Writes outside `<repo>` and the `<repo>-bosun-*` sibling worktrees.
- Pushes to any remote, fetches from one, or talks to a forge (GitHub, GitLab, …).
- Modifies your global git config or your `user.{name,email}`.
- Modifies repo-level git config beyond what `git worktree add` already does.

## Why bosun, and not just Claude Code's worktree-isolated Agent?

Claude Code's `Agent(isolation: "worktree")` is for **delegating one task** to a sub-agent inside a single conversation. The sub-agent gets its own worktree, does the work, and returns when the sub-task completes — the worktree dies with it. Visibility is parent-to-child only; nothing outside that pair can see what the sub-agent is doing.

Bosun is for **coordinating N persistent sessions** across operator and agent restarts. Sessions live as long as the operator wants them to, survive Ghostty restarts and laptop reboots, and `bosun status` shows them all in one place — not one parent-child pair at a time.

Specific things bosun does that an isolated Agent can't:

- **Cross-session predict-before-merge** (`bosun predict`) — heuristic conflict detection across a plan's lanes before any work starts.
- **Merge orchestration with reflog undo** (`bosun merge --undo`) — squash-merge multiple sessions back to base and reset cleanly if it went wrong.
- **Safety contract** validated through SIGBUS, CRASHED state, and corrupted-gitdir trials (`docs/v0.8-trial-findings.md`).
- **`bosun rescue`** for the failure modes the agent can't recover from itself (CRASHED sessions, salvageable dirty trees).
- **`bosun doctor`** for environmental preflight before any session starts.
- **Agent-spawned sub-coordination** (v0.9 `bosun_spawn`, off by default) — sessions can spawn their own child sessions when a lane needs to fan out.

**When NOT to use bosun:** one-shot tasks that fit in a single agent's context. The wider CLI surface is only worth it once you're coordinating parallel work across sessions; for a single task, the Agent tool's worktree isolation is the lighter answer.

## Install

```
go install github.com/jasondillingham/bosun/cmd/bosun@latest
```

Or build from source — see `Makefile`.

## Quick start

```
# 1. Sanity-check your environment (one-time, recommended).
bosun doctor

# 2. Describe the work; bosun proposes the lane split for you.
bosun init 3 --suggest "add auth, refactor http routing, write tests"
# (writes .bosun/suggested-plan.md, creates 3 worktrees + branches,
#  and drops a per-session BOSUN_BRIEF.md in each)

# 3. Open an agent in each session (or use --launch on init).
bosun launch session-1
bosun launch session-2
bosun launch session-3

# 4. Watch what's happening.
bosun status         # one-shot table
bosun tui            # interactive control center

# 5. Each session, when ready, runs `bosun done`. Then:
bosun merge          # squash-merge every DONE session back to your base branch
bosun cleanup        # reap merged sessions
```

If anything looks off, `bosun doctor` is the first thing to try.

## Commands

```
bosun init [N | label...] [--brief plan.md | --suggest "<goal>"] [--launch] [--isolate-cache] [--force] [--resume]
                            Create worktrees + branches (numeric N or named
                            labels). --suggest generates a brief from a goal
                            description; --brief consumes a hand-written one.
bosun launch <session> [--initial-prompt "..."]
                            Spawn an agent window for an existing session
bosun status [--with-overlaps] [--watch] [--json] [--summary-only]
                            Print a table of session states + path collisions
bosun show <session> [--json]
                            Inspect one session's brief, claims, recent commits
bosun claim <session> <paths...>
                            Session declares paths it's editing (advisory)
bosun done <session>        Session signals it is ready to merge
bosun merge [<session>...] [--dry-run] [--undo <sha>] [--no-load-check]
                            Squash-merge DONE sessions back to base
bosun rescue <session> [--launch]
                            Recover a CRASHED session: snapshot its dirty
                            files to .bosun/rescues/, or relaunch a window
bosun remove <session> [--force]
                            Tear down a session cleanly; --force salvages
                            uncommitted files into .bosun/rescues/ first
bosun cleanup [--orphans] [--purge]
                            Reap DONE or empty sessions in bulk
bosun list [--ready] [--json]
                            Print session names (--ready for DONE only)
bosun config show|set|get|unset|init|validate
                            Inspect or edit .bosun/config.json
bosun predict <plan.md>     Heuristic conflict prediction across a plan's lanes
bosun suggest "<goal>"      Propose disjoint lanes for a goal; write a plan
bosun doctor                System health check before bosun goes to work
bosun mcp [--socket path]   Run the MCP server (foreground)
bosun tui                   Bubbletea control center
bosun serve [--port N]      HTTP dashboard with SSE event stream
```

## Requirements

- Git on PATH (>= 2.40)
- Go 1.25+ to build (the MCP SDK requires it)

Runs on macOS, Linux, and Windows (x86_64 + arm64 binaries available).
**macOS users:** keep the bosun project **out of** `~/Documents/` and `~/Desktop/` — both are iCloud-synced by default, which creates phantom-duplicate files inside worktrees. `bosun doctor` warns when it detects this. See [`docs/macos-setup.md`](./docs/macos-setup.md) for the full first-time-setup guide and the recipe to relocate an existing repo out of iCloud.

## Status

**Validated.** The safety contract held through SIGBUS, CRASHED state, corrupted-gitdir recovery, and `merge --undo` reflog reset during the v0.8 trial #2 against `homelab-status-mcp` — see [`docs/v0.8-trial-findings.md`](./docs/v0.8-trial-findings.md). 22 packages currently green under `make check`; `make fuzz` and `make stress` clean.

**Not yet validated.** Zero external users — bosun has not been used by anyone other than the maintainer on their own repo. v0.9's agent-spawn flow (`bosun_spawn`) has not been exercised externally; trial #3 is queued (see issue #7) and the public release of v0.9 is gated on it. v1.0 will be gated on accumulated external usage signal, not on shipping more features.

See `RELEASES.md` for full version history, `SPEC.md` for the v0.1 implementation spec, and `CLAUDE.md` if you're a Claude Code session contributing to this codebase.

## Roadmap

- **v0.1** — init/status/show/claim/done/merge/remove/list. Filesystem-based coordination. Optional brief fan-out + session launcher.
- **v0.2** — MCP server interface: `bosun_claim` / `bosun_release` / `bosun_done` / `bosun_stuck` / `bosun_announce` / `bosun_check` tool calls, plus polish (`--summary-only`, `bosun launch`, `cleanup --orphans`, dependency-aware briefs, non-Ghostty tab support).
- **v0.3** — Bubbletea TUI control center (`bosun tui`), HTTP dashboard (`bosun serve`), custom session labels, agent process detection, cross-process claims `flock`.
- **v0.4** — Lifecycle hooks scaffolding, `merge --dry-run`, `list/show --json`, web brief preview + events feed, orphan-dir recovery, `bosun config`.
- **v0.5** — All hook call-sites wired (pre-merge / post-merge / pre-cleanup / post-cleanup / pre-remove), kickoff robustness (per-op timeouts, progress reporting), predictive conflict analysis (`bosun predict`), `bosun suggest` brief authoring.
- **v0.6** — Resilience anchor: agent-liveness gate on destructive ops, pre-merge `git fsck`, reflog-based `merge --undo`, CRASHED state + `bosun rescue`, heartbeat MCP tool, hook timeout enforcement, init resumability (`bosun init --resume`), README "Safety contract" section.
- **v0.7** — Polish round: launch UX, predictor accuracy (Files-avoid exclusion), pre-flight robustness (stale-branch refusal in init, load check at merge), state+rescue resilience (Spotlight phantom filter, corrupted-gitdir recovery, salvage on `remove --force`). Plus a bug-hunt wave: proc detection via cmdline, MCP goroutine leak, rescue salvage error surfacing, cleanup/merge silent-error fixes. Refactors: `internal/phantom`, `internal/lockfile`. Fuzz + stress test targets via `make fuzz` and `make stress`.
- **v0.8** *(in progress)* — Public-launch readiness: `bosun doctor` system preflight, `init --suggest` for one-step onboarding, external-repo trial #2, CI on macOS/Linux/Windows, README + LICENSE + RELEASES.md catchup. After v0.8: the repo flips public.

## License

Apache 2.0 — see [`LICENSE`](./LICENSE).
