# Bosun v0.1.0 — Spec (as shipped)

**Status:** Shipped 2026-05-15. This document describes what v0.1.0 actually does. The original draft (scale-first rescope) is preserved by git history; this version reflects the additions surfaced during the dogfood iteration loop.
**Author:** Jason Dillingham
**Target language:** Go 1.23+
**Target OSes:** macOS (Intel + Apple Silicon), Linux (x86_64 + arm64), Windows (x86_64 + arm64)
**See also:** [`RELEASES.md`](./RELEASES.md) for the v0.1.0 changelog and [`docs/v0.2-roadmap.md`](./docs/v0.2-roadmap.md) for what's next.

---

## What is Bosun

A CLI tool that lets a single operator run **as many parallel Claude Code (or other coding-agent) sessions as the project can absorb**, each on its own isolated git worktree, with:

- one place to see what every session is doing
- a way to brief N sessions from a single plan
- early-warning signals when two sessions touch the same files
- clean squash-merge back to the base branch when sessions report done

**The metaphor:** a *bosun* (boatswain) is the ship's officer who runs the work crew on deck — assigns tasks, keeps order, reports to the captain. The user is the captain. Each session is a deckhand. Bosun coordinates the crew so the work happens in parallel without the captain having to context-switch between N terminals.

## The problem we're solving

When you scale parallel coding-agent sessions from 3-4 up to "as many as the project supports," you hit five problems:

1. **Branch chaos.** Everyone on `main` collides; everyone on ad-hoc branches drifts.
2. **Visibility blindness.** Context-switching across N terminals to know what's happening.
3. **Work assignment friction.** Briefing N sessions one-by-one becomes the bottleneck.
4. **Conflict-at-merge-time.** Two sessions independently edit the same file and you only find out at the integration step.
5. **Resource contention.** Parallel builds/tests fight over the same shared caches.
6. **Recovery cost.** Untangling stepped-on work takes longer than doing it sequentially.

**v0.1 of Bosun addresses all six**, with a deliberately small surface — filesystem-based primitives, no daemon, no protocol. v0.2 will replace the filesystem coordination with a structured MCP server while keeping the same workflow shape.

## v0.1 scope (what's in)

A Go CLI binary called `bosun` that exposes these subcommands:

```
bosun init [N] [--brief plan.md] [--launch] [--initial-prompt "..."]
              [--isolate-cache] [--force] [--from <branch>]
                            Create N worktrees + N branches; optionally drop briefs,
                            spawn agent sessions (with an auto-prompt pointing at the
                            brief), and isolate build caches.
bosun status [--with-overlaps] [--json] [--no-color]
             [--watch [--interval N]]
                            Print a one-line summary + table of session states. With
                            --watch, re-render every N seconds (default 2) until
                            interrupted (Ctrl-C). Includes claim-overlap detection.
bosun show <session>        Inspect one session's brief, claims, state, and recent
                            activity (last 10 commits + git status).
bosun claim <session> <paths...>
                            Session declares the paths it's currently editing.
                            Idempotent; advisory (surfaces via --with-overlaps).
bosun done <session> [--message "..."] [--force] [--stuck]
                            Session signals it is ready to merge (or STUCK with
                            --stuck). Refuses dirty or no-commits-ahead unless --force.
bosun merge [<session>...] [--all] [--no-squash] [--message "..."]
                            Squash-merge DONE sessions back to base. With --all,
                            attempt every session regardless of state. Skips sessions
                            whose content is already patch-id-equivalent to base (e.g.
                            after a prior squash).
bosun remove <session> [--force]
                            Tear down a session's worktree + branch. Auto-allows
                            removal without --force when the session's commits are
                            already on base by patch-id.
bosun cleanup [--dry-run] [--force]
                            Batch-remove DONE, empty, or already-merged sessions in
                            one shot. --dry-run previews; --force also removes dirty
                            or unmerged sessions.
bosun list [--ready]        Print just session names (for shell scripting).
```

That's the complete v0.1.0 command surface.

## v0.1.0 scope (what's NOT in)

These remain deferred to later versions. See [`docs/v0.2-roadmap.md`](./docs/v0.2-roadmap.md) for the planned next steps.

