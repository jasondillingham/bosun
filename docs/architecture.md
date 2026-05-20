# Bosun architecture

How bosun thinks. Synthesizes the operating model, core abstractions, state machine, and trust boundary into one read. If you want one document to understand what bosun is and how it stays correct, this is it.

Specifications for individual subsystems (the MCP protocol, the UID-per-worktree naming scheme, the v0.9 spawn-tree design, etc.) live in their own files alongside this one and are linked inline.

## Operating model

Bosun runs a fleet of coding-agent sessions in parallel, each on its own git worktree, and gives the operator one place to see and coordinate them. The agents are off-the-shelf (Claude Code, Aider, Codex CLI, anything that exec's a binary in a worktree) and bosun stays out of their way; bosun's contribution is the *fleet-level* coordination: who's editing what, who's done, what merges cleanly, what's stuck.

The whole system is plain text on disk under `.bosun/` plus a small JSON-RPC server (the MCP server) that agents call into when they want to coordinate explicitly. Both surfaces read and write the same files. There is no in-memory state that outlives a single command invocation.

## Core abstractions

**Session.** One unit of parallel work. Each session owns:

- A **branch** (`bosun/session-N` for numbered sessions, `bosun/<label>` for named ones)
- A **worktree** (`<repo>-bosun-<round-timestamp>-<PID>-<N>` sibling to the main worktree; scheme C from [`uid-worktree-design.md`](./uid-worktree-design.md))
- A **brief** (`BOSUN_BRIEF.md` inside the worktree — the assignment the agent reads)
- **State files** under `.bosun/state/<label>.<suffix>` for done/stuck/heartbeat/attached-pid/agent-command/docker-host/usage

A session is the unit of "agent A is doing thing X." The agent has no idea bosun exists; bosun watches the branch, worktree, and state files to derive what the agent is up to.

**Round.** One invocation of `bosun init` creates N sessions that all share a round-timestamp. The round is the operator's mental unit ("the auth refactor round"); bosun's only explicit handling of it is the shared timestamp baked into worktree paths and the optional resume breadcrumb (`.bosun/init.state`) for partial-init recovery.

**Claim.** A session's advisory declaration of which paths it's editing. Lives at `.bosun/claims/<label>.json`. Other sessions check claims via the `bosun_check` MCP tool (or `bosun claim --check` CLI) before editing. Pre-merge, the predictor runs over the in-memory plan to estimate claims before any agent has actually run.

**State markers.** Per-session signal files: `.done`, `.stuck`, `.heartbeat`, `.attached-pid`, `.usage`. Each is a small text/JSON file. The set is intentionally small so the on-disk representation is grep-able.

## The state machine

```
                              ┌──────────────┐
       bosun init ──────────► │   WORKING    │
                              └──┬───────┬───┘
              agent crash         │       │   agent calls
              (dirty + no proc)   │       │   bosun_done / bosun_stuck
                                  ▼       ▼
                            ┌──────────┐ ┌─────────┐ ┌───────┐
                            │ CRASHED  │ │  DONE   │ │ STUCK │
                            └─────┬────┘ └────┬────┘ └───┬───┘
                                  │           │          │
                       bosun rescue           │   bosun stuck --done
                                  │           ▼          ▼
                                  └──► bosun merge ─► (squashed onto base)
                                              │
                                              ▼
                                      bosun cleanup
                                              │
                                              ▼
                                          (gone)
```

`WORKING`, `DONE`, `STUCK` are explicit (state files exist on disk). `CRASHED` is *derived* by `session.Derive` from the absence of a live agent process plus a dirty worktree — bosun never writes a `.crashed` marker. The `STALE` flag (set when a heartbeat is older than 5 minutes) is also derived; it co-exists with the State enum rather than replacing a value.

## The claim graph

Two sessions editing the same file is the classic merge-conflict shape bosun is designed to avoid. The claim graph is bosun's first line of defense:

1. When a session is about to edit a file, the agent calls `bosun_claim session=session-1 paths=[internal/auth.go]`. Bosun records the claim.
2. Any other session about to edit can call `bosun_check paths=[internal/auth.go]` and learn that `session-1` is already on it. The agent then either chooses a different file or coordinates explicitly.
3. At merge time, `bosun predict` runs over the brief and flags expected overlaps before any work happens.

The claim graph is *advisory*. Bosun doesn't block writes; it surfaces collisions. The trust model assumes agents act in good faith — and that the operator has visibility into the choices each agent makes.

## The spawn tree

A session can spawn sub-sessions via the `bosun_spawn` MCP tool. The parent-child relationships live in `.bosun/spawn-tree.json`. Depth is capped (default 1, hard ceiling 4) so a runaway agent can't fork-bomb; per-parent concurrent-spawn quotas (default 3) limit blast radius.

Spawn-tree validation runs at every `Load`: self-references, orphaned children, and parent cycles are refused. See [`v0.9-spawn-spec.md`](./v0.9-spawn-spec.md) for the auth + quota model and [`v1.0-sub-task-spec.md`](./v1.0-sub-task-spec.md) for the lighter-weight sub-task registry.

## The MCP protocol

Bosun's MCP server is the agent-facing interface. Documented in full in [`mcp-protocol.md`](./mcp-protocol.md); the short version:

- One Unix socket per repo at `.bosun/mcp.sock` (or a fallback in `os.TempDir()` for path-length-constrained cases).
- Owner-only permissions (`0o600`) so non-operator local users can't connect.
- 14+ tools as of v0.11: `bosun_check`, `bosun_claim`, `bosun_release`, `bosun_done`, `bosun_stuck`, `bosun_attach`, `bosun_heartbeat`, `bosun_usage`, `bosun_announce`, `bosun_spawn`, `bosun_subtask`, `bosun_subtask_cancel`, `bosun_check_tree`, `bosun_predict`, plus operator-defined custom tools (Phase 5).
- Tools write the same `.bosun/` files the CLI writes. Mixed-mode operation (some sessions on MCP, some on CLI) is the supported case, not a degraded fallback.

The server runs as a single `*mcp.Server` shared across connections. State writes serialize through `internal/lockfile` (POSIX `flock`) so concurrent agents can't tear shared files.

## The liveness gate

`bosun status` derives one of "RUNNING / DONE / STUCK / CRASHED" per session. The detection ladder lives in `session.Derive`:

1. **Attached PID** wins first. If `.bosun/state/<label>.attached-pid` exists and the PID is alive, the session is `RUNNING <pid>`. If the PID is dead, the session flips to `CRASHED` (this is the v0.11 "recoverable crash" — the agent registered, then disappeared).
2. **Proc-scan** is the fallback. Bosun scans for a process whose binary basename matches the configured agent command (`claude`, `claude-code`, `code-cli`, or whatever the operator set) and whose CWD is inside the worktree.
3. **Heartbeat-implies-running** (Phase 5 #63). When the agent runs inside a Docker container, neither the attached-PID path nor the proc-scan can see across the PID namespace boundary. If a fresh `bosun_heartbeat` (newer than 5 min) exists, bosun treats the session as `RUNNING heartbeat`. Distinct from a real PID; rendered as the literal `heartbeat` in the RUNNING column.
4. **External mode** is the override. When `config.liveness_gate = "external"`, the entire detection ladder is skipped; the column renders `external` and CRASHED auto-transitions are suppressed. Operators driving agents from outside bosun's view (CI runners, the Claude Code `Task` sub-agent flow) use this mode.

## Trust model

Bosun trusts:

- **The operator** with full shell access. Anything in `.bosun/config.json` runs as the operator anyway; hooks, custom MCP tools, `agent_command` overrides all execute with operator privileges.
- **Agents** to act on their own session honestly. Sessions can self-assert their label in MCP calls (no caller-credential verification). With the socket locked to `0o600`, the threat surface is bounded by the operator's own trust boundary — all agents are equally privileged inside the operator's account.

Bosun does NOT trust:

- **Other local users on the host.** The MCP socket is `0o600` for this reason. State files default to operator-only read.
- **External endpoints.** Webhook bodies are curated (allowlist of safe env keys, agent-controlled free text excluded). Anthropic API responses are size-capped. TLS verification is never disabled.

The security audit at [`security-audit-2026-05.md`](./security-audit-2026-05.md) traces these boundaries in detail.

## Failure modes

What happens when things go wrong, and how the code structure responds:

**Agent crashes mid-work.** `attached-pid` exists but PID is dead → state derives CRASHED → operator runs `bosun rescue <label>` to recover any uncommitted work into a salvage dir, or `bosun remove --force` to wipe.

**Init crashes mid-flight.** `.bosun/init.state` carries the resume breadcrumb. `bosun init --resume` picks up where the prior run died. Orphan worktree directories from a crashed `git worktree add` get auto-cleaned at resume time if they contain only crashed-AddWorktree artifacts (the empty-dir or `.git`-only shape); operator hand-fixes survive the safety check.

**Merge conflict.** `bosun merge` runs squash-merges; on conflict it stops, leaves the operator in the conflicted state, and reports which session conflicted. Manual `git` resolution + commit; bosun's `bosun merge --undo` rolls back recent merges via pre/post-SHA anchors recorded in `.bosun/merges.log`.

**Concurrent processes.** Cross-process serialization via `internal/lockfile.WithLock` everywhere shared files are written: claims, state markers, audit logs, events log, spawn-tree. POSIX `flock` on a `.lock` file co-located with the data. Windows lock primitives are TODO stubs as of v0.11 — operators on Windows accept the documented "concurrent `bosun done` may race" caveat until `LockFileEx` lands.

**iCloud / Spotlight corruption (issue #15).** macOS File Provider can strip git worktree admin metadata under load. `bosun doctor` detects iCloud-managed paths and refuses init unless `--force-icloud` is passed. `bosun rescue` and `bosun remove --force` salvage from the corruption shape after the fact.

## Design principles

A few invariants the codebase tries to hold:

**Small focused files.** `internal/git/`, `internal/claims/`, `internal/state/`, `internal/session/`, `internal/launcher/`, `internal/webhooks/` etc. are independently testable. The CLI in `cmd/bosun/` wires them together; the internal packages have no cross-deps that would force a refactor cascade.

**No global state.** No package-level mutable state outside the documented tool-registration init() hooks in `internal/mcp/`. No `init()` side effects beyond Cobra command registration.

**Plain text on disk.** Every file in `.bosun/` is grep-able. State markers are sub-PIPE_BUF JSON lines or simple text, atomic-appendable. Audit logs and the events log rotate at 10 MiB so they don't grow unbounded. The whole on-disk representation is recoverable by hand if the binary breaks.

**Agent-agnostic.** Bosun has zero hard dependencies on Claude Code or any other agent. The MCP protocol is the only Claude-Code-shaped piece and it's optional — agents that don't connect to MCP still work via the CLI. Operators wire alternative agents via `agent_command` wrappers ([`examples/agent-wrappers/`](../examples/agent-wrappers/)).

**Git as ground truth.** Branch existence, ahead-of-base count, dirty-tree count, recent commits — all derived live from `git`. Bosun's state files supplement the git view; they never replace it. If `.bosun/` is wiped, the worktrees and branches still work and bosun re-derives a sensible view on the next status.

## Reading order

If you're new to the codebase and want the right entry points:

1. [`SPEC.md`](../SPEC.md) — the v0.1 specification. Some details are stale but the framing is intact.
2. This document (`architecture.md`) — the synthesis.
3. [`mcp-protocol.md`](./mcp-protocol.md) — agent-facing contract.
4. [`uid-worktree-design.md`](./uid-worktree-design.md) — naming + path layout.
5. [`security-audit-2026-05.md`](./security-audit-2026-05.md) — threat model and findings.
6. [`bug-hunt-2026-05.md`](./bug-hunt-2026-05.md) + [`bug-hunt-2026-05-pass2.md`](./bug-hunt-2026-05-pass2.md) — recent correctness audits.

For specific subsystems, the per-feature docs (`v0.9-spawn-spec.md`, `v1.0-sub-task-spec.md`, `agent-command-design.md`, `sandbox-launcher-design.md`, `remote-docker-plan.md`) each cover one slice end-to-end.
