# Releases

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
