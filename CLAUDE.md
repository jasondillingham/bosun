# CLAUDE.md — Operating instructions for Bosun

If you're a Claude Code session working on the Bosun codebase, read this before you start.

## What you're building

Bosun is a Go CLI that coordinates parallel Claude Code sessions on isolated git worktrees. **Read `SPEC.md` for the full v0.1 spec.** That document is authoritative for scope.

## Release stance

**Bosun is public under Apache 2.0** (since v0.8.0). The launch
checklist's gates — robust kickoff, `bosun doctor` preflight,
`bosun init --suggest` for one-step onboarding, and an external-repo
trial that validated the safety contract end-to-end — were cleared.
See [`docs/v0.8-trial-findings.md`](./docs/v0.8-trial-findings.md)
for what the gate-clearing trial actually exercised.

What that means for contributors and Claude Code sessions working
in this repo:

- The `main` branch is public. Everything you commit and push is
  visible to anyone on the internet. Treat the repo as if it were
  always going to be read by a stranger.
- Never commit secrets (API keys, tokens, passwords). The repo's
  history is already auditable; a future commit can't redact the
  past.
- Marketing-shaped writing (README rewrites for HN, demo GIFs, blog
  posts) is fair game now. Keep claims accurate — overpromising
  hurts the next person who tries the tool.
- The safety contract in the README is load-bearing trust. Don't
  weaken it without surfacing the change explicitly and updating
  the README in the same change.

## Scope discipline (most important rule)

`SPEC.md` lists what's in v0.1 and what's explicitly NOT. Do not extend into v0.2+ work even when the codebase invites it. Examples of common drift:

- ❌ Adding an MCP server interface to `init` "because it would only take a few hours"
- ❌ Adding a watch mode to `status` "because users will obviously want this"
- ❌ Adding hooks/extensibility points "for future use"
- ❌ Adding custom session names beyond `session-N`

If you're tempted to add something not in the v0.1 spec, write a TODO in `docs/v0.2-deferred.md` and keep moving.

## Ground truth: Leonard

This repo is wired to **Leonard** — a local-first MCP server that
keeps you honest against the live codebase. Use it. The store lives
at `.leonard/` (gitignored), the symbol index is built from
`leonard index`, and the post-edit hook runs `go vet ./...` after
every `Edit`/`Write` and logs the result.

Three surfaces exposed under the `mcp__leonard__*` tool namespace:

- **Symbol index** — `find_symbol(query)` / `verify_symbol(name)`
  resolve a name against the actual source tree, not your memory of
  it. Names in handoff docs and auto-memory rot; verify before you
  recommend or edit.
- **Decision log** — `record_decision`, `get_decisions`,
  `supersede_decision`, `get_stale_decisions`. Capture non-trivial
  calls (deferrals, "tried X, picked Y because…", scope cuts) so
  the next session reads them instead of re-litigating.
- **Claim ledger** — every `Edit`/`Write` triggers the verifier and
  the pass/fail gets recorded as a claim. `get_unverified_claims()`
  surfaces edits whose verifier didn't confirm.

When to call which tool:

- **At session start, and when a doc references a symbol you plan to
  lean on:** `verify_symbol` it. If it fails, the doc is stale —
  trust the live code and update or remove the stale reference.
- **After a non-trivial decision:** `record_decision` with topic,
  rationale, and related files/symbols. Future-you will thank
  present-you.
- **Before closing out a coding session:** `get_unverified_claims()`
  to catch edits the verifier flagged that you missed.

If a future session needs to change the verifier (e.g., add
`golangci-lint`), edit `.leonard/config.toml`'s `[post_edit.verify]`
block, then re-trust via `leonard config trust` — Leonard refuses to
run an unauthorized command.

## Conventions

- **Go 1.23+**, idiomatic Go style (gofmt, golangci-lint default rules)
- **Dependencies:** Cobra for CLI wiring. `golang.org/x/term` for TTY detection if needed. **No other third-party deps** without strong justification.
- **Git operations:** Always via `os/exec` calling the `git` binary. Never `go-git` or git plumbing internals.
- **Paths:** Always `path/filepath`. Never string concatenation for paths.
- **Errors:** Wrap with `fmt.Errorf("%w", err)`. User-facing errors prefixed with `bosun: `.
- **Exit codes:** 0 success / 1 user error / 2 git error / 3 internal error.

## Testing standards

- Unit tests for every public function in `internal/`
- Table-driven tests for parsers, formatters, validators
- Integration tests use `t.TempDir()` to create a real git repo, then exercise commands end-to-end
- Run `go test -race ./... -count=1` before considering anything done — race-clean is non-negotiable

## Cross-OS care

The single biggest source of bugs in CLI tools is OS-specific path handling. Defensive habits:

- Build a Windows VM or use GitHub Actions Windows runner for testing — don't trust that macOS+Linux passing means Windows passes
- `filepath.Join`, never `"/"` concatenation
- Watch for case-sensitivity differences (macOS HFS+ is case-insensitive by default; Linux ext4 is case-sensitive)
- Test the install path: a fresh user clones the repo, runs `go build`, and the binary works on their OS

## Git operations that need special care

- **Worktree paths:** Use absolute paths internally even if relative ones work — Windows sometimes resolves relatives differently
- **Branch deletion:** Use `git branch -D` (force) only when explicitly requested via `--force` flag. Default behavior is `git branch -d` (safe, refuses if unmerged)
- **`git worktree list --porcelain`:** Parse this carefully — output format is line-oriented with blank lines as record separators. Test against fixtures.
- **`git status --porcelain`:** Counts untracked files (`??` lines) — filter these out for the DIRTY column per spec.

## Parallel-friendly architecture

Since this tool's whole purpose is enabling parallel Claude Code sessions on the codebase you're building, please make the codebase itself parallel-friendly:

- **Small, focused files.** Easier for parallel sessions to work without stepping on each other.
- **Clean package boundaries.** `internal/git/`, `internal/session/`, `internal/status/` etc. should be independently testable.
- **No global state.** No package-level mutable state. No init() side effects beyond Cobra registration.

If you can split the work across `internal/git/`, `internal/status/`, `internal/config/` as natural parallel work streams, do so.

## When to stop

v0.1 is done when **all 15 acceptance criteria in SPEC.md pass.** Resist the urge to keep building. Ship v0.1, get it on a user's machine, get feedback, then plan v0.2.

## When in doubt, ask

The spec is detailed but not exhaustive. If you hit a real ambiguity:

1. Document the question in `docs/QUESTIONS.md` with proposed resolution
2. Make the reasonable call and continue
3. Surface the question in the PR description for review

Better to ship with documented decisions than to block on perfection.
