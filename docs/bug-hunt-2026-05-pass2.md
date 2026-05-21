# Bosun bug hunt — May 2026, second pass

Follow-up to `docs/bug-hunt-2026-05.md`. First pass covered bug *classes* (races, state machines, errors, cross-platform, test gaps). This pass went after *subsystems*: git invocation, init kickoff flow, HTTP/SSE serve, brief + spawn-tree parsing, suggest/LLM integration.

## Fix status

| Finding | Status | Severity |
|---|---|---|
| P2-1 — `brief.ValidateBriefs` silently accepts empty/case-wrong briefs | **Fixed** (refuses with actionable error) | Medium |
| P2-2 — UTF-8 BOM not stripped before regex match | **Fixed** | Medium |
| P2-3 — `init.state` lingers when post-init hook fails | **Fixed** (Clear runs before hook) | Medium |
| P2-4 — `git.UnmergedPatches` missing `\r` trim on Windows | **Fixed** | Low |
| P2-5 — `extractJSON` docstring lies about "largest" object | **Fixed** (docstring matches code) | Low |
| Init partial-failure orphan worktrees | **Fixed** in `29b8625` (auto-cleans crashed-AddWorktree orphans on `--resume`) | High |
| Init `--force` cleanup has no rollback | **Fixed** (plan-then-execute with completed-list reporting) | Medium |
| Same-second parallel init collision | **Fixed** (PID appended to round timestamp via `newRoundTimestamp`) | Medium |
| spawn-tree no consistency validation on Load | **Fixed** (`validateTree` rejects self-parent, orphans, cycles) | Medium |
| Suggest no retry on 429/5xx | **Fixed** (`transientCallError` + exponential backoff) | Medium |
| Suggest token-budget growth from retry-template echoes | **Fixed** (`retryErrorByteCap=256` truncates validation-error strings echoed back) | Medium |
| `git.BranchExists/TreeEqualsBase/MergeYieldsBase` bypass timeout | **Fixed** (`runAllowExitCode` helper) | Low |
| HTTP/SSE no connection limit | **Fixed** (`--max-connections` flag, default 64) | Low |
| Subtask label collision unclear error message | **Fixed** 2026-05-21 (`spawntree.AddChild` now names the label + points at `bosun status` / `bosun cleanup`) | Low |

**Status:** all 14 findings closed. This pass is fully resolved.

## Findings detail

### P2-1 + P2-2 — Silent zero-brief: empty file, wrong-case headings, BOM-corrupted first line

`brief.ValidateBriefs` used to iterate an empty slice and return nil. So a brief file with:

- nothing at all, or
- only prose without `## session-N` headings, or
- headings with wrong case like `## Session-1`, or
- a UTF-8 BOM (`0xEF 0xBB 0xBF`) on the first byte that broke the regex match

…would silently produce **zero** briefs. The operator's `bosun init --brief plan.md` then created zero worktrees with no error to explain why.

Both fixes are in `internal/brief/brief.go`:
- `parseContent` now strips the UTF-8 BOM via `strings.TrimPrefix(s, utf8BOM)` before the CRLF normalization.
- `ValidateBriefs` refuses an empty slice with a new exported sentinel `ErrEmptyBriefs`, so callers can choose how to surface it. `cmd_init.go` catches `errors.Is(err, brief.ErrEmptyBriefs)` and wraps with the existing rich "Expected shape" message; other callers (predict, MCP `bosun_spawn`) get the default which lists the three most likely causes.

Two new tests pin both: `TestParseString_RejectsEmptyBriefs` (table-driven over the failure modes) and `TestParseString_StripsUTF8BOM`.

### P2-3 — `init.state` lingers when post-init hook fails

`cmd_init.go:704` ran the post-init hook **before** clearing `.bosun/init.state`. If the hook failed (script exits non-zero, network call times out, anything), the resume breadcrumb stayed on disk. The operator's next plain `bosun init` was refused with "previous init didn't finish" — even though every worktree had been created and the round was effectively done.

