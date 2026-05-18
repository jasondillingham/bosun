# Contributing to Bosun

Thanks for considering a contribution. Bosun is a small, scope-disciplined
project. This document covers what we accept, what we don't, and the
mechanics of getting a change in.

## Before you start

- Read [`README.md`](./README.md) for what Bosun is and the
  safety contract it commits to.
- Read [`SPEC.md`](./SPEC.md) for the authoritative v0.1 scope.
- Read [`CLAUDE.md`](./CLAUDE.md) if you're using Claude Code (or any
  agent) to help with your change — it documents the conventions
  and the no-skip-hooks rule.

## Scope

Bosun's roadmap is laid out in `README.md` under "Roadmap" and the
in-scope command surface is fixed by `SPEC.md`. Out-of-scope additions
are routinely rejected even when they look small. Common drifts:

- ❌ MCP server extensions beyond what's already shipped
- ❌ Hooks or extensibility points "for future use"
- ❌ A watch mode, daemon, or background service that wasn't in the spec
- ❌ Custom session-naming schemes beyond `session-N` and the v0.9
  hierarchical labels (`session-1.auth`)
- ❌ Anything that pushes to a remote, fetches, or talks to a forge

If you're unsure whether a change is in scope, open an issue with the
`feature` template **before** writing code. We'd rather discuss a
one-paragraph proposal than reject a finished PR.

In-scope contributions we welcome:

- ✓ Bug fixes for shipped behavior
- ✓ Cross-platform fixes (especially Windows; see `README.md` for
  current platform status)
- ✓ Documentation improvements and clarifications
- ✓ Test coverage for under-tested code paths
- ✓ Performance fixes for measurable regressions

## Development setup

```sh
git clone https://github.com/jasondillingham/bosun.git
cd bosun
go build -o bosun ./cmd/bosun
./bosun --version
```

You need Go 1.25+ and Git ≥ 2.40. `bosun doctor` will tell you if
anything in your environment looks off.

## Running tests

`make check` is the single command to run before committing anything
non-trivial. It runs:

- `go vet ./...`
- `go test -race -count=1 ./...` (the whole suite, including
  scenario and scale tests, race-clean)
- A demo dry-run to catch regressions in the example flow

```sh
make check
```

Other useful targets:

- `make test` — unit tests without the race detector (faster)
- `make test-race` — race-detector run only
- `make fuzz` — every `Fuzz*` target for 30s each (override with
  `FUZZTIME=5m make fuzz`)
- `make stress` — stress + concurrency tests

`make check` must pass before you push. Race-clean is non-negotiable.

## Release dry-run

Releases are produced by GoReleaser via [`.github/workflows/release.yml`](./.github/workflows/release.yml) on `v*` tag pushes. To exercise the same config locally without publishing anything:

```sh
goreleaser release --snapshot --clean --skip=publish
```

That builds all six platform archives (darwin / linux / windows × amd64 / arm64) plus a `checksums.txt` into `./dist/`. `dist/` is gitignored, so the repo stays clean. Install GoReleaser via `go install github.com/goreleaser/goreleaser/v2@latest` if you don't have it.


## Pre-commit hooks

**Do not skip hooks.** `--no-verify` is forbidden on this repo.
If a hook fails, fix the underlying issue. The hooks exist to keep
`main` clean; bypassing them defeats the purpose.

## Submitting a change

1. Fork and create a topic branch.
2. Make your change. Keep it focused — one logical change per PR.
3. Add or update tests. Every public function in `internal/` should
   have unit-test coverage; new commands need integration tests.
4. Run `make check`. Fix anything it reports.
5. Commit with a descriptive message. Reference an issue if one
   exists.
6. Open a PR using the template at `.github/PULL_REQUEST_TEMPLATE.md`.
   The template's checklist matches what reviewers look for.

## Using Bosun to develop Bosun

Bosun is built to run parallel agent sessions on its own codebase.
If you want to use it that way for your contribution:

1. Sketch the work as a plan markdown file with one section per lane.
   See `examples/` for prior plans.
2. Run `bosun predict <plan.md>` to check for cross-lane conflicts
   before launching sessions.
3. Run `bosun init <N> --brief <plan.md> --launch` (or use
   `init --suggest "<goal>"` to draft the plan from a goal description).
4. Each session reads its `BOSUN_BRIEF.md` and works the lane.
5. Sessions run `bosun done` when ready; you run `bosun merge` to
   squash them back to your topic branch.
6. Open one PR from the topic branch.

The brief for each lane should match the same scope discipline as a
hand-written PR. Lanes that drift into v0.2+ work get rejected the same
way solo PRs do.

## Tone and style

- Match the existing voice in `README.md` and `SPEC.md`: concise,
  factual, no marketing language.
- Comments only when the *why* is non-obvious. Don't restate what the
  code does.
- `gofmt` and `golangci-lint` defaults. `make check` enforces vet.
- User-facing errors are prefixed `bosun: `.
- Exit codes: 0 success / 1 user error / 2 git error / 3 internal error.

## Reporting bugs

Use `.github/ISSUE_TEMPLATE/bug_report.md`. Include `bosun --version`
and `bosun doctor` output. If you suspect a safety-contract violation
(see `README.md`), use the safety_violation template instead — those
get priority routing. For private disclosure, see `SECURITY.md`.

## Code of conduct

By participating, you agree to abide by the
[Contributor Covenant](./CODE_OF_CONDUCT.md). Report violations to
jasonmdillingham@gmail.com.

## License

Contributions are licensed under Apache 2.0, the same license as the
project — see [`LICENSE`](./LICENSE). You retain copyright on your
contribution; opening a PR signals you have the right to license it
this way.