- ❌ MCP server interface — **v0.2** (filesystem claims/state cover this for now)
- ❌ Bubbletea TUI control center — **v0.2** (originally planned for v0.3; dogfood feedback pulled it forward)
- ❌ Real-time push/notification of session state between sessions — v0.2
- ❌ Predictive conflict analysis (knowing what a session *will* touch before it touches it) — v0.2
- ❌ Web dashboard — v0.3
- ❌ Auto-detection of "is an agent process actually running in this worktree?" — v0.3
- ❌ Hooks (pre-init, post-merge, etc.) — v0.4
- ❌ Custom session names beyond `session-N` — v0.2 (numbered only in v0.1.0)
- ❌ Dependency-aware work dispatch ("session-2 depends on session-1") — encode in the plan markdown if you need it
- ❌ Tab support for non-Ghostty terminals (Terminal.app, iTerm2, gnome-terminal, Windows Terminal) — v0.2 polish

**Note:** Watch mode (`bosun status --watch`) and the `cleanup` command were both deferred in the original draft but pulled forward during dogfood as friction at scale.

## Worktree + branch layout

If the user's repo is at `/Users/jdoe/code/myproject/`, after `bosun init 4` the layout is:

```
/Users/jdoe/code/
├── myproject/                  ← main worktree (cwd when running bosun)
├── myproject-bosun-1/          ← session 1's worktree
├── myproject-bosun-2/          ← session 2's worktree
├── myproject-bosun-3/          ← session 3's worktree
└── myproject-bosun-4/          ← session 4's worktree
```

Branches in the shared `.git` directory:

```
main                            ← base
bosun/session-1                 ← session 1's branch
bosun/session-2
bosun/session-3
bosun/session-4
```

**Naming rationale:**
- Worktree path suffix uses `-bosun-N` (dashes) to keep filesystem-safe across Windows
- Branch name uses `bosun/session-N` (slashes) to group in `git branch --list "bosun/*"`
- The `bosun/` prefix on branches lets us cleanly distinguish bosun-managed work from user branches

## State layout

### In the main repo (gitignored under `.bosun/`)

```
.bosun/
├── config.json                 ← optional config (see below)
├── claims/
│   ├── session-1.json          ← paths session-1 has claimed
│   ├── session-2.json
│   └── …
├── state/
│   ├── session-1.done          ← presence = ready to merge
│   ├── session-2.stuck         ← presence = blocked, needs attention
│   └── …
└── briefs/
    └── plan.last.md            ← last plan.md used by `bosun init --brief`
```

`bosun init` auto-adds `.bosun/` to the main repo's `.gitignore`, and if `--brief <path>` is used and the plan file lives inside the repo, it's added to `.gitignore` too. macOS symlink mismatch between cwd-relative paths and git-canonicalized paths is handled via `EvalSymlinks` before comparison.

### In each session worktree (excluded via `.git/info/exclude`, never committed)

```
<worktree>/
├── BOSUN_BRIEF.md              ← per-session brief with a workflow preamble
└── .claude/
    └── CLAUDE.md               ← pointer so Claude Code auto-loads the brief
```

`BOSUN_BRIEF.md` leads with a "How to work this session" preamble that walks the agent through the bosun lifecycle (read → implement → `make check` → commit → claim → done) before the per-session assignment. `.claude/CLAUDE.md` is a small auto-loader so any Claude Code session opened in the worktree starts by reading the brief, even when `--launch --initial-prompt` wasn't used.

Both files are written into the worktree but appended to that worktree's `.git/info/exclude` at init time, so `git add .` from the agent never picks them up — preventing the brief-conflict pattern that surfaced in round-1 dogfood.

### Optional config file at `.bosun/config.json`

```json
{
  "base_branch": "main",
  "session_prefix": "bosun",
  "worktree_suffix_pattern": "-bosun-{N}",
  "default_session_count": 4,
  "isolate_cache_default": false,
  "launcher": "auto"
}
```

`launcher` accepted values: `auto` (detect tmux → terminal → print-command), `tmux`, `terminal`, `print`. Defaults to `auto` if unset.