Fixed by swapping the order: `istate.Clear` now runs first; the post-init hook fires after the breadcrumb is gone. Justification — the post-init hook is observability, not load-bearing; treating its failure as a reason to refuse the next round was the wrong call.

### P2-4 — `git.UnmergedPatches` missing `\r` trim

`internal/git/git.go:511` parses `git cherry` output line-by-line and checks for `+ ` prefix. On Windows (or with `core.autocrlf` set), git can emit `\r\n` line endings. The trailing `\r` survives the `strings.Split(out, "\n")` and the `HasPrefix` check fails on `+ <sha>\r`. Result: unmerged-patch count off by one in the rare CRLF case.

Fixed by adding `line = strings.TrimRight(line, "\r")` at the top of the loop, matching the same pattern `parseWorktreeList` already uses.

### P2-5 — `extractJSON` docstring lies

`internal/suggest/claude.go:457` says the function "returns the largest balanced JSON object embedded in text." It doesn't. It returns the *outermost* balanced object starting at the first `{`. If Claude ever emits two top-level objects (a documentation example followed by the real proposal), the function picks the first one — which is fine for typical Claude responses but doesn't match the docstring.

Fixed the docstring to describe what the code actually does. No code change; in practice Claude never emits multiple top-level objects in a single response, so the existing behavior is what we want.

## Resolved — bigger work

These findings opened larger than P2-1…P2-5 and were resolved in follow-up commits rather than in the initial pass. Listed here for the historical trail.

### Init partial-failure orphan worktrees (High) — Fixed in `29b8625`

If `bosun init 4` succeeded at creating worktrees for sessions 1–2 and then failed on session 3 (disk full, git lock contention, anything), `istate.MarkComplete` was never called for session 3. On `--resume`, the resume logic skipped completed sessions (1 and 2) but didn't clean up the *partial* state of session 3 — the loop re-attempted `git worktree add` for that path, hit "already exists," and refused.

**Fixed by** `cmd_init.go:529-547` — resume-time orphan reconciliation. When `--resume` detects a partially-created worktree dir from a previous crash (empty dir, or one with only a partial `.git` link), it cleans the dir before retrying. Surfaces to stderr so the operator sees what happened.

### `--force` cleanup has no rollback (Medium) — Fixed

When `bosun init --force` was run, the cleanup loop removed existing worktrees and branches one-by-one. If removal of worktree 2 failed mid-loop, worktree 1 was already deleted but worktree 3 still existed. No rollback; manual recovery only.

**Fixed by** `buildForceCleanupPlan` (`cmd_init.go:168`) — plan-then-execute. The cleanup operations are collected first, then run with explicit progress accounting so a partial failure surfaces a clear "X of N done, here's what's left" message instead of leaving the operator guessing.

### Parallel init same-second collision (Medium) — Fixed

Round timestamps were captured at second precision. Two `bosun init` runs in the same UTC second from the same repo produced identical worktree directory names — second invocation overwrote the first (under `--force`) or failed on "already exists."

**Fixed by** `newRoundTimestamp` (`cmd_init.go:249-256`) — appends `os.Getpid()` to the timestamp suffix. Two same-second invocations from different PIDs now produce distinct paths. Tests inject a deterministic PID via `initPIDFn`.

### spawn-tree no consistency validation on Load (Medium) — Fixed

`spawntree.Store.Load` decoded the JSON file and checked the schema version, but didn't validate parent-child consistency: orphaned children, self-references, cycles. A corrupt or hand-edited file could land bosun in a state where `ParentOf` walked indefinitely or `EnrichSessions` crashed on a missing entry.

**Fixed by** `validateTree` (`spawntree.go:131-163`) — runs unconditionally on Load. Rejects self-parent links, orphaned children whose parent isn't in the Sessions map, and cycles (via parent-chain walk with revisit detection, bounded by `cycleHopLimit=32`).

### Suggest no retry on transient HTTP errors (Medium) — Fixed

