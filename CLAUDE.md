# CLAUDE.md — Operating instructions for Bosun

If you're a Claude Code session working on the Bosun codebase, read this before you start.

## What you're building

Bosun is a Go CLI that coordinates parallel Claude Code sessions on isolated git worktrees. **Read `SPEC.md` for the full v0.1 spec.** That document is authoritative for scope.

## Release stance (read this before any public-facing action)

**Bosun is NOT public yet.** The repo is private; the v0.2.0 tag is local-only;
RELEASES.md is intentionally not yet caught up. Plan is to open-source under
Apache 2.0 once the tool is well-tested enough that someone landing on the
GitHub page from a Hacker News thread has a good experience — that means
robust kickoff (no silent init hangs), a working `bosun doctor`-style
preflight, and a brief-authoring assistant that lets a new user succeed
without writing 6 disjoint briefs from scratch.

Until then, **do not**:

- Push to a public origin / create a public mirror
- Generate marketing material (README rewrites for HN, demo GIFs that
  would get linked anywhere, blog post drafts that say "available now")
- Set the GitHub repo visibility to public

You can absolutely:

- Improve the README for future public-launch readiness
- Draft (not publish) blog posts in `docs/blog/`
- Write release notes in `RELEASES.md`
- Build distribution scaffolding (Homebrew formula, prebuilt binaries) and
  keep it dormant

When the maintainer is ready to go public, they'll say so explicitly and
remove this section. Don't take silence as permission.

## Scope discipline (most important rule)

`SPEC.md` lists what's in v0.1 and what's explicitly NOT. Do not extend into v0.2+ work even when the codebase invites it. Examples of common drift:

- ❌ Adding an MCP server interface to `init` "because it would only take a few hours"
- ❌ Adding a watch mode to `status` "because users will obviously want this"
- ❌ Adding hooks/extensibility points "for future use"
- ❌ Adding custom session names beyond `session-N`

If you're tempted to add something not in the v0.1 spec, write a TODO in `docs/v0.2-deferred.md` and keep moving.

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