If the config file is absent, defaults apply (`main`, `bosun`, `-bosun-{N}`, 4, false, auto).

## Data model

Bosun is **mostly stateless** — git is the source of truth for code state, and `.bosun/` is the source of truth for coordination state. Per session, we derive:

| Field | Source |
|:---|:---|
| Session name | derived from worktree path suffix `-bosun-N` |
| Branch | `git -C <worktree> rev-parse --abbrev-ref HEAD` |
| Worktree path | `git worktree list --porcelain` |
| Last commit | `git -C <worktree> log -1 --format='%h|%ct|%ar|%s'` |
| AHEAD | `git -C <worktree> rev-list --count <base>..HEAD` |
| DIRTY | `git -C <worktree> status --porcelain` (filtered, see below) |
| STATE | filesystem: presence of `.bosun/state/session-N.{done,stuck}` |
| CLAIMED | filesystem: contents of `.bosun/claims/session-N.json` |

`DIRTY` counts tracked file changes (modified + added + deleted + renamed). Untracked files (`??`) are not counted — they're usually intentional build artifacts.

## Command behavior specifications

### `bosun init [N]`

**Arguments:**
- `N` (optional): number of sessions. Defaults to `config.default_session_count` or 4.
- `--brief <path>` (optional): path to a markdown plan file (see brief format below)
- `--launch` (optional): spawn an agent session in each worktree (see launcher below)
- `--isolate-cache` (optional): set per-worktree build-cache env vars when launching
- `--force` (optional): overwrite existing bosun worktrees

**Preconditions:**
- CWD must be inside a git repo
- The repo's HEAD must be on the base branch (refuse otherwise; user can override with `--force`)
- The worktree paths must not already exist (refuse otherwise; user can override with `--force`)

**Effect:**
- For i in 1..N:
  - Create branch `bosun/session-i` from base
  - Create worktree at `<repo-parent>/<repo-name>-bosun-i` checking out that branch
  - If `--brief` provided: extract the `## session-i` section from the plan markdown and write it as `<worktree>/BOSUN_BRIEF.md`
  - If `--launch` provided: spawn an agent session in the worktree (see launcher below)
- Print a summary of created sessions and worktree paths

**Idempotency:** Without `--force`, refuses to overwrite. With `--force`, removes existing bosun worktrees first.

**Exit codes:** 0 success, 1 user error, 2 git error, 3 internal error.

### `bosun status`

**Effect:** Print a table to stdout with one row per bosun-managed session:

```
SESSION    BRANCH               STATE    AHEAD  DIRTY  CLAIMED  LAST_COMMIT
session-1  bosun/session-1      DONE     2      0      3        23s ago — implement auth handler
session-2  bosun/session-2      WORKING  1      3      5        1m ago  — add data layer
session-3  bosun/session-3      WORKING  0      0      0        —       — (no commits)
session-4  bosun/session-4      STUCK    4      0      2        8s ago  — refactor http routing
```

**Column meanings:**
- `SESSION` — short session name (`session-N`)
- `BRANCH` — full branch name
- `STATE` — `WORKING` (default), `DONE` (`.bosun/state/session-N.done` exists), or `STUCK` (`.bosun/state/session-N.stuck` exists)
- `AHEAD` — commits ahead of base branch
- `DIRTY` — count of uncommitted tracked file changes
- `CLAIMED` — count of distinct paths in this session's claims file
- `LAST_COMMIT` — relative time + first 60 chars of subject (or `—`)

**Sort order:** by session number ascending.

**Flags:**
- `--with-overlaps`: after the table, print a list of file/path collisions across sessions' claims. Example:
  ```
  Overlapping claims:
    internal/auth/handler.go     session-1, session-3
    internal/http/router.go      session-2, session-4
  ```
- `--json`: emit machine-readable JSON (array of session objects with snake_case keys + a separate `overlaps` array)
- `--no-color`: disable color even on a TTY (also disabled if `NO_COLOR` env var is set)

### `bosun show <session>`

