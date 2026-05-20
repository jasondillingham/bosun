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
| Same-second parallel init collision | **Fixed** (PID appended to round timestamp) | Medium |
| Suggest retry-template error growth | **Fixed** (`retryErrorByteCap=256`) | Medium |
| spawn-tree no consistency validation on Load | **Fixed** (`validateTree` rejects self-parent, orphans, cycles) | Medium |
| `git.BranchExists/TreeEqualsBase/MergeYieldsBase` bypass timeout | **Fixed** (`runAllowExitCode` helper) | Low |
| Suggest no retry on 429/5xx | **Fixed** (`transientCallError` + exponential backoff) | Medium |
| HTTP/SSE no connection limit | **Fixed** (`--max-connections` flag, default 64) | Low |
| Init `--force` cleanup has no rollback | **Fixed** (plan-then-execute with completed-list reporting) | Medium |
| Init partial-failure orphan worktrees | **Open** — needs resume-time orphan detection | High |
| Suggest token-budget unbounded | **Open** — needs token estimation or hard byte cap | Medium |
| Subtask label collision unclear error message | **Open** — small UX improvement | Low |

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

## Open — bigger work

### Init partial-failure orphan worktrees (High)

If `bosun init 4` succeeds at creating worktrees for sessions 1–2 and then fails on session 3 (disk full, git lock contention, anything), `istate.MarkComplete` is never called for session 3. On `--resume`, the resume logic skips completed sessions (1 and 2) but doesn't know to also clean up the *partial* state of session 3. The loop re-attempts `git worktree add` for that path, hits "already exists," and refuses.

Fix path (not done): when init detects a partially-created session at resume time, prompt-or-auto-clean the orphan worktree before retrying. Or rework the per-session state machine so partial creation has a "rollback in progress" state distinct from "complete."

### `--force` cleanup has no rollback (Medium)

When `bosun init --force` is run, the cleanup loop removes existing worktrees and branches one-by-one. If removal of worktree 2 fails mid-loop, worktree 1 is already deleted but worktree 3 still exists. There's no rollback. The operator has to fix the state manually.

Fix path (not done): collect the cleanup plan first, validate everything is removable, then perform the cleanup transactionally (or at least surface a clear "partial cleanup, you're at state X" message at the end).

### Parallel init same-second collision (Medium)

The round timestamp is captured at second precision. Two `bosun init` runs in the same UTC second from the same repo produce identical worktree directory names. The second invocation either overwrites the first (under `--force`) or fails on "already exists."

Fix path (not done): include the PID, or millisecond precision, or a small random nonce in the timestamp. The current behavior is documented in a test as expected, but it's documentation of a bug, not a feature.

### spawn-tree no consistency validation on Load (Medium)

`spawntree.Store.Load` decodes the JSON file and checks the schema version, but doesn't validate parent-child consistency:
- Orphaned children (parent label doesn't exist in `Sessions`)
- Self-references (`Sessions["session-1"].Parent == "session-1"`)
- Cycles in the parent chain

Real-world incidence is low — the file is operator-controlled and bosun's own writers maintain consistency. But a corrupt JSON or hand-edited file can land bosun in a state where `ParentOf` walks indefinitely or `EnrichSessions` crashes on a missing entry.

Fix path (not done): add a `validateTree` pass at Load time. Could be lazy (validate-on-walk) instead of eager.

### Suggest no retry on transient HTTP errors (Medium)

`internal/suggest/claude.go:374` doesn't distinguish 429 (rate-limit, retry-worthy) from 4xx (auth-error, terminal). Any non-2xx response fails the whole call. Anthropic's own SDK auto-retries 429s with exponential backoff.

Fix path (not done): classify the response status; retry transient errors with capped backoff, fail-fast on terminal ones. Reuse `maxProposeAttempts` budget or add a separate transport-retry budget.

### Suggest token-budget unbounded (Medium)

Each Propose retry appends 2 messages (assistant + user) to the conversation. After 3 attempts the conversation can be 5KB+ of context. The Anthropic API will charge for it and may clip if it hits the context window. There's no cap or warning.

Fix path (not done): track input bytes (or token estimate), warn at 50% of a sensible budget (say 50K tokens), fail gracefully past 80%. Or just truncate validation-error strings echoed back to ~256 chars to bound growth.

### HTTP/SSE no connection limit (Low)

`bosun serve` accepts unbounded concurrent SSE connections. Each spawns a goroutine + an event-poll timer. An attacker (or curious script) opening 10K connections creates 10K goroutines.

Mitigation in place: the default bind is `127.0.0.1`, so the attack surface is localhost-only by default. Operators who set `--bind 0.0.0.0` opt into the larger surface.

Fix path (not done): add a `--max-connections` flag, or document that operators exposing the dashboard should put nginx in front with connection limits.

### `git.BranchExists/TreeEqualsBase/MergeYieldsBase` bypass timeout (Low)

These three methods call `c.Runner.Run(ctx, ...)` directly instead of `c.run()`, which applies the configured timeout. `BranchExists` is fine (sub-millisecond `show-ref`), but `TreeEqualsBase` calls `git diff` and `MergeYieldsBase` does merge-tree work — both legitimately slow on large repos. Could hang past the configured timeout.

Fix path (not done): refactor to use `c.run()`, with exit-code-1 special-cased instead of returned as error. Small refactor.

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