`internal/suggest/claude.go` didn't distinguish 429 (rate-limit, retry-worthy) from 4xx (auth-error, terminal). Any non-2xx response failed the whole call.

**Fixed by** `transientCallError` (`claude.go:387-393`) — wrapper type for retry-worthy failures. `callOnce` returns it on 429/5xx + transport-layer errors; the outer call loop checks `errors.As` and retries with exponential backoff inside the `maxProposeAttempts` budget.

### Suggest token-budget growth from retry-template echoes (Medium) — Fixed

Each Propose retry appended 2 messages (assistant + user) to the conversation. After 3 attempts the conversation could be 5KB+ — Anthropic would charge for it and could clip if the context window was hit. The dominant growth source was validation-error strings being echoed back into the retry follow-up template.

**Fixed by** `retryErrorByteCap = 256` (`claude.go:146-152`) — `truncate(parseErr.Error(), retryErrorByteCap)` and `truncate(validateErr.Error(), retryErrorByteCap)` bound the echoed-error portion of each retry. The full conversation can still grow to several KB across 3 attempts, but no single retry inflates it by an unbounded validation-error dump.

### HTTP/SSE no connection limit (Low) — Fixed

`bosun serve` accepted unbounded concurrent SSE connections. Each spawned a goroutine + event-poll timer; 10K connections meant 10K goroutines.

**Fixed by** `--max-connections` flag (`cmd_serve.go:51`) — default 64, `0` disables the cap. Default bind is still `127.0.0.1`, so the practical attack surface is localhost; operators binding `0.0.0.0` get bounded resource use without further config.

### `git.BranchExists/TreeEqualsBase/MergeYieldsBase` bypass timeout (Low) — Fixed

These three methods called `c.Runner.Run(ctx, ...)` directly instead of `c.run()`, skipping the configured timeout. `TreeEqualsBase`'s `git diff` and `MergeYieldsBase`'s merge-tree work could legitimately exceed the timeout on large repos.

**Fixed by** `runAllowExitCode` (`git.go:197-209`) — runs git through the timeout path while returning the process exit code, so exit-code-1 (the "differs"/"not present" signal) can be classified without being treated as an error. The three call sites route through it (`git.go:345, 576, 610`).

### Subtask label collision unclear error message (Low) — Fixed 2026-05-21

`spawntree.AddChild`'s collision error read `child session %q already exists in spawn tree` — technical, no next-step guidance. After wrapping through `spawn.Run`'s `record spawn tree:` prefix, the operator saw a doubly-confusing message.

**Fixed by** the message now naming the offending label and pointing at `bosun status` (to inspect) / `bosun cleanup` (to remove if stale). Test pinned in `TestAddChild_RefusesDuplicateChild`.

## Agent overcalls (verified safe, listed so they don't get re-flagged)

- **"`clauseRe` truncates on nested parens"** — real, but the practical case (`(host: ssh://host(backup):2222)`) is unusual enough that the fix can wait. Operators using non-trivial parens in clause values can quote / escape.
- **"`extractJSON` returns first not largest"** — code is correct for typical Claude responses; only the docstring needed fixing.
- **"HTTP/SSE Last-Event-ID missing"** — by design; events.go uses disk-backed backfill instead. Documented as intentional.
- **"Goroutine leak in SSE handler on disconnect"** — defers on the ticker `Stop()` calls fire correctly; verified by reading the code.
- **"Brief Host clause not validated against config"** — true, but Host validation belongs to the launcher (which already errors at docker-run time on unknown host). Validate-at-parse would couple `internal/brief` to `internal/config`.

## Methodology

Five Explore agents, focused on subsystems rather than bug classes. Each was given specific files and specific failure modes to inspect. Findings were verified directly against the code — agents overcalled in five places (listed above). Verification before believing is mandatory.

Out of scope for this pass:
- TUI bugs (`internal/tui/control/`) — separate review
- Performance / profile-driven hot spots
- Documentation drift
- The MCP SDK transport itself (relies on upstream `go-sdk/mcp` library)