**Effect:** Print:
1. The session's branch + worktree path
2. The session's `BOSUN_BRIEF.md` if present
3. The session's current claims
4. The session's state marker (DONE/STUCK message if any)
5. Last 10 commits on the branch (`git log -10`)
6. Current `git status --short` output

### `bosun claim <session> <paths...>`

**Effect:**
- Writes/updates `.bosun/claims/<session>.json` to include the given paths plus a timestamp
- Paths are stored as repo-root-relative
- Idempotent: claiming an already-claimed path is a no-op
- Glob patterns (`internal/auth/**`) are stored as-is and expanded at overlap-check time

**Note:** Claims are advisory — they do not prevent edits. They only surface in `bosun status --with-overlaps` so the operator can intervene.

### `bosun done <session> [--message "..."]`

**Effect:**
- Refuse if the session has uncommitted changes (suggest committing or using `bosun show` to inspect; `--force` overrides)
- Refuse if the session has 0 commits ahead of base (use `bosun remove` instead; `--force` overrides)
- Touch `.bosun/state/<session>.done`, with optional message written into the file body
- Remove any `.bosun/state/<session>.stuck` marker if present

### `bosun merge [<session>...]`

**Arguments:** Zero or more session names (`session-1`) or short forms (`1`). If none specified, operate on all bosun sessions.

**Default behavior:** Only merge sessions where `.bosun/state/<session>.done` exists. Override with `--all` to attempt every session.

