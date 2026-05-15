# Bosun v0.1 — Implementation Spec

**Status:** Draft, ready for implementation.
**Author:** Jason Dillingham
**Target language:** Go 1.23+
**Target OSes:** macOS (Intel + Apple Silicon), Linux (x86_64 + arm64), Windows (x86_64 + arm64)

---

## What is Bosun

A CLI tool that lets a single operator run **N parallel Claude Code sessions on isolated git worktrees**, with visibility into what each session is doing, and clean merging back to a base branch when done.

**The metaphor:** a *bosun* (boatswain) is the ship's officer who runs the work crew on deck — assigns tasks, keeps order, reports to the captain. The user is the captain. Each Claude Code session is a deckhand. Bosun coordinates the crew so the work happens in parallel without the captain having to context-switch between four terminals.

## The problem we're solving

When you run 3–4 Claude Code sessions in parallel against the same repo, you hit the same problems every time:

1. **Branch chaos.** Either everyone's on `main` (collisions) or each on its own branch with no coordination.
2. **Visibility blindness.** You context-switch between N terminals to know what's happening in each session.
3. **Merge surprise.** At the end of a multi-session run, the integration step reveals conflicts you could have avoided.
4. **Resource conflicts.** Two sessions try to `go build` or run tests simultaneously and fight for the build cache.
5. **Recovery cost.** When sessions step on each other, untangling the mess takes longer than the work would have taken sequentially.

v0.1 of Bosun solves problems 1, 2, and 3 by giving each session an isolated git worktree, surfacing live state in one place, and providing clean merge-back. Problems 4 and 5 are deferred to v0.2+.

## v0.1 scope (what's in)

A Go CLI binary called `bosun` that exposes these subcommands:

```
bosun init [N]              Create N worktrees + N feature branches at sibling paths
bosun status                Print a table of session states
bosun show <session>        Tail one session's recent commits + uncommitted changes
bosun merge [<session>...]  Squash-merge specified sessions (or all) back to base branch
bosun remove <session>      Tear down a session's worktree cleanly (with safety checks)
bosun list                  Print just session names, one per line (for shell scripts)
```

That's the complete v0.1 command surface. Nothing more, nothing less.

## v0.1 scope (what's NOT in)

The following are explicitly deferred. The implementer should NOT add them to v0.1, even if tempted:

- ❌ MCP server interface — v0.2
- ❌ Live coordination between sessions (`bosun_lock`, `bosun_announce` tool calls) — v0.2
- ❌ File-level conflict prediction — v0.2
- ❌ Web dashboard — v0.3
- ❌ Auto-detection of "is a Claude Code process running in this worktree?" — v0.3
- ❌ Hooks (pre-init, post-merge, etc.) — v0.4
- ❌ Custom session names beyond `session-N` — v0.2 (numbered only in v0.1)
- ❌ Watch mode (`bosun status --watch`) — v0.2 (snapshot only in v0.1)
- ❌ Any TUI beyond plain text — v0.3

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
bosun/session-1                 ← session 1's branch (path-style for grouping in `git branch`)
bosun/session-2
bosun/session-3
bosun/session-4
```

**Naming rationale:**
- Worktree path suffix uses `-bosun-N` (dashes) to keep filesystem-safe across Windows
- Branch name uses `bosun/session-N` (slashes) to group in `git branch --list "bosun/*"`
- The `bosun/` prefix on branches lets us cleanly distinguish bosun-managed work from user branches

## Data model

Bosun is **mostly stateless** — git is the source of truth. We derive everything from git CLI calls:

| What | How we get it |
|:---|:---|
| List of sessions | `git worktree list --porcelain` + filter for `bosun/session-N` branch pattern |
| Each session's branch | `git -C <worktree> rev-parse --abbrev-ref HEAD` |
| Each session's last commit | `git -C <worktree> log -1 --format='%h|%ct|%ar|%s'` |
| Commits ahead of base | `git -C <worktree> rev-list --count <base>..HEAD` |
| Dirty file count | `git -C <worktree> status --porcelain` line count |
| Untracked file count | filtered subset of above |

Optional config file at `.bosun/config.json` in the repo root (gitignored):

```json
{
  "base_branch": "main",
  "session_prefix": "bosun",
  "worktree_suffix_pattern": "-bosun-{N}",
  "default_session_count": 4
}
```

If the config file is absent, sensible defaults apply (`main`, `bosun`, `-bosun-{N}`, 4).

## Command behavior specifications

### `bosun init [N]`

**Arguments:** Optional integer N. Defaults to `default_session_count` from config, or 4 if no config.

**Preconditions:**
- CWD must be inside a git repo
- The repo's HEAD must be on the base branch (refuse otherwise; user can override with `--force`)
- The worktree paths must not already exist (refuse otherwise; user can override with `--force`)

**Effect:**
- For i in 1..N:
  - Create branch `bosun/session-i` from base
  - Create worktree at `<repo-parent>/<repo-name>-bosun-i` checking out that branch
- Print a summary of created sessions and worktree paths

**Idempotency:** Without `--force`, refuses to overwrite. With `--force`, removes existing bosun worktrees first.

**Exit codes:** 0 success, 1 user error, 2 git error.

### `bosun status`

**Effect:** Print a table to stdout with one row per bosun-managed session:

```
SESSION    BRANCH               AHEAD  DIRTY  LAST_COMMIT
session-1  bosun/session-1      2      0      23s ago — implement auth handler
session-2  bosun/session-2      1      3      1m ago  — add data layer
session-3  bosun/session-3      0      0      —       — (no commits)
session-4  bosun/session-4      4      0      8s ago  — refactor http routing
```

**Column meanings:**
- `SESSION` — short session name (`session-N`)
- `BRANCH` — full branch name
- `AHEAD` — commits ahead of base branch
- `DIRTY` — count of uncommitted file changes (modified + added + deleted, but not untracked)
- `LAST_COMMIT` — relative time + first 60 chars of commit subject (or `—` if no commits past base)

**Sort order:** by session number ascending.

**Output format:** plain text by default; `--json` flag emits machine-readable JSON; `--no-color` disables color even on a TTY.

### `bosun show <session>`

**Effect:** Print:
1. The session's branch + worktree path
2. Last 10 commits on the branch (`git log -10`)
3. Current `git status --short` output

Useful for "what has session-2 been doing?" inspection.

### `bosun merge [<session>...]`

**Arguments:** Zero or more session names (or short forms like `1`, `2`). If none, merge all bosun sessions.

**Effect:**
- For each specified session (or all if none specified):
  - Refuse if session has uncommitted changes (suggest `bosun show` to inspect)
  - Run `git merge --squash bosun/session-N` from the base branch worktree
  - If conflict: report the conflict and stop (don't commit; leave for user to resolve)
  - If clean: commit with message `merge: bosun/session-N` (or custom via `--message`)
- Print summary: which sessions merged cleanly, which had conflicts, which were skipped

**Flag:** `--no-squash` to use `--no-ff` regular merges instead of squash (default is squash).

**Safety:** Always operates on the base branch. Refuses if HEAD isn't on the base branch.

### `bosun remove <session>`

**Effect:**
- Refuse if session has uncommitted changes (`--force` to override)
- Refuse if session has commits ahead of base that haven't been merged (`--force` to override)
- Remove the worktree via `git worktree remove`
- Delete the branch via `git branch -D bosun/session-N`

### `bosun list`

**Effect:** Print one session name per line (`session-1\nsession-2\n...`). For shell scripting.

## Architecture / file layout

```
bosun/
├── cmd/bosun/
│   └── main.go                 ← entry point; Cobra wiring
├── internal/
│   ├── git/
│   │   ├── git.go              ← thin wrapper around `os/exec` for git CLI
│   │   ├── worktree.go         ← worktree-specific operations
│   │   └── git_test.go         ← unit tests with mocked exec
│   ├── session/
│   │   ├── session.go          ← Session struct + derivation logic
│   │   └── session_test.go
│   ├── status/
│   │   ├── status.go           ← status table rendering (uses text/tabwriter)
│   │   ├── status_json.go      ← JSON output
│   │   └── status_test.go
│   ├── config/
│   │   ├── config.go           ← config loader (defaults + .bosun/config.json)
│   │   └── config_test.go
│   └── tui/
│       └── tui.go              ← color + tty detection helpers
├── docs/
│   └── DESIGN.md               ← deeper design notes (link back to SPEC.md)
├── .bosun/
│   └── config.example.json
├── .gitignore
├── Makefile
├── go.mod
├── go.sum
├── README.md
├── SPEC.md                     ← this file
├── CLAUDE.md                   ← operating instructions for Claude Code sessions working on this repo
└── LICENSE
```

## Implementation notes

- **CLI library:** Use `github.com/spf13/cobra` (industry standard). One dep only.
- **No third-party libs beyond Cobra.** Stdlib does everything else.
- **All git operations via `os/exec`.** Do NOT use `go-git` or git internals plumbing — wraps too much complexity and varies across git versions. `os/exec` calling `git` is portable and version-stable.
- **Output rendering:** `text/tabwriter` for the status table. Stdlib.
- **Color:** Detect TTY via `golang.org/x/term` (single tiny dep, x/ is effectively stdlib) or roll your own check via `os.Stdout.Stat()` + `os.ModeCharDevice`. Disable color if `--no-color` flag is set or `NO_COLOR` env var is set.
- **Path handling:** ALWAYS `path/filepath`, NEVER string concatenation. This is critical for Windows.
- **Error wrapping:** Use `fmt.Errorf("%w", err)` consistently. User-facing errors get a `bosun: ` prefix.
- **Exit codes:** 0 success / 1 user error / 2 git error / 3 internal error.

## Cross-OS considerations

- **Path separators:** Use `filepath.Join`. Test on Windows in CI.
- **Line endings:** Don't make assumptions; git handles this.
- **Shell invocation:** Don't invoke shells. Always invoke `git` directly via `exec.Command("git", args...)`.
- **Executable detection:** At startup, verify `git` is on PATH via `exec.LookPath("git")`. Friendly error if not.
- **Worktree path naming:** Sibling directories with `-bosun-N` suffix. Windows-safe (no characters that need escaping).
- **CI matrix:** GitHub Actions matrix runs on `ubuntu-latest`, `macos-latest`, `windows-latest`.

## Acceptance criteria for v0.1

A v0.1 release is ready when **all** of these are true:

1. ✅ `bosun init 4` in a fresh git repo creates 4 worktrees + 4 branches as specified
2. ✅ `bosun status` prints the expected table format with correct data
3. ✅ `bosun show session-1` prints last 10 commits + git status
4. ✅ Manual edits in `myproject-bosun-1/` are visible in `bosun status` as dirty count
5. ✅ Commits in `myproject-bosun-1/` are visible in `bosun status` as ahead count
6. ✅ `bosun merge` cleanly squash-merges all sessions; conflicts are reported but don't crash
7. ✅ `bosun remove session-1` tears down the worktree + deletes the branch (with safety checks)
8. ✅ `bosun list` prints session names one per line
9. ✅ `make build` produces a single-binary executable for the host platform
10. ✅ `make cross` produces binaries for darwin/{amd64,arm64} + linux/{amd64,arm64} + windows/{amd64,arm64}
11. ✅ CI on GitHub Actions runs the test suite on `ubuntu-latest`, `macos-latest`, `windows-latest`
12. ✅ Unit test coverage ≥ 70% across `internal/`
13. ✅ At least one integration test that creates a temp git repo, runs `bosun init` + `bosun status` + `bosun merge`, asserts the result
14. ✅ README explains the use case in ≤ 30 lines
15. ✅ `bosun --help` prints clean usage text for each subcommand

## Testing approach

- **Unit tests:** For the git wrapper, use a `runner` interface that can be mocked. For the status renderer, table-driven tests against fixture session data.
- **Integration tests:** Create a `t.TempDir()`-based git repo in `testdata/`, exercise the commands end-to-end.
- **CI:** Three-OS matrix on GitHub Actions: ubuntu-latest, macos-latest, windows-latest. Each runs `go test -race ./... -count=1`.
- **Coverage gate:** `make test` should report coverage; CI checks ≥ 70% on `internal/`.

## Open questions for the implementer

The implementer should resolve these before coding (or note them in the PR):

1. **Should `init` use absolute paths or relative paths for worktrees?** Recommendation: relative (`../<name>-bosun-N`) so the user can move the parent directory and not break worktrees. Verify this works on Windows.

2. **Should `bosun list` filter by bosun-managed branches only, or include all worktrees?** Recommendation: bosun-managed only (filter branches starting with `bosun/`).

3. **Should `merge` operate on the user's current branch, or always switch to `base_branch`?** Recommendation: always operate on `base_branch`; refuse if HEAD isn't there.

4. **For dirty file count in `status`, should we count untracked files?** Recommendation: no — untracked files are usually intentional (logs, build artifacts). Use `git status --porcelain | grep -v '^??'`.

5. **For `bosun status --json`, what's the schema?** Recommendation: array of objects with the same keys as the table columns (lowercase, snake_case).

6. **Should we support `bosun init --from <branch>` to base sessions on a non-default branch?** Recommendation: yes, but defaults to `config.base_branch`.

## Future versions (NOT in v0.1 — for context)

- **v0.2:** MCP server interface. Sessions can call `bosun_announce`, `bosun_check`, `bosun_lock` tools to coordinate.
- **v0.3:** Web dashboard. Same data as `bosun status` but live-updating in a browser. Same shape as Jason's existing job-hunt dashboard.
- **v0.4:** Hooks (pre-init, post-merge), session profiles, named sessions.

## Author's note

This spec is the handoff document. A separate Claude Code session will pick this up and implement v0.1 from scratch. The implementer should treat this spec as authoritative for scope and stop when the acceptance criteria are met — *not* extend into v0.2+ work even when the codebase invites it. Scope discipline is the most important skill for getting v0.1 shipped cleanly.
