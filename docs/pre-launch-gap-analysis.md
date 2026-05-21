# Pre-launch gap analysis (2026-05-18)

A fresh review of bosun's product surface, asking the question:
"setting aside the public-launch flip itself, what's actually
missing from the application?"

Based on: full command surface (`bosun --help`), `make test-cover`
output, `docs/v0.9-trial-3c-findings.md`, `docs/launch-checklist.md`,
and a walk through the internal package structure.

---

## The headline gap: v0.9 is the differentiator and it's broken

Per [`docs/v0.9-trial-3c-findings.md`](./v0.9-trial-3c-findings.md),
`bosun_spawn` has **four open bugs** (A/B/C/D) in the spawn
lifecycle:

- **Bug A** — sub-worktrees can vanish without `spawn-tree.json`
  updates (state divergence)
- **Bug B** — parent agent has no signal when subs die (silent
  solo-work as worst case)
- **Bug C** — `merge --tree` skipped scenarios when subs never
  reach DONE
- **Bug D** — launcher silently fails on rapid-fire spawn

`bosun_spawn` is the feature that distinguishes bosun from
Claude Code's worktree-isolated `Agent` per the README. It's
also the riskiest path (agent-inside-a-session spawning more
sessions). Phase 4b of the launch checklist is gated on a trial
that has not passed.

**This is the #1 thing missing from the app.** Everything below
is polish.

---

## Test coverage maps suspiciously well to where bugs landed

From `make test-cover`:

```
launcher:    40.7%   ← Bug D in trial #3c lives here
doctor:      53.3%
preflight:   61.2%
web:         61.2%
state:       62.9%
claudehook:  73.8%
spawntree:   74.6%
mcp:         78.5%
tui/control: 78.5%
tui:         78.6%
claims:      79.3%
predict:     80.1%
session:     80.9%
lockfile:    80.0%
status:      81.8%
git:         83.3%
brief:       86.8%
init:        86.8%
proc:        89.1%
config:      90.8%
phantom:     91.7%
suggest:     92.6%
hooks:       100.0%
```

`internal/launcher` is the least-tested package in the repo and
it's the one that silently failed during the v0.9 trial. The
pattern is clear: every place coverage dipped below 70% is a
place we've already taken a hit. Spawn-related lifecycle work
should not ship to public users with a 40% coverage launcher
underneath it.

---

## Repo hygiene gaps for going public

The launch checklist treats these as not-blocking, but the
moment the repo flips public these are visible-by-absence:

- **No `CONTRIBUTING.md`** — first PR-er has no idea what to do
- **No `SECURITY.md`** — ironic given the safety contract is the
  load-bearing trust signal; nowhere to report a contract
  violation
- **No `CODE_OF_CONDUCT.md`** — most community-tooling repos
  have one
- **No `.github/ISSUE_TEMPLATE/`** — bug reports, especially
  safety-contract violations, will be free-form prose
- **No `PULL_REQUEST_TEMPLATE.md`** — contributors won't know to
  run `make check`

---

## Distribution story is bare-bones

```
go install github.com/jasondillingham/bosun/cmd/bosun@latest
```