**Effect:**
- For each candidate session:
  - Refuse if it has uncommitted changes (skip, report)
  - Run `git merge --squash bosun/session-N` from the base-branch worktree
  - If conflict: report and stop the queue (don't commit; leave for the user to resolve)
  - If clean: commit with message `merge: bosun/session-N` (or `--message`)
  - On clean merge: remove `.bosun/state/<session>.done` and clear claims
- Print summary: which sessions merged cleanly, which had conflicts, which were skipped

**Flags:**
- `--ready-only` (default true): only merge sessions marked done
- `--all`: attempt every session regardless of done state
- `--no-squash`: use `--no-ff` regular merges instead of squash
- `--message <msg>`: override the commit message

**Safety:** Always operates on the base branch. Refuses if HEAD isn't on the base branch.

### `bosun remove <session>`

**Effect:**
- Refuse if session has uncommitted changes (`--force` to override)
- Refuse if session has commits ahead of base that haven't been merged (`--force` to override)
- Remove the worktree via `git worktree remove`
- Delete the branch via `git branch -d bosun/session-N` (or `-D` if `--force`)
- Clear `.bosun/claims/<session>.json` and `.bosun/state/<session>.*`

### `bosun list`

**Effect:** Print one session name per line.

**Flag:** `--ready` filters to sessions marked done.

## Brief format

`bosun init --brief plan.md` reads a markdown file with one `## session-N` heading per session. Anything between one heading and the next belongs to that session. Example:

```markdown
# Refactor plan

## session-1
Refactor `internal/auth/` to use the new identity provider.
Focus: handler.go, middleware.go. Don't touch the storage layer.

## session-2
Migrate `internal/storage/` from pgx v4 to v5. Update the pool config.

## session-3
Update the HTTP routing layer. Don't touch auth or storage.
```

Each `## session-N` body is written verbatim as `<worktree>/BOSUN_BRIEF.md`. If a session number is missing from the plan, that worktree just doesn't get a brief.

The original plan file is copied to `.bosun/briefs/plan.last.md` for reference.

## Launcher

`bosun init --launch` spawns an agent session in each newly-created worktree. The launcher tries strategies in order based on the `launcher` config:

| Strategy | Detection | Action |
|---|---|---|
| `tmux` | `tmux` on PATH **and** `$TMUX` set (we're inside a tmux session) | `tmux new-window -c <worktree>` running `claude` (with the prompt as a separate argv element when `--initial-prompt` is set) |
| `terminal` | Per-OS detection (see below) | Spawns a new terminal window/tab cd'd to the worktree, running `claude '<prompt>'` |
| `print` | always works | Print copy-pasteable shell commands; user runs them manually |

### Terminal-strategy detection order

1. **Ghostty** — `exec.LookPath("ghostty")` OR `/Applications/Ghostty.app/Contents/MacOS/ghostty` on macOS. Preferred when available because it's cross-OS and the most likely terminal a bosun operator is already running.
2. **macOS:** `osascript` → Terminal.app
3. **Linux:** `x-terminal-emulator`, `gnome-terminal`, `konsole`, `xterm` (in that order)
4. **Windows:** `cmd /c start cmd /K`

### Tabs vs windows

When the chosen terminal is Ghostty, the first session opens a new window and every subsequent session opens as a tab in that window (`ghostty +new-tab -e bash -lc "..."`). This is wired automatically by `bosun init` — no flag needed. Non-Ghostty terminals still spawn a separate window per session in v0.1.0; tab support for Terminal.app / iTerm2 / gnome-terminal / Windows Terminal is v0.2 polish.

### Spawning model

All terminal-spawning paths use `exec.Cmd.Start()` and reap the child in a background goroutine — never `Run()`, which would block until the spawned terminal exits and hang `bosun init` after the first session. `tmux new-window` exits immediately, so the tmux path keeps `Run()`.

`auto` (default) tries `tmux` → `terminal` → `print`.

### Initial prompt

`--initial-prompt "..."` is passed to the spawned `claude` as a positional argument so it becomes the agent's first message. When `--launch --brief` is set with no explicit `--initial-prompt`, bosun defaults to:

> `Read BOSUN_BRIEF.md in this directory — it's your assignment. Read it in full, then follow the workflow it describes.`

Single quotes in the prompt are POSIX-shell-escaped (`'\''`); double quotes are cmd.exe-escaped (`""`) on Windows.

### Isolate-cache

When `--isolate-cache` is set, the launcher exports these env vars in the spawned process:

- `GOCACHE=<worktree>/.cache/go-build`
- `GOMODCACHE=<worktree>/.cache/go-mod`
- `npm_config_cache=<worktree>/.cache/npm`
- `PYTHONPYCACHEPREFIX=<worktree>/.cache/pycache`
- `CARGO_TARGET_DIR=<worktree>/target`

Per-cache dirs are created on first launch. This trades disk space for parallel-safety.

## Architecture / file layout

```
bosun/
├── cmd/bosun/
│   └── main.go                 ← entry point; Cobra wiring
├── internal/
│   ├── git/
│   │   ├── git.go              ← thin wrapper around os/exec for git CLI
│   │   ├── worktree.go         ← worktree-specific operations
│   │   └── git_test.go
│   ├── session/
│   │   ├── session.go          ← Session struct + derivation
│   │   └── session_test.go
│   ├── status/
│   │   ├── status.go           ← table rendering
│   │   ├── status_json.go      ← JSON output
│   │   └── status_test.go
│   ├── claims/
│   │   ├── claims.go           ← claim/read/overlap detection
│   │   └── claims_test.go
│   ├── state/
│   │   ├── state.go            ← done/stuck markers
│   │   └── state_test.go
│   ├── brief/
│   │   ├── brief.go            ← parse plan.md, write BOSUN_BRIEF.md
│   │   └── brief_test.go
│   ├── launcher/
│   │   ├── launcher.go         ← tmux/terminal/print strategies
│   │   └── launcher_test.go
│   ├── config/
│   │   ├── config.go           ← defaults + .bosun/config.json loader
│   │   └── config_test.go
│   └── tui/
│       └── tui.go              ← color + tty detection
├── docs/
│   ├── DESIGN.md
│   └── v0.2-deferred.md        ← parking lot for ideas that don't belong in v0.1
├── .bosun/
│   └── config.example.json
├── .gitignore
├── Makefile
├── go.mod
├── go.sum
├── README.md
├── SPEC.md                     ← this file
├── CLAUDE.md
└── LICENSE
```

## Implementation notes

- **CLI library:** `github.com/spf13/cobra` (only third-party dep).
- **No third-party libs beyond Cobra.** Stdlib does everything else.
- **All git operations via `os/exec`.** Do NOT use `go-git` or git plumbing internals.
- **Output rendering:** `text/tabwriter` for the status table.
- **Color:** Detect TTY via `golang.org/x/term` (effectively stdlib) or roll your own via `os.Stdout.Stat()` + `os.ModeCharDevice`. Honor `--no-color` and `NO_COLOR`.
- **Path handling:** ALWAYS `path/filepath`, NEVER string concatenation.
- **Error wrapping:** `fmt.Errorf("%w", err)` consistently. User-facing errors get a `bosun: ` prefix.
- **Exit codes:** 0 success / 1 user error / 2 git error / 3 internal error.
- **Process spawning:** Use `exec.Command` with explicit env. Inherit stdout/stderr only when running `git`; detach when launching agent sessions.

## Cross-OS considerations

- **Path separators:** `filepath.Join`. Test on Windows in CI.
- **Line endings:** Don't make assumptions; git handles this.
- **Shell invocation:** Don't invoke shells. Invoke `git` directly via `exec.Command("git", args...)`.
- **Executable detection:** At startup, verify `git` is on PATH via `exec.LookPath("git")`. Friendly error if not.
- **Worktree path naming:** Sibling directories with `-bosun-N` suffix. Windows-safe.
- **Launcher fallbacks:** Always end at `print` so the command never hard-fails on an exotic OS.
- **CI matrix:** GitHub Actions matrix runs on `ubuntu-latest`, `macos-latest`, `windows-latest`.

## Acceptance criteria for v0.1.0

All 20 original criteria pass, plus the dogfood-driven extensions added during the v0.1.x iteration loop.

### Original 20

1. ✅ `bosun init 4` in a fresh git repo creates 4 worktrees + 4 branches as specified
2. ✅ `bosun init 4 --brief plan.md` writes a per-session `BOSUN_BRIEF.md` in each worktree
3. ✅ `bosun init 4 --launch` opens an agent session in each worktree (or prints fallback commands)
4. ✅ `bosun init 4 --launch --isolate-cache` sets per-worktree `GOCACHE`/`GOMODCACHE`/etc.
5. ✅ `bosun status` prints the expected table with STATE + CLAIMED columns
6. ✅ `bosun status --with-overlaps` detects two sessions claiming overlapping paths
7. ✅ `bosun show session-1` prints brief, claims, state, last 10 commits, git status
8. ✅ `bosun claim session-1 internal/auth/` persists, idempotent on repeat
9. ✅ `bosun done session-1` flips state to DONE; refuses if dirty or no commits ahead
10. ✅ `bosun merge` defaults to merging only DONE sessions, skips others with a reason
11. ✅ `bosun merge --all` attempts every session; conflicts are reported, not crashed
12. ✅ `bosun remove session-1` tears down worktree + branch + claims + state with safety checks
13. ✅ `bosun list` prints session names; `bosun list --ready` filters to DONE sessions
14. ✅ `make build` produces a single-binary executable for the host platform
15. ✅ `make cross` produces binaries for darwin/{amd64,arm64} + linux/{amd64,arm64} + windows/{amd64,arm64}
16. ✅ CI on GitHub Actions runs the test suite on `ubuntu-latest`, `macos-latest`, `windows-latest`
17. ✅ Unit test coverage ≥ 70% across `internal/`
18. ✅ Integration test exercises init → claim → commit → done → merge end-to-end against a temp git repo
19. ✅ README explains the use case in ≤ 30 lines
20. ✅ `bosun --help` prints clean usage text for each subcommand

### Added during dogfood (v0.1.x)

21. ✅ `bosun status --watch` re-renders the table on an interval; clean exit on SIGINT
22. ✅ `bosun status` prints a one-line summary above the table (state counts, total ahead, overlap count)
23. ✅ `bosun cleanup` batch-removes DONE / empty / squash-merged sessions; `--dry-run` and `--force` flags work
24. ✅ `bosun remove` auto-allows removal of post-squash-merge sessions via `git cherry` patch-id detection
25. ✅ `bosun merge` skips sessions whose content is already on base by patch-id (no retry-loop conflicts)
26. ✅ `bosun init --launch --initial-prompt "..."` passes through; defaults to "Read BOSUN_BRIEF.md..." when `--brief` is set
27. ✅ Ghostty is detected and preferred; first session opens a window, subsequent sessions open as tabs
28. ✅ `BOSUN_BRIEF.md` is excluded from the worktree's git index so `git add .` doesn't commit it
29. ✅ `.claude/CLAUDE.md` is auto-written so Claude Code sessions in the worktree load the brief without an external prompt
30. ✅ `bosun init --brief <path>` auto-adds the plan file to `.gitignore` when it lives inside the repo
31. ✅ Commands invoked from inside a linked worktree resolve to the main repo's `.bosun/` state correctly
32. ✅ Launcher uses `Start()` + background reap; `bosun init --launch` doesn't hang on terminals that don't fork

## Testing approach

- **Unit tests:** Git wrapper uses a `Runner` interface that can be mocked. Status renderer uses table-driven tests against fixture session data. Claims/state/brief packages have isolated filesystem tests against `t.TempDir()`.
- **Integration tests:** `t.TempDir()`-based git repo. Exercise commands end-to-end.
- **CI:** Three-OS matrix on GitHub Actions: ubuntu-latest, macos-latest, windows-latest. Each runs `go test -race ./... -count=1`.
- **Coverage gate:** `make test-cover` reports coverage; CI checks ≥ 70% on `internal/`.

## Open questions for the implementer

The implementer should resolve these and document decisions in the PR:

1. **Worktree paths: absolute or relative?** Recommendation: relative (`../<name>-bosun-N`) so the user can move the parent directory and not break worktrees. Verify this works on Windows.

2. **Should `bosun list` filter by bosun-managed branches only?** Recommendation: yes (filter branches starting with `bosun/`).

3. **Should `merge` operate on the user's current branch, or always switch to `base_branch`?** Recommendation: always operate on `base_branch`; refuse if HEAD isn't there.

4. **For DIRTY count, count untracked files?** Recommendation: no — filter `^??` lines.

5. **Claims: should we resolve globs at claim time or at overlap time?** Recommendation: store as-is, resolve at overlap time so the claim survives file rename/creation.

6. **What does the `bosun done` body look like?** Recommendation: optional message + ISO timestamp, newline-separated. Plain text. Just enough for `bosun show` to surface.

7. **Should `bosun init --from <branch>` be supported?** Recommendation: yes, defaults to `config.base_branch`.

8. **Launcher on Linux without a desktop terminal — does it just fall back to `print`?** Recommendation: yes. Headless Linux users (CI, containers) get the print strategy.

## Future versions

See [`docs/v0.2-roadmap.md`](./docs/v0.2-roadmap.md) for the planned next step. At a glance:

- **v0.2 (next):** Bubbletea TUI control center + MCP server. The TUI replaces `bosun status --watch` with a persistent operator dashboard; the MCP server replaces `.bosun/claims/` and `.bosun/state/` with structured tool calls so sessions coordinate directly. Workflow shape stays identical.
- **v0.3:** Web dashboard for live monitoring. Same data as `bosun status` but live-updating over HTTP.
- **v0.4:** Hooks (pre-init, post-merge), session profiles, named sessions, dependency-aware dispatch.

## Author's note (post-ship)

v0.1.0 shipped via a tight dogfood loop: implement → use bosun on bosun itself → catch friction → fix → use again. Three rounds, ~30 commits between the initial scaffold and tag. Highlights of what the loop surfaced (none of which were in the original spec):

- Agents don't read `BOSUN_BRIEF.md` on their own → workflow preamble + `.claude/CLAUDE.md` auto-loader
- Agents finish the work but skip commit + claim + done → preamble makes the lifecycle explicit, `--initial-prompt` auto-kicks
- Post-merge sessions cluttered status as "ahead but not DONE" → patch-id detection in `remove` + `cleanup` + `merge`
- Four scattered Ghostty windows is bad UX → first session opens a window, rest open as tabs
- `git merge --squash` writes conflict markers to stdout → error wrapper now includes both streams
- macOS `/var` ↔ `/private/var` symlink mismatch → `canonicalAbs` resolves both before computing relative paths

The filesystem coordination primitives in v0.1.0 are deliberately simple so they can be replaced wholesale by the v0.2 MCP server without breaking the operator workflow.
