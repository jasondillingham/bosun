# Bosun

> *"The bosun runs the work crew on deck while the captain charts the course."*

[![CI](https://github.com/jasondillingham/bosun/actions/workflows/ci.yml/badge.svg)](https://github.com/jasondillingham/bosun/actions/workflows/ci.yml)
[![Fuzz](https://github.com/jasondillingham/bosun/actions/workflows/fuzz.yml/badge.svg)](https://github.com/jasondillingham/bosun/actions/workflows/fuzz.yml)
[![Stress](https://github.com/jasondillingham/bosun/actions/workflows/stress.yml/badge.svg)](https://github.com/jasondillingham/bosun/actions/workflows/stress.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go version](https://img.shields.io/github/go-mod/go-version/jasondillingham/bosun)](go.mod)
[![Latest release](https://img.shields.io/github/v/release/jasondillingham/bosun)](https://github.com/jasondillingham/bosun/releases)

Coordinate parallel Claude Code (or any) sessions on isolated git worktrees, with one place to see what's happening and clean merge-back when work is done.

## Try this in 5 minutes

```sh
git clone https://github.com/jasondillingham/bosun.git ~/bosun-demo
cd ~/bosun-demo && go build -o ~/bin/bosun ./cmd/bosun
bosun tour
```

`bosun tour` walks you through `init`, parallel edits, `predict`, `merge`, and `cleanup` on a throwaway repo — no setup, no agent launching, no Anthropic API key. About 5 minutes.

**Watch a recording** of the auto-driven tour (`BOSUN_TOUR_AUTO=1 bosun tour`):

![bosun tour demo](demo.gif)

Higher-fidelity playback options:
- Interactive player at [asciinema.org/a/aPMDJsNbseBdi307](https://asciinema.org/a/aPMDJsNbseBdi307) (lets you pause / scrub / copy text out)
- Local: `asciinema play docs/assets/bosun-tour.cast`

### `bosun tui` — interactive control center

`bosun tui` is the Bubbletea control center: one screen for every session, with keybinds to merge, cleanup, remove, launch, and preview briefs without leaving the table.

<!-- TODO: capture a real PNG at docs/assets/tui-screenshot.png when a TTY is handy. -->

```
Bosun control · 4 sessions · 2 DONE · 2 WORKING · 8 ahead

   SESSION    BRANCH           STATE    AHEAD  DIRTY  CLAIMED  LAST
   session-1  bosun/session-1  DONE     3      0      2        1m ago  · implement auth handler
 ▸ session-2  bosun/session-2  WORKING  1      4      1        14s ago · add data layer
   session-3  bosun/session-3  DONE     4      0      3        2m ago  · write integration tests
   session-4  bosun/session-4  WORKING  0      0      0        —       — (no commits)

Recent activity
  session-3  [done]   ready to merge — 4 commits squashed
  session-2  [claim]  internal/data/, cmd/bosun/cmd_status.go
  session-1  [merge]  merged — 3 commit(s) squashed

status: merge session-1: merged — 3 commit(s) squashed
j/k move · m merge · M merge-all · c cleanup · r remove · l launch · s brief · R refresh · q quit
```

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

## Install

**Download a prebuilt binary.** Tagged releases publish darwin/linux/windows × amd64/arm64 archives on the [Releases page](https://github.com/jasondillingham/bosun/releases/latest). One-liner that grabs the right archive for the current host, extracts the `bosun` binary, and drops it on `PATH`:

```sh
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/jasondillingham/bosun/main/scripts/install.sh | sh
```

```powershell
# Windows (PowerShell)
iwr -useb https://raw.githubusercontent.com/jasondillingham/bosun/main/scripts/install.ps1 | iex
```

Prefer not to pipe a script straight into a shell? Both installers are short and self-contained — [`scripts/install.sh`](./scripts/install.sh) and [`scripts/install.ps1`](./scripts/install.ps1) — read them first, then run locally. Or grab the archive for your OS/arch directly from the [Releases page](https://github.com/jasondillingham/bosun/releases/latest) and extract by hand.

**From source (Go 1.25+):**

```
go install github.com/jasondillingham/bosun/cmd/bosun@latest
```

Or build from source — see `Makefile`. Full install options (prebuilt binary, `go install`, build from source, Homebrew when available) live in [`docs/installing.md`](./docs/installing.md).

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

### Alternative agents (Ollama, Docker, …)

Bosun's default agent is Claude Code (`claude`), but the agent
command is per-repo and per-session configurable. Point it at a
wrapper script — for example, one that hands the session off to a
local Ollama server or runs Claude inside a Docker container — and
bosun launches that instead.

```sh
# Repo-wide default
bosun config set agent_command examples/agent-wrappers/ollama-aider.sh

# Per-session override via the brief
cat > plan.md <<'EOF'
## session-1 (command: examples/agent-wrappers/ollama-aider.sh)
Routine refactor — cheap local model is fine.

## session-2
Architecture work — falls back to the config default (claude).
EOF
bosun init 2 --brief plan.md
```

Starter wrapper scripts live in [`examples/agent-wrappers/`](examples/agent-wrappers/);
read the [README there](examples/agent-wrappers/README.md) for the
contract and known limitations. The design that motivates the
feature is [`docs/agent-command-design.md`](docs/agent-command-design.md).

For Docker-isolated sessions, bosun ships a native `docker` launcher
(no wrapper script needed):

```sh
bosun config set launcher docker
bosun config set docker.image ghcr.io/your-org/bosun-agent:latest
bosun init 4   # each session now runs in its own container
```

Bosun composes `docker run --rm -it -v <worktree>:/work ...` and
hands it to your OS terminal launcher. Worktree + MCP socket are
bind-mounted; operator-configured `docker.extra_mounts` and
`docker.env_passthrough` cover credentials and runtime config. See
[`docs/sandbox-launcher-design.md`](docs/sandbox-launcher-design.md)
for the design and the deferred items (detached mode, in-container
self-registration without mounting `bosun` itself).

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

## Supported platforms

| OS | Status | Notes |
|---|---|---|
| **macOS** (Intel + Apple Silicon) | ✓ primary development target | iCloud-path refusal in `bosun init`; see below |
| **Linux** (x86_64) | ✓ tested on Ubuntu 25.04 (kernel 6.14) | `bosun tour` + `bosun doctor` + full init/merge/cleanup cycle validated end-to-end |
| **Windows** | ⚠ not yet supported in v0.10 | Builds compile but the terminal launcher only knows about Ghostty / Terminal.app / gnome-terminal. Windows Terminal / cmd.exe / WSL integration is post-v0.10 work. Compile-only CI may land sooner. |

**macOS users:** keep the bosun project **out of** `~/Documents/`, `~/Desktop/`, and `~/Library/Mobile Documents/` — all are iCloud-synced by default, and iCloud File Provider strips git's worktree admin metadata under load. `bosun init` refuses these paths by default (override with `--force-icloud` if you've disabled iCloud sync for the dir). `bosun doctor` catches and recovers the corruption shape if you hit it. See [`docs/macos-setup.md`](./docs/macos-setup.md) for the full guide and the recipe to relocate an existing repo out of iCloud.

## Comparison

Bosun overlaps with a few neighbors. Honest tradeoffs below — if any one of these is already what you want, stay there.

**vs. raw `git worktree`.** `git worktree add` plus your own tmux/terminal discipline handles two branches fine. Bosun is heavier — a binary, a `.bosun/` directory, a coordination model — and only earns its weight once you're juggling 3+ lanes and want one table to see them all, plus a merge orchestrator and a documented safety contract. If you're managing two branches, raw worktrees are the right answer.

**vs. Claude Code's `Agent(isolation: "worktree")`.** That tool delegates *one task* to a sub-agent inside one conversation; the worktree dies when the sub-agent returns. Visibility is parent-to-child only; nothing outside that pair can see the work. Bosun is for *N persistent* sessions that survive Ghostty restarts and laptop reboots, with `bosun status` / `bosun tui` showing them all in one place, predict-before-merge (`bosun predict`), reflog-based undo (`bosun merge --undo`), and `bosun rescue` for CRASHED state. **When the isolated Agent wins:** one-shot tasks that fit in a single conversation — the Agent tool is the lighter answer there.

**vs. hosted / cloud AI-coordinators (Devin, Cursor background agents, etc.).** Most of those are aimed at a single agent doing more work autonomously, often in a managed sandbox. Bosun is a local CLI for the operator-in-the-loop case: you launch the agents, they coordinate via local files + MCP, and nothing leaves your machine. **When they win:** if you want hosted/cloud execution, async work-while-you-sleep, or a polished web UI, bosun isn't competing.

## FAQ

**How is this different from running `git worktree` myself?**
You can build this yourself with `git worktree add`, a few tmux panes, and discipline. Bosun is the packaged version — one table for status, declarative claims so sessions know what each other is editing, a squash-merge orchestrator with reflog undo, and a documented [safety contract](#safety-contract--what-bosun-does-to-your-repo). If you already have a workflow you like, keep it.

**What if I'm not using Claude Code?**
Bosun is agent-agnostic. The CLI surface (`init`, `status`, `merge`, …) works for any agent or human you put in a worktree — Cursor, Aider, your own shell, a teammate, a script. The MCP tools are the only Claude-Code-shaped piece, and they're optional.

**Does bosun talk to GitHub (or any forge)?**
No. The safety contract is explicit: bosun never pushes, fetches, or talks to a forge. Worktrees, branches, and squash-merges are all local. You push when *you* push.

**Can I use bosun on Windows?**
Not in v0.10. Builds compile, but the terminal launcher only knows Ghostty / Terminal.app / gnome-terminal — Windows Terminal / cmd.exe / WSL integration is post-v0.10 work. See the [Supported platforms](#supported-platforms) table.

**Is bosun safe for production codebases?**
The safety contract holds — bosun never touches `main` except via `bosun merge`, never pushes, never modifies global git config. It's been trialed end-to-end through SIGBUS, CRASHED state, corrupted-gitdir recovery, and `merge --undo` reflog reset ([`docs/v0.8-trial-findings.md`](./docs/v0.8-trial-findings.md), [`docs/v0.9-trial-3c-findings.md`](./docs/v0.9-trial-3c-findings.md)). Honest caveat: zero external users so far — try it on a side project first.

**How do I undo a `bosun merge`?**
`bosun merge --undo <sha>` resets your base branch to a prior SHA via the reflog, but only when `main` hasn't advanced past it. If you've already pushed or rebased past the merge, recovery is on you — the reflog is the source of truth.

**What happens if a session crashes mid-work?**
The session goes to CRASHED state. `bosun rescue <session>` snapshots its dirty files to `.bosun/rescues/` so nothing is lost, and can relaunch the window. `bosun doctor` is the first thing to run if anything looks off.

## Status

**Validated end-to-end.** Safety contract held across SIGBUS, CRASHED state, corrupted-gitdir recovery, and `merge --undo` reflog reset in trial #2 (`docs/v0.8-trial-findings.md`). The v0.9 spawn-tree machinery — hierarchical labels, `merge --tree` post-order cascade, dotted-label worktree naming — held in trial #3c (`docs/v0.9-trial-3c-findings.md`). Issue #15 (macOS iCloud worktree-admin corruption) has a foundational fix: `bosun init` refuses iCloud-managed paths by default, `bosun doctor` detects the corruption shape, and `bosun doctor --fix` recovers it. The fix's empirical validation gate is "a real user hits this and the doctor catches it" — see issue #15. All 23 packages green under `make check`; `make fuzz` and `make stress` clean; cross-OS validated on macOS + Ubuntu 25.04.

**Not yet validated.** Zero external users. Three v0.9 trials (#3, #3a, #3b, #3c) ran on a maintainer-owned repo on a maintainer's machine. The "stranger picked it up and shipped real work" signal flips this from "compelling prototype" to "this graduates." Until then: treat the safety contract as load-bearing trust and the rest as well-tested-but-unprovenfor your specific workflow.

See `RELEASES.md` for full version history, `SPEC.md` for the v0.1 implementation spec, and `CLAUDE.md` if you're a Claude Code session contributing to this codebase.

## Used by

**Pre-launch — this slot is reserved for community usage.**

So far, bosun is in real use on:

- The maintainer's day-to-day workflow (the bosun repo itself dogfoods bosun for its own parallel-session development — every release has shipped under bosun coordination).
- Release-prep work for [`architect-mcp`](https://github.com/jasondillingham/architect-mcp).

If you've shipped real work with bosun, open a PR adding your project here — an honest one-liner about how you used it is plenty. No logos required; no "trusted by 500+ teams" marketing inflation.

## Roadmap

- **v0.1** — init/status/show/claim/done/merge/remove/list. Filesystem-based coordination. Optional brief fan-out + session launcher.
- **v0.2** — MCP server interface: `bosun_claim` / `bosun_release` / `bosun_done` / `bosun_stuck` / `bosun_announce` / `bosun_check` tool calls, plus polish (`--summary-only`, `bosun launch`, `cleanup --orphans`, dependency-aware briefs, non-Ghostty tab support).
- **v0.3** — Bubbletea TUI control center (`bosun tui`), HTTP dashboard (`bosun serve`), custom session labels, agent process detection, cross-process claims `flock`.
- **v0.4** — Lifecycle hooks scaffolding, `merge --dry-run`, `list/show --json`, web brief preview + events feed, orphan-dir recovery, `bosun config`.
- **v0.5** — All hook call-sites wired (pre-merge / post-merge / pre-cleanup / post-cleanup / pre-remove), kickoff robustness (per-op timeouts, progress reporting), predictive conflict analysis (`bosun predict`), `bosun suggest` brief authoring.
- **v0.6** — Resilience anchor: agent-liveness gate on destructive ops, pre-merge `git fsck`, reflog-based `merge --undo`, CRASHED state + `bosun rescue`, heartbeat MCP tool, hook timeout enforcement, init resumability (`bosun init --resume`), README "Safety contract" section.
- **v0.7** — Polish round: launch UX, predictor accuracy (Files-avoid exclusion), pre-flight robustness (stale-branch refusal in init, load check at merge), state+rescue resilience (Spotlight phantom filter, corrupted-gitdir recovery, salvage on `remove --force`). Plus a bug-hunt wave: proc detection via cmdline, MCP goroutine leak, rescue salvage error surfacing, cleanup/merge silent-error fixes. Refactors: `internal/phantom`, `internal/lockfile`. Fuzz + stress test targets via `make fuzz` and `make stress`.
- **v0.8** — Public-launch readiness: `bosun doctor` system preflight, `init --suggest` for one-step onboarding, external-repo trial #2, README + LICENSE + RELEASES.md catchup.
- **v0.9** — Agent-driven coordination: `bosun_spawn` MCP tool, spawn-tree data layer with hierarchical session labels (`session-1.auth`), tree-shaped `status` / `show` / `list`, `cleanup --tree` cascade, `merge --tree` post-order, PreToolUse hook for auto-claim.
- **v0.10** — "Somewhat solid from day one." Phase 1 macOS reliability: detect + refuse iCloud-managed paths, recover from issue #15 admin-dir corruption via `bosun doctor --fix`. Phase 2A agent UX: `bosun_spawn` context-isolation pitch reframe, `bosun_check_tree` tool, structured `.bosun/audit/spawn.log`, v1.0 sub-task spec. Phase 3 first-5-minutes: `bosun tour` interactive walkthrough, `bosun new-brief --pattern` scaffolding, README quickstart, demo asset.

## License

Apache 2.0 — see [`LICENSE`](./LICENSE).
