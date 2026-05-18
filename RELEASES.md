# Releases

## v0.10.1 — 2026-05-18 — dogfood-frictions round

Closes four frictions surfaced by the architect-mcp dogfood round
(see `docs/dogfood-architect-mcp.md`). All four shipped in a single
4-lane bosun-on-bosun round on `/tmp/bosun-work/` (the safe path
per issue #15's iCloud findings).

- **`bosun --version`** (#16, closed). Cobra Version field on the
  root command with build-time ldflags injection from
  `git describe --tags --always --dirty`. `make build` and the
  cross-compile targets both produce binaries that report the
  derived version. Plain `go build ./...` (no Makefile) still works
  and reports `bosun version dev` for sandbox builds. CI workflow
  also receives the injection.
- **`bosun predict` distinguishes claims from prose mentions**
  (#17, closed). Code-fenced and backtick-quoted paths are claims
  that feed the overlap calculation; plain prose mentions
  ("`Do NOT modify internal/config/**`") are informational and
  dropped from the overlap calc. Pinned by
  `TestPredict_ArchitectMCPRegression` which holds the dogfood
  regression case (17+ overlaps → ≤3).
- **#18 verify_cmd substitution** — already fixed in `7a54b1f`
  (2026-05-15). Closed as already-resolved; the dogfood doc ran
  against a stale binary that predated the fix.
- **`bosun hook install` manages .gitignore** (#12, closed). The
  install path now appends `.claude/*` + `!.claude/settings.json`
  with a bosun-named leading comment, so per-developer Claude
  Code state doesn't accumulate in untracked paths a careless
  `git add .` would sweep in. Idempotent across re-runs;
  `--no-gitignore` opts out.

Deferred to a later round: #19 (off-tree worker false-CRASHED,
needs `bosun attach` design) and #20 (predictor coverage
analysis, v1.x territory).

## v0.10.0 — 2026-05-18 — "somewhat solid from day one"

Three-phase push to close the foundational reliability + UX gaps
surfaced by the v0.9 trials and the AI-engineer review. Bosun goes
from "neat prototype" to "shippable to a stranger on macOS." All
foundational work plus the agent-experience polish plus the
first-five-minutes doorway.

**Phase 1 — macOS reliability (closes issue #15):**

Trial #3c and the v0.9 spawn bug hunt both surfaced the same
corruption shape: iCloud File Provider stripping
`.git/worktrees/<name>/{HEAD,commondir,gitdir}` files under load,
making worktrees invisible to git and breaking every multi-worktree
bosun command. Confirmed via the `/tmp/` repro experiment (clean for
9+ minutes vs. ~5-minute corruption on iCloud-managed paths).

- `internal/phantom/admin_scan.go` — new `ScanWorktreeAdmin` detects
  both divergence shapes (iCloud `" N"`/`" (N)"` suffix phantom dirs
  AND admin dirs missing top-level metadata). Read-only; powers the
  doctor check and the recovery path.
- `internal/doctor/checks_worktree_admin.go` — new
  `CheckWorktreeAdminCorruption` doctor check returns FAIL with a
  FixFn that reaps phantom + broken admin dirs (worktree dirs and
  uncommitted work are NOT touched) and runs `git worktree prune`.
- `bosun init` now refuses to run under iCloud-managed paths by
  default (`~/Documents/`, `~/Desktop/`, `~/Library/Mobile Documents/`).
  New `--force-icloud` opt-in for operators who've disabled iCloud
  sync for the dir but the heuristic doesn't know it.
- `docs/macos-setup.md` rewritten with refusal behavior, recovery
  walkthrough, and the new doctor check.

**Phase 2A — agent UX (closes issues #13 + #14):**

- `bosun_spawn` MCP tool description reframed around **context
  isolation** rather than raw parallelism. Trial #3b showed the old
  "parallelize" pitch lost against the agent's solo-tractable
  heuristic on small work; the new framing teaches the LLM to reach
  for spawn only when the parent's context window is the real
  constraint. Regression test pins the wording.
- `bosun_check_tree` — new MCP tool returns per-child state
  (`alive`/`dead`/`no-launch`/`done`) for a parent's spawn tree.
  Closes the trial #3c Bug B (parent had no signal when subs
  vanished). Same auth gate as `bosun_spawn`; only the parent's
  own live agent can query.
- `.bosun/audit/spawn.log` — new structured JSON log of every
  `bosun_spawn` invocation, success and refused. 10MB / 5-file
  rotation, flock-serialized, fail-open. Refusal gate vocabulary
  stabilized as 7 named constants. `tool_spawn.go` refactored
  through a `refuse(gate, err)` helper to keep logging DRY across
  the 11 return sites.
- `docs/v1.0-sub-task-spec.md` — design spec for the lightweight
  `bosun_subtask` MCP tool (shared worktree, no merge cycle, per-
  context isolation only). Pairs with `bosun_spawn` rather than
  replacing it. v1.0 implementation work tracked under issue #11.

**Phase 3 — first-five-minutes UX:**

- `bosun tour` — interactive 5-step walkthrough on a throwaway
  `/tmp` sandbox. Builds its own repo, runs through
  init/edit/status/predict/merge/cleanup, removes the sandbox on
  exit (or `--keep-sandbox` to preserve). `BOSUN_TOUR_AUTO=1`
  drives the whole flow non-interactively for tests / recordings.
- `bosun new-brief --pattern <name>` — scaffold generator emitting
  ready-to-fill briefs for four common patterns: `recipe` (spawn-
  warranted, from `docs/brief-recipe-template.md`), `review`
  (multi-lane code review), `audit` (multi-lane bug hunt), and
  `cleanup` (multi-lane refactor). Patterns ship embedded via
  `//go:embed`; each one parses cleanly through `brief.ParseString`
  and is end-to-end tested through `bosun init`.
- README "Try this in 5 minutes" quickstart — three-command
  doorway at the top of the README (clone, build, `bosun tour`).
  Demo asset (asciinema/GIF) follows as a separate operator-driven
  recording per the launch checklist.

**Stats:** 4 GitHub issues closed (#13, #14, plus #15's
foundational fixes — issue remains open until field-validated),
~2,800 LOC of code + tests across `internal/phantom/`,
`internal/doctor/`, `internal/mcp/`, `internal/briefscaffold/`,
`cmd/bosun/`, plus docs. Three bosun-on-bosun rounds (Phase 1 was
direct-driven for tight coupling; Phases 2A + 3 ran as 4-lane and
3-lane parallel rounds respectively, both on `/tmp/bosun-work/`
to dodge the iCloud trigger).

## v0.7 — 2026-05-17 (private; v0.8 will be the first public tag)

Polish round + bug-hunt + refactor + future-bug-hunting test suite, all landed locally. Not yet tagged or pushed publicly — `CLAUDE.md`'s release stance gates the public visibility flip on v0.8's launch checklist.

**Polish round (4 lanes):**

- `bosun launch <session>` defaults to the brief prompt when `BOSUN_BRIEF.md` is present (matches `init --launch` behavior; the standalone path no longer drops the operator at an empty prompt).
- Predictor (`bosun predict`) no longer counts paths under "Files (avoid)" headings as predicted edits. False-positive overlap counts dropped 79 → 12 on the v0.6 round-1 plan.
- Init refuses when `bosun/session-N` exists at a SHA that diverges from base (the post-cleanup-without-branch-delete case that silently used stale code). `--force` resets cleanly. Merge picked up the same 1-minute load pre-flight init has had since v0.5.
- macOS Spotlight / iCloud phantom-file filter in the state and claims dirs; `bosun rescue` and `bosun remove --force` now detect corrupted worktree gitdirs (HEAD/commondir missing) and salvage files into `.bosun/rescues/` before destruction.

**Bug-hunt waves:**

- Claims dir was ingesting `session-2 3.json` phantoms as additional CLAIMED counts on real sessions; filter applied, stale claims cleared on init.
- `bosun init`'s `unlockWorktree` was bypassing `Client.run` timeout enforcement; migrated to a proper `git.Client.UnlockWorktree` method.
- `bosun suggest` was calling `git ls-files` / `rev-parse` / `log` without timeout context; bounded each at 30s.
- `proc.Running()` couldn't detect Claude Code on macOS because the binary lives at `~/.local/share/claude/versions/<X.Y.Z>/` and `p.Name()` returned the version directory's basename rather than "claude". Now also checks the cmdline's first token, recovering the recognizable name. `BOSUN_PROC_DEBUG=1` surfaces candidate-skip reasons.
- `bosun init --force` was silent during the orphan-worktree-dir cleanup (observed ~6 minutes recursing through a Go module cache); now reports progress per-label.
- MCP server's `Serve` ctx-watcher goroutine leaked when Accept returned an error not caused by Stop; closed with a done channel.
- `bosun rescue` salvage's `copyWorktreeBestEffort` silently `return nil`'d on every per-file error, lying about what made it into the snapshot. Now tracks skipped paths with reasons and logs them to stderr.
- `executeCleanupOne` and `mergeOne` (4 sites) were `_ = rc.{state,claims}.Clear(name)` — silent failures left stale `.bosun/` metadata. Extracted `clearSessionMetadata` helper; warnings go to stderr.

**Refactors:**

- `internal/phantom` — extracted the Spotlight/iCloud duplicate-detection regex from `internal/state` + `internal/claims` (was on its way to a third copy). One `IsLikelyPhantom(name, exts...)` helper.
- `internal/lockfile` — consolidated four POSIX flock helpers (`withInitLock` / `withStateLock` / `withStoreLock` / `withMcpSpawnLock`) into one `WithLock` / `WithLockResult[T]` package. Behavior preserved; Go generics handle the mcp-autostart's payload-returning case.

**Future-bug-hunting test suite:**

- 5 fuzz targets behind `make fuzz` (default 30s each, configurable via `FUZZTIME`): brief parser, predictor extractor, label validator, phantom detector, uptime-load parser. Found a real bug on first run — `parseUptimeLoad` accepted negative load values; fixed.
- 1 new stress test (`claims.TestStress_MixedOpsAcrossManySessions` — 32 sessions × 50 ops) + broadened `make stress` target pattern.
- Failing fuzz corpora persist under `testdata/fuzz/<target>/` so a regression caught once gets replayed every subsequent run.

See `docs/v0.7-roadmap.md` for the strategic shape. The four lane briefs landed via `v0.7-round1-plan.md` (gitignored, see `STATUS.md` from the round if needed).

---

## v0.6.2 — 2026-05-17 (private)

Single-session round closing the timeout coverage gap that v0.6.1's dogfood loop surfaced: `bosun merge` hung 11 minutes on `git fsck` under load because three call sites (`fsck`, `rev-parse HEAD`, `reset --hard`) bypassed `Client.run`'s configured timeout by shelling out via `exec.CommandContext` directly. Centralized all git invocations through `Client.run`; added a table-driven test that exercises every public Client method against a fake-hung-git shim to prevent backsliding.

## v0.6.1 — 2026-05-17 (private)

Trial blockers from v0.6's external-repo trial:
- **session-1** (`internal/git/`) — `git worktree add` ignored the configured per-op timeout under fsync pressure; wrapped through Client.run with a worktree-specific 120s floor and a clear `bosun init --resume` recovery hint on timeout.
- **session-2** (`cmd/bosun/cmd_init.go` + `internal/init/state.go`) — `bosun init --resume` refused with arg-count mismatch on the no-arg invocation and refused on the locked-in-progress-worktree case the breadcrumb existed for. Resume now derives count + brief + label set from `init.state`, unlocks the in-progress worktree, and reconciles to the next step.

## v0.6 — 2026-05-16 (private)

**Resilience anchor.** Closes the gap between "main is safe via git-worktree isolation" and "bosun won't make things worse when something already went sideways."

- Agent-liveness gate on destructive ops (`bosun merge` / `remove` / `cleanup` refuse when a live agent has uncommitted work; `--ignore-running` opts in).
- Pre-merge `git fsck` on the source worktree — catches torn-write corruption before the squash.
- Reflog-based `bosun merge --undo` (resets main to a pre-merge SHA only when main hasn't advanced past it).
- CRASHED state (`proc.Running() == false && worktree dirty`) + `bosun rescue <session>` to snapshot the dirty worktree under `.bosun/rescues/`.
- MCP `bosun_heartbeat` tool + STALE derived state for sessions that haven't pinged in 5 minutes.
- Hook timeout enforcement (`hooks.Run` now respects `TimeoutSeconds`; 30s default when zero).
- `bosun init --resume` for partial-init recovery (per-step state in `.bosun/init.state`).
- README "Safety contract" section spelling out exactly what bosun does without being asked.

See `docs/v0.6-roadmap.md` for the eight asks and lane breakdown.

## v0.5 — 2026-05-15 (private)

- `bosun suggest "<goal>"` — Claude-API-backed brief authoring. Inspects the repo (RepoIntel), proposes N disjoint lanes, validates, renders to plan markdown.
- Remaining hook call-sites wired: `pre-merge`, `post-merge`, `pre-cleanup`, `post-cleanup`, `pre-remove`.
- Kickoff robustness: per-op git timeout (30s default), init progress reporting, 1-minute load-average pre-flight on `bosun init`.
- `bosun predict <plan.md>` — heuristic conflict prediction across a brief's lanes.
- `bosun config` round-out: `unset`, `validate`, `init`.

See `docs/v0.5-roadmap.md` and `docs/v0.5-suggest-spec.md`.

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