That's the only install path in the README. The Makefile has a
`cross` target that builds for darwin/linux/windows × amd64/arm64,
but nothing publishes those binaries. A non-Go-user (a Claude
Code user who's a frontend dev, say) cannot `brew install bosun`
or `curl | sh` or download a release artifact.

Specifically missing:

- **GoReleaser** config (or hand-rolled GitHub Actions release
  workflow)
- **Homebrew tap** (`brew tap jasondillingham/bosun`)
- **Prebuilt binaries on GitHub Releases** — the `cross` target
  needs to be wired to release tags
- **Install one-liner** (`curl -sSL https://.../install.sh | sh`)

For a tool whose audience is "people running Claude Code," half
of whom are not Go programmers, this gates adoption hard.

---

## CI is the bare minimum

`.github/workflows/ci.yml` runs `go vet`, `go build`, `go test
-race` on three OSes. Good baseline. But:

- **No `golangci-lint`** — for a 39k-LOC codebase we should be
  catching `errcheck`, `staticcheck`, `gosec`, `ineffassign`
  violations
- **`make fuzz` and `make stress` exist but aren't in CI.** They're
  "run periodically." Periodically becomes never. At minimum run
  them on a weekly schedule via `workflow_dispatch` or cron.
- **No coverage reporting** — no Codecov badge, no per-PR
  coverage delta to catch regressions
- **No release automation** — tagging `v0.8.0` won't currently
  produce release artifacts
- **No `gosec` or dependency scanning** (Dependabot is free)

---

## README is missing the things that earn stars

It's an excellent technical README but it's missing the elements
that turn a stranger's scroll into a star:

- **No badges row** (CI, license, Go version, latest release,
  coverage)
- **`demo.gif` exists in the repo root but isn't embedded in
  the README.** Five-minute fix, biggest first-impression delta.
- **No TUI screenshot.** `bosun tui` is the Bubbletea control
  center — the most visually impressive thing in the project.
  Not a single image of it.
- **No "Who's using bosun" section.** Empty by definition pre-
  launch, but reserve the spot.
- **No FAQ.** First question every reader has: "How is this
  different from just using `git worktree` manually?" Second:
  "What if I'm not using Claude Code?" Both deserve answers in
  the README, not buried in SPEC.md.
- **No asciinema/asciicast.** Real terminal recording > static
  gif for a CLI tool.

---

## Functional gaps that look like 1.0 blockers

These are explicit non-goals or deferred features in the spec,
but a daily-use coordination tool feels incomplete without them:

1. **No `--watch` / live status mode.** SSE exists in `bosun
   serve` but the table form (`bosun status`) is one-shot. The
   most natural workflow ("keep watching while I work") requires
   the user to set up `watch -n 2 bosun status` themselves.
2. **No `bosun events --tail`.** The web server has an event
   stream but no terminal-side consumer of it.
3. **No PR/forge integration even as opt-in.** The safety
   contract correctly says "never talks to a forge" by default —
   but there's no `bosun merge --push` or `bosun pr-create`
   opt-in either. A user who finishes 4 sessions and wants to
   open 4 PRs has to drop back to `gh`.
4. **No `bosun attach <session>`.** `bosun launch` spawns a new
   agent window; there's no way to re-attach to an agent that's
   already running if the operator lost the terminal.
5. **No `bosun version` in the help output.** Embed git SHA at
   build time.
6. **No "session history" or journal.** Once a session is
   `cleanup`'d, its claims/commits/brief are gone. A
   `.bosun/history/` archive would let users grep "what did
   session-2 do last week."
7. **Conflict prediction is regex-heuristic only.** `bosun
   predict` reads paths from a plan markdown. For Go-aware
   prediction (the language bosun is written in!) parsing
   import graphs would predict actual symbol-level conflicts.
   The current prediction is shallow enough that trial findings
   show it false-positives on "Files (avoid)" sections.

---

## Observability gaps

- **No structured logging.** When something goes wrong (Bug A
  in trial #3c — sub-worktrees disappearing under unclear
  circumstances), there's no log file to inspect. A `.bosun/logs/`
  ring buffer of recent commands + their outcomes would have made
  that bug trivially diagnosable.
- **No `bosun debug` command.** When a user reports an issue,
  "send me the output of `bosun debug`" should produce a
  self-contained bundle (config, recent state, MCP socket
  history, doctor output).
- **No MCP socket tracing.** When the agent calls a bosun tool
  that fails, there's no way to see the request/response without
  rebuilding with logging.

---

## Trial coverage breadth

The external-repo trials have all been against
**`homelab-status-mcp`** — one Go MCP server repo. The safety
contract has not been exercised against:

- A monorepo (10k+ files)
- A polyglot repo (Go + TypeScript + Python)
- A repo with git submodules
- A repo with git-LFS
- A Windows-native repo in actual day-to-day use (CI runs
  Windows, but no human trial)
- A repo with pre-commit hooks (husky, lefthook) that bosun's
  commits would trigger

The biggest "what if" for a public launch is a user filing
"bosun corrupted my monorepo" on day 2. That fear is reduced by
deliberately trialling against shapes we haven't yet.

---

## Priority order

1. **Fix v0.9 spawn lifecycle (Bugs A-D).** Add `spawntree.Sync()`
   reconciliation. Add `bosun_check_tree` MCP tool. Get the
   launcher to a 70%+ coverage and write the rapid-fire spawn
   repro test mentioned in the trial findings.
2. **Embed `demo.gif` in the README and add a TUI screenshot.**
   Five minutes of work, biggest first-impression delta.
3. **Wire `make fuzz` and `make stress` into CI on a weekly
   schedule.** They exist; just run them.
4. **GoReleaser + Homebrew tap.** Removes the "must have Go
   installed" barrier for half the potential audience.
5. **Add `CONTRIBUTING.md`, `SECURITY.md`, issue templates.** One
   hour total. Makes the public flip feel professional rather
   than research-grade.
6. **Run trial #4 against a non-MCP-server codebase.** Pick a
   polyglot or larger Go repo. This is the gate against day-2
   "bosun corrupted my monorepo" reports.
7. **`bosun status --watch`.** The single most-natural-to-want
   feature we don't have.
8. **`golangci-lint` + Dependabot + coverage badge.** Standard
   hygiene that pays off on the first PR.

The trial-gated discipline shown so far is the right answer here
too: don't pile on features, fix the spawn lifecycle, ship the
polish, run one more trial against a different shape of repo,
then flip.
