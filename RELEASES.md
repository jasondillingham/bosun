# Releases

## v0.4.0-rc1 — 2026-05-16

Lifecycle hooks land as a real extension point, the operator gets the integration-rehearsal flags they kept asking for (`merge --dry-run`, `list/show --json`), the web dashboard grows the brief preview + announcement feed, and the round-2 follow-ups close the loop on orphan-dir recovery, in-place config editing, and the bug-hunt findings.

Tagged `-rc1` because the hooks surface only covers init/done so far — pre-merge / post-merge / pre-cleanup / post-cleanup / pre-remove call-sites are queued for v0.5 (see [`docs/v0.5-roadmap.md`](./docs/v0.5-roadmap.md)). Everything else is production-ready.

### What's in

**Round 1 — feature lane:**

- **Lifecycle hooks scaffolding.** `internal/hooks` runs operator-defined shell commands at lifecycle moments. v0.4 ships three event hooks (`pre-init`, `post-init`, `post-done`) wired through `.bosun/config.json`. Each hook supports `fail_open` (warn-don't-block on non-zero exit) and `timeout_seconds`. The runner intentionally goes through `sh -c` so operators can use pipes / `&&` chains without bosun re-parsing the command. Unknown event names fail validation at config load so typos surface immediately. Remaining call-sites (`pre-merge` / `post-merge` / `pre-cleanup` / `post-cleanup` / `pre-remove`) are scaffolded but unwired pending v0.5.
- **`bosun merge --dry-run`.** Print exactly what `bosun merge` *would* do — which sessions would squash-merge, which would skip and why, in dependency order — without touching the work tree. Dependent-session simulation pretends already-dry-run-merged sessions are "merged" so a `(depends: session-1)` chain reports cleanly. Pairs naturally with the new `--json` flags for scripted preflight.
- **`bosun list --json` and `bosun show --json`.** Stable machine-readable wire shape (`version: "v1"`) for both commands. `bosun list --json` emits the session table; `bosun show <session> --json` emits the per-session detail (branch, state, claims, recent commits, brief metadata). Built so a future external dashboard or CI gate can consume bosun state without parsing the human-readable tables.
- **Web brief preview + events feed.** `bosun serve` (the v0.3 HTTP dashboard) now ships `/api/brief/<session>` for the rendered BOSUN_BRIEF.md and surfaces `bosun_announce` events on the live SSE stream so operators see TUI-equivalent activity in the browser tab.

**Round 2 — robustness lane:**

- **Orphan-directory recovery.** `bosun cleanup --orphans` (shipped in v0.2) now also detects worktrees that exist on disk but have no corresponding git registration — a state the v0.4 kickoff hit when `git worktree add` hung mid-flight under filesystem pressure (see `docs/v0.4-findings.md`). The cleanup path reconciles both directions: stale git registrations *and* orphan directories.
- **`bosun config` command.** Inspect (`bosun config show`) and edit (`bosun config set <key>=<value>`) `.bosun/config.json` without hand-editing JSON. Validates keys against the known schema (rejecting hook-event typos and the like) so config drift is caught at write time, not at the next bosun command. Operators previously had to `cat`/`jq`/`vim` the config file by hand — this just makes the right thing easy.
- **Bug-hunt audit fixes.** Round-2 audited `internal/claims/` / `internal/state/` for the kinds of bugs the v0.3.1 flock fix surfaced — concurrent-writer races, partial-write recovery, lock-file leak on crash. The audit lane closed several latent issues without changing the public API. See commit log under sessions 3/4 of the round.

### Compatibility

- Backwards-compatible. `.bosun/config.json` files written by v0.2 / v0.3 load unchanged; the new `hooks` field is optional and defaults to "no hooks".
- New flags are additive. Existing scripts that parse `bosun list` / `bosun show` text output keep working; only opt into `--json` if you want the stable contract.
- Hooks run through `sh -c` — on Windows agents that need this, install Git-for-Windows / WSL `sh` first.

### What's NOT in (deferred to v0.5)

- Pre-merge / post-merge / pre-cleanup / post-cleanup / pre-remove hook call-sites. Scaffolding is in; the call-sites are the v0.5 ask.
- Predictive conflict analysis ("session-1 will touch X before it actually edits") — needs static analysis or LSP integration; not in any v0.4 round.
- Ghostty tab support for older Ghostty versions — still pending upstream `+new-tab` argv support (we dropped our own attempt in v0.2; see `docs/v0.5-roadmap.md`).
- The silent-init-hang fix from `docs/v0.4-findings.md` (timeouts on `git worktree add`, progress reporting, pre-flight load check). Documented for v0.5.

See [`docs/v0.5-roadmap.md`](./docs/v0.5-roadmap.md) for the next round's planned scope.

---

## v0.3.0 — 2026-05-16

The operator dashboard round. v0.2 finished the agent-facing surface (MCP tool calls for coordination); v0.3 builds the *operator*-facing surface — a long-running TUI control center, a browser dashboard, custom session names, and "is the agent actually running?" detection. Plus the v0.3.1 follow-up that hardened claims against cross-process races.

### What's in

- **Bubbletea TUI control center (`bosun tui`).** A persistent terminal UI that replaces "open six terminals to run `bosun status` over and over." Auto-refreshes every 2s, with keybinds for the common operator actions: `j`/`k` to move, `m` to merge the selected session, `M` to merge every DONE session, `c` to cleanup, `r` to remove (confirms first), `l` to launch a session window, `s` to toggle an inline brief preview, `R` to refresh, `q` / `Ctrl-C` to quit. Action handlers are dependency-injected so the same Model is driven by tests with fakes — no terminal required for the test suite.
- **Web dashboard (`bosun serve`).** Long-lived HTTP server exposing `/api/status` (JSON snapshot) and `/api/events` (server-sent events). A minimal embedded HTML page at `/` consumes both for a browser-tab view of the fleet. Defaults to binding `127.0.0.1`; there is no authentication, so binding to a non-loopback address opens the dashboard to that network at your own risk.
- **Custom session labels.** `bosun init auth http storage` works alongside `bosun init 4`. Labels participate in every command — `bosun status` shows them, `bosun claim auth ...` works, plan-markdown briefs can target `## auth` / `## http`. Reserved-word and shell-metacharacter validation rejects unsafe labels at parse time. Numeric `session-N` sessions are now just a special case of labels for back-compat.
- **Agent process detection.** New `internal/proc` package detects whether a Claude Code agent process is actually running in each worktree (matches on both the process basename — `claude` / `claude-code` / `code-cli` — and the working directory, so an unrelated process whose CWD happens to coincide doesn't false-positive). Surfaces as a `RUNNING` indicator in `bosun status` and the TUI. Backed by gopsutil; per-process permission errors are swallowed silently.
- **v0.3.1 — cross-process claims flock.** Follow-up patch (commit `333ff36`) introduced `flock(2)` on the claims file so concurrent CLI and MCP claim writes can't race each other into dropped updates. The boundary-test suite in the v0.3.1 bug-hunt round caught the race directly — two writers both reading-then-writing the claims file would silently lose whichever update finished second. The fix also added the cross-process boundary tests that exposed the issue.
- **v0.3.1 — stale-socket discovery hardening.** The launcher now checks that an inherited `BOSUN_MCP_SOCK` actually points at the current repo's socket before honoring it, fixing a wedge where an operator's stale env var from a previous repo prevented sessions from finding the local MCP server. See `docs/v0.4-findings.md` (Bug 4) for the original reproducer.
- **v0.3.1 — read-only GOMODCACHE removal helper.** `internal/git.chmodWritableTree` chmod-walks a worktree before `git worktree remove` so read-only Go module cache files don't EACCES the removal. Partial fix; the realistic failing tree from `bosun-bosun-1` cleanup is still parked for capture (see `docs/v0.4-findings.md`, Bug 3).

### Compatibility

- New commands (`bosun tui`, `bosun serve`) are additive; the v0.2 CLI shape is unchanged.
- Custom labels are backwards-compatible with `session-N`: existing scripts and plan files keep working.
- `bosun status` gains a `RUNNING` column; scripts that parse status text and expect the v0.2 column set should switch to `bosun list --json` (added in v0.4) or pin their parsing to columns they care about.
- New runtime dependencies: `github.com/charmbracelet/bubbletea` (TUI), `github.com/shirou/gopsutil/v3` (process detection). Still within the "small dependency surface" target.

### What's NOT in (planned for v0.4 or v0.5)

- Lifecycle hooks — landed in v0.4.
- `merge --dry-run` and `list/show --json` — landed in v0.4.
- Predictive conflict analysis — needs static analysis; deferred.

---

## v0.2.0 — 2026-05-16

Promotes the `-alpha` round-0 protocol foundation to a full MCP-tool surface for session coordination, plus a focused round of operator-visible polish. Sessions can now `bosun_claim` / `bosun_release` / `bosun_done` / `bosun_stuck` / `bosun_announce` / `bosun_check` directly through tool calls instead of shelling out to the CLI, while the same filesystem state stays canonical so non-MCP sessions keep working.

### What's in

**MCP server interface (round 1):**

- **Full tool surface.** `bosun_claim(paths)` / `bosun_release(paths)` / `bosun_done(message?)` / `bosun_stuck(message)` / `bosun_check(paths)` / `bosun_announce(event)` — all wired to the same `.bosun/claims/` / `.bosun/state/` the CLI reads and writes. Tools self-register at package init() so adding a tool no longer means editing a central registry.
- **Session-identity handshake.** Each MCP connection identifies which session it represents at connect-time, validated against the live session list. `bosun_claim` / `bosun_done` / `bosun_stuck` therefore don't need an explicit session argument from the agent — the server already knows.
- **Auto-export of `BOSUN_MCP_SOCK`.** `bosun init --launch` now drops the resolved socket path into each spawned session's environment, so MCP-capable agents discover the server with zero config.
- **MCP autostart.** `bosun status` / `bosun init` / etc. start the MCP server lazily if one isn't already running, fronted by a per-repo lock so concurrent invocations don't fight to bind the socket. The autostart path is platform-aware (`mcp_autostart_unix.go` / `mcp_autostart_windows.go`).
- **`bosun_announce` event feed.** A JSONL append-only log at `.bosun/events.jsonl` captures every announcement; `bosun status` tails the last 5 inline.

**v0.2 polish (post-MCP):**

- **`bosun status --summary-only`.** Just the one-line header (state counts, total ahead, overlap count), no table. For scripting and small terminals. Mutually exclusive with `--json` / `--watch`.
- **`bosun launch <session>` standalone command.** Spawn a launcher window for an existing session without going through `init`. Useful when a window got closed accidentally, you want to retry with a different command (`--command`), or you're testing the launcher itself. Includes `--open-as-tab` for tab-aware terminals.
- **`bosun cleanup --orphans`.** Tear down sessions beyond the configured fleet size — when `bosun init --force` goes from N=6 to N=3, sessions 4..6 stop showing up in plans but their worktrees / branches linger. `--orphans=N` cleans everything past the cap (default: the live `default_session_count`).
- **Dependency-aware plan briefs.** Plan markdown can declare `## session-2 (depends: session-1)` and `bosun merge` respects ordering — dependent sessions skip until their predecessors are merged. The same metadata threads through `bosun merge --dry-run` so dependency chains preview correctly.
- **Tab support for non-Ghostty terminals.** The launcher knows how to open a new tab in iTerm2 (AppleScript), Terminal.app (AppleScript), gnome-terminal (`--tab`), and Windows Terminal (`wt new-tab`). Auto-detected; falls back to a new window if the terminal isn't recognized.
- **Configurable verify command.** `.bosun/config.json` gains `verify_cmd` (default `make check`). The brief preamble auto-injected into BOSUN_BRIEF.md uses this value, so projects that run `go test ./...` or `npm test` get a brief that matches their workflow.

### Compatibility

- Backwards-compatible. Filesystem-based coordination from v0.1 / v0.2-alpha still works unchanged. Sessions that don't connect to MCP keep operating off `.bosun/claims/` / `.bosun/state/`; mixed-mode is the intended behavior, not a fallback.
- New tools and the autostart machinery are opt-in via `BOSUN_MCP_SOCK`. CLI-only workflows are unaffected.
- `bosun init --launch` now exports `BOSUN_MCP_SOCK`; sessions that previously read a *stale* `BOSUN_MCP_SOCK` from the operator's shell could wedge — see the v0.3.1 fix above for the discovery-hardening follow-up.

### What's NOT in (planned for v0.3)

- Bubbletea TUI control center — landed in v0.3.
- Web dashboard (`bosun serve`) — landed in v0.3.
- Custom session names — landed in v0.3.
- Agent process detection — landed in v0.3.
- Lifecycle hooks beyond MCP coordination — landed in v0.4.

---

## v0.2.0-alpha — 2026-05-15

Round-0 foundation for the v0.2 MCP server work. Establishes the protocol layer, one stub tool, and the discovery contract that round-1 parallel sessions will build on. **Not production-ready** — explicitly tagged `-alpha` to signal that round 1 is where the user-facing surface fills in.

### What's in

- New `internal/mcp` package built on `github.com/modelcontextprotocol/go-sdk` v1.6+
- Custom Unix-socket transport so one server can fan multiple sessions onto a single shared backend (vs. the SDK's default stdio = one subprocess per session)
- One tool: `bosun_check(paths) → {conflicts: [{path, sessions}]}` — read-only, queries existing `.bosun/claims/` for overlaps
- New `bosun mcp` subcommand: foreground daemon, default socket at `<repo>/.bosun/mcp.sock`, `--socket` override
- Discovery contract: sessions read `BOSUN_MCP_SOCK` env var (auto-export in round-1's session-4)
- Helpful error when the resolved socket path exceeds the ~104-byte Unix-domain limit (catches the deep-repo-path footgun before bind fails)
- Two tests: in-process pipe-based smoke test for the protocol; end-to-end test spawning the real `bosun mcp` subprocess and dialing the socket
- Protocol notes in `docs/mcp-protocol.md` documenting the contract round-1 sessions will target

### Compatibility

- **Go 1.25+** is now required (up from 1.23 in v0.1.0). The MCP SDK requires it.
- The filesystem-based coordination from v0.1.0 still works unchanged. The MCP server reads and writes the same `.bosun/claims/` and `.bosun/state/` that `bosun status` / `cleanup` / `merge` operate on. Sessions that don't connect to MCP keep working as before; mixed-mode operation is the intended behavior, not a fallback.

### What's NOT in (planned for round 1)

- `bosun_claim`, `bosun_release` tools
- `bosun_done`, `bosun_stuck` tools
- `bosun_announce` (operator-visible events)
- `bosun init --launch` auto-exporting `BOSUN_MCP_SOCK` to each spawned session
- Session-identity handshake (today's tools are stateless across the connection)

See [`docs/mcp-protocol.md`](./docs/mcp-protocol.md) for the round-1 plan.

---

## v0.1.0 — 2026-05-15

First tagged release. A Go CLI for coordinating parallel coding-agent sessions on isolated git worktrees, with a workflow built around `init → claim → done → merge → cleanup`. Repo is private; tag is for internal versioning.

### Install

```
go install github.com/jasondillingham/bosun/cmd/bosun@v0.1.0
```

Or grab a pre-built binary from the GitHub release for your OS/arch (darwin/linux/windows × amd64/arm64).

### What's in

**Commands:** `init`, `status`, `show`, `claim`, `done`, `merge`, `remove`, `cleanup`, `list`

**Key behaviors:**
- **Brief fan-out.** `bosun init --brief plan.md` parses a markdown plan with `## session-N` sections and drops a per-session `BOSUN_BRIEF.md` into each worktree, each prefixed with a "How to work this session" lifecycle preamble.
- **Session launcher.** `bosun init --launch` spawns an interactive agent session in each worktree. Auto-detection order: tmux (when inside tmux) → Ghostty → OS-native terminal → print-fallback. On Ghostty the first session opens a new window and subsequent sessions open as tabs.
- **Initial prompt.** `--initial-prompt "..."` passes a kickoff message to the launched agent. Defaults to "Read BOSUN_BRIEF.md..." when paired with `--brief`.
- **Filesystem coordination.** Claims (advisory file declarations) live in `.bosun/claims/`; session state (DONE/STUCK markers) in `.bosun/state/`. Both auto-managed by the relevant commands.
- **Live status.** `bosun status` prints a one-line summary above the table (state counts, total ahead, overlap count). `--watch` re-renders on an interval; `--json` emits machine-readable output; `--with-overlaps` adds a collision report.
- **Patch-id-aware lifecycle.** `bosun remove`, `bosun cleanup`, and `bosun merge` all detect when a session's commits are patch-id-equivalent to base (after a squash-merge) and handle them as "already-merged" instead of treating them as unmerged work.
- **Isolated build caches.** `bosun init --launch --isolate-cache` points `GOCACHE` / `GOMODCACHE` / `npm_config_cache` / `PYTHONPYCACHEPREFIX` / `CARGO_TARGET_DIR` at per-worktree directories so parallel builds don't fight.

### Compatibility

- Go 1.23+ to build (only third-party deps: `github.com/spf13/cobra`, `golang.org/x/term`)
- Git on PATH (any version that supports `worktree`, `rev-parse --git-common-dir`, `cherry`)
- Runs on macOS, Linux, Windows (CI tests all three)

### What it solves

- **Branch chaos** at multi-session parallelism — every session gets its own isolated worktree on a `bosun/session-N` branch
- **Visibility blindness** across N terminals — one `bosun status` shows everything
- **Work assignment friction** — fan out N briefs from a single plan markdown
- **Conflict-at-merge-time** — `bosun claim` lets sessions declare paths up front; `bosun status --with-overlaps` surfaces collisions before merge
- **Resource contention** — `--isolate-cache` partitions build artifacts per worktree
- **Recovery cost** — every command is idempotent and the lifecycle is auditable via `bosun show`

### What shipped beyond the original draft

The original v0.1 spec listed 8 commands and a smaller surface. The v0.1.0 release added the following based on real-world dogfood feedback while building bosun itself:

- `bosun cleanup` command (originally a v0.2 deferred item)
- `bosun status --watch` mode (originally deferred to v0.2)
- One-line summary header on `bosun status`
- `--initial-prompt` flag for `bosun init --launch` (auto-kickoff prompt)
- `--stuck` flag for `bosun done`
- Workflow preamble auto-prepended to `BOSUN_BRIEF.md`
- `.claude/CLAUDE.md` auto-loader written into each worktree
- Ghostty support in the launcher, with first-window-then-tabs UX
- Patch-id detection (`git cherry`) integrated into remove / cleanup / merge
- Auto-gitignore of plan files at the repo root
- Workflow-aware error handling: `bosun merge` reports conflicts gracefully instead of crashing; launcher uses `Start()` + background reap so init doesn't hang

### What's not in v0.1.0 (deferred to v0.2)

- MCP server interface — sessions still coordinate via filesystem state, not tool calls
- Tab support for non-Ghostty terminals (Terminal.app, iTerm2, gnome-terminal, Windows Terminal)
- Custom session names beyond `session-N`
- Conflict prediction before sessions step on each other
- Bubbletea TUI control center — deferred to v0.3 so the MCP work in v0.2 lands cleanly first

See [`docs/v0.2-roadmap.md`](./docs/v0.2-roadmap.md) for the planned next step.

### Acknowledgments

Implementation surfaced and refined during a multi-round dogfood session where bosun was used to coordinate work on bosun itself. The dogfood loop caught at least 8 real bugs that the original test harness missed.
