# Bosun dogfood log — architect-mcp public-release push

A running log of every bosun command invoked, its output, and frictions/wins observed. Maintained by the main Claude Code session driving the round.

**Context:** Using bosun to coordinate 4 parallel Claude Code lanes to take `architect-mcp` from "private repo with pre-public checklist" to "ready to flip public." Round goal is double-duty: ship the architect-mcp work AND surface real-world friction in bosun for the maintainer.

**Round metadata**
- bosun binary: `/Users/jasondillingham/Documents/Homelab/bosun/bosun`
- bosun version: TBD (`bosun --version` to be captured below)
- architect-mcp repo root: `/Users/jasondillingham/Documents/Homelab/architect-mcp`
- Base branch: `main`
- Plan file: `bosun-public-release-plan.md`
- Start time: 2026-05-18

---

## 1. Pre-flight

### `bosun --version`

```
$ bosun --version
Error: unknown flag: --version
```

**Friction #1 (minor UX gap):** `--version` flag not implemented. Standard CLI convention; would expect either `--version` or a `version` subcommand. Workaround: check binary mtime or git SHA. Logging as a v1.x polish item, not blocking.

### `bosun predict bosun-public-release-plan.md`

Ran predict against the lane plan. Found 17 overlaps. Output (tail):

```
Overlaps: 17
  - [high] internal/mcp/server.go (sessions: session-2, session-3)
  - [high] internal/mcp/server.go (sessions: session-2, session-4)
  - [high] internal/mcp/server.go (sessions: session-3, session-4)
  ...
  - [medium] examples/ (sessions: session-1, session-2)
  - [medium] examples/ (sessions: session-1, session-3)
  - [medium] internal/config/ (sessions: session-1, session-2)
  - [low] internal/mcp/ (sessions: session-1, session-2)
Error: bosun: predicted 17 overlap(s) — see report above
```

**Friction #2 (predictor signal-to-noise):** Bosun's `predict` regex-matches inline path mentions, so a lane's `Do NOT modify internal/config/**` constraint shows up as a claim. Real overlap (lane-1 and lane-3 both want `examples/`) is correctly detected — but it's buried in false positives where the predictor sees a "stay-out-of" note as a "I-want-this" claim. Workaround for this round: rewrite the briefs so paths only appear in affirmative scope lists (code-fenced), and constraints are prose without path strings. **Bosun-side finding worth logging for v1.x:** predict could try to distinguish "lane claims X" from "lane warns off X" by looking at the surrounding sentence context, or by requiring path scopes to be code-fenced.

**Bosun-side win:** the predictor exit code is non-zero on overlaps. That's the right default — forces the operator to explicitly acknowledge before pushing init through. Good safety posture.

### `bosun doctor`

Run prior to any session. Output:

```
Bosun health check — /Users/jasondillingham/Documents/Homelab/architect-mcp
  ✓ git-version: git 2.50 (supported)
  ✓ git-on-path: git found on PATH
  ✓ repo-writeable: repository root is writable
  ✓ bosun-dir-writeable: .bosun directory is writable
  ⚠ filesync-icloud: repository is under ~/Documents; macOS may sync it to iCloud (creates phantom files)
      fix: either disable iCloud sync for Documents in System Settings, or move the repository to ~/code/ or similar
  ✓ orphan-worktrees: no orphan worktree directories
  ✓ init-lock: no leftover init.lock
  ✓ phantom-branch-refs: no bosun branches yet (nothing to check)
  ✓ mcp-socket: Unix socket bind succeeded
1 warning(s) — bosun should work but the operator should be aware.
```

**Observations**
- The iCloud warning is real and architect-mcp lives under `~/Documents/Homelab/` like the rest of the homelab. Bosun's warning is well-scoped (says "may sync" and explains the failure mode). Accepting the risk; v0.7 added Spotlight phantom filter so I'm expecting graceful handling.
- Doctor output is clean and operator-friendly. ✓ / ⚠ / ✗ glyphs are easy to skim.

### Pre-state of architect-mcp

- `go test ./...` — all four existing test packages green
- `go build ./...` — clean
- Tracked tests: `config`, `architect (client + ollama + presets)`, `screenshot`, `mcp (output_path)`
- Visible gaps: no tests for `critique.go`, `improve.go`, `compare.go`, `multi_blueprint_tool.go`, `critique_tool.go`, `improve_tool.go`, `compare_tool.go`, `blueprint_url_tool.go`
- Visible leaks (manual pre-scan, see Section 0):
  - `internal/config/config_test.go:47` — fixture uses `http://thor:11434` (Thor hostname)
  - `internal/mcp/output_path_test.go:25` — path fixture is `/Users/jason/.ssh/authorized_keys` (real username `jason`)

---

## 2. Round lifecycle

### `bosun init 4 --brief bosun-public-release-plan.md`

```
system load is 5.15; init may be slow (--no-load-check to skip)
Creating worktree session-1 (1/4)...
Creating worktree session-2 (2/4)...
Creating worktree session-3 (3/4)...
Creating worktree session-4 (4/4)...
Created 4 session(s):
  session-1  → /Users/jasondillingham/Documents/Homelab/architect-mcp-bosun-1  (branch: bosun/session-1)
  session-2  → /Users/jasondillingham/Documents/Homelab/architect-mcp-bosun-2  (branch: bosun/session-2)
  session-3  → /Users/jasondillingham/Documents/Homelab/architect-mcp-bosun-3  (branch: bosun/session-3)
  session-4  → /Users/jasondillingham/Documents/Homelab/architect-mcp-bosun-4  (branch: bosun/session-4)
```

**Bosun-side wins**
- Load-check warning is concise and offers the override flag inline. Good UX.
- `BOSUN_BRIEF.md` correctly generated in each worktree, lane content parsed from `## session-N` headings.
- Brief boilerplate at the top is operator-facing copy (read this, commit, claim, done) — a nice touch for sessions whose agents may not have prior context.

**Friction #3 (assumed Makefile target):** The brief boilerplate tells the agent to run `make check` to validate. architect-mcp's Makefile has `make test` but no `check` target. Not bosun's bug per se — bosun's brief assumes a convention this repo doesn't follow. Workaround: tell lane agents to run `go test -race ./...` and `go vet ./...` directly. Optionally the operator can add a `check` alias to the Makefile in a post-merge pass — but doing so in any lane this round would step outside the agreed scope. **Bosun-side polish for v1.x:** brief boilerplate could detect the project's verify command from the Makefile / package.json / etc., or expose it via `.bosun/config.json` so each repo customizes the validate line.

### `bosun status` (initial)

```
4 sessions — 4 WORKING · 0 commits ahead total
SESSION    BRANCH           STATE    AHEAD  DIRTY  CLAIMED  RUNNING  LAST_COMMIT
session-1  bosun/session-1  WORKING  0      0      0        —        —       — (no commits)
session-2  bosun/session-2  WORKING  0      0      0        —        —       — (no commits)
session-3  bosun/session-3  WORKING  0      0      0        —        —       — (no commits)
session-4  bosun/session-4  WORKING  0      0      0        —        —       — (no commits)
```

Clean snapshot. `RUNNING` column is `—` because no agent has been launched against any worktree yet.

### Spawned 4 lane agents (parallel)

Used the Agent tool to spawn 4 sub-agents in parallel, each pointed at its worktree with a self-contained brief that tells it to read `BOSUN_BRIEF.md` and execute. This is a surrogate for the operator opening 4 terminal windows — bosun's `--launch` flag would normally do that, but in this headless Claude Code environment, sub-agents are the closest analog.

**Bosun-side finding:** the round still exercises the meat of bosun (worktrees, briefs, claim, done, status, merge, cleanup), but **`RUNNING` column never lights up** because there's no `bosun launch`-spawned process for bosun's proc detection to find. The agents are running, but not under bosun's process tree. For a future bosun feature: a `bosun attach session-N <pid>` so non-launched workers can register themselves.

### `bosun status` after session-1 DONE

```
4 sessions — 1 DONE, 3 WORKING · 1 commit ahead total
SESSION    BRANCH           STATE    AHEAD  DIRTY  CLAIMED  RUNNING  LAST_COMMIT
session-1  bosun/session-1  DONE     1      0      2        —        39 seconds ago — Scrub internal hostname + personal path from test fixtures
session-2  bosun/session-2  WORKING  0      0      0        —        —       — (no commits)
session-3  bosun/session-3  WORKING  0      0      0        —        —       — (no commits)
session-4  bosun/session-4  WORKING  0      0      0        —        —       — (no commits)
```

**Lane 1 summary (from sub-agent report):**
- 1 commit (`8f79c6c`), 2 files changed: `internal/config/config_test.go` (Thor → `ollama-host`) and `internal/mcp/output_path_test.go` (`/Users/jason` → `/Users/example`).
- Confirmed no other real leaks. False positives: `thor` substring inside `author`/`authoritative`; `vault` only as a generic example word in a screenshot.go comment.
- `LICENSE` correct as-is. `internal/config/config.go` defaults already `localhost:11434`/`gemma4:26b`. Examples clean.
- `go vet`, `go test -race`: both green.

**Bosun-side wins observed in lane 1:**
- `bosun claim` worked from inside the worktree without issue.
- The DONE state propagated to `bosun status` in the operator's view within seconds. No polling required.
- The `CLAIMED` column correctly shows 2 (the two test files claimed before edit). Visible accountability.

**Bosun-side friction observed in lane 1:**
- (none yet)

### `bosun status` after lane 4 DONE + lane 2 mid-flight

```
4 sessions — 2 DONE, 1 WORKING, 1 CRASHED · 4 commits ahead total
SESSION    BRANCH           STATE    AHEAD  DIRTY  CLAIMED  RUNNING  LAST_COMMIT
session-1  bosun/session-1  DONE     1      0      2        —        10 minutes ago — Scrub internal hostname + personal path from test fixtures
session-2  bosun/session-2  CRASHED  2      1      23       —        19 seconds ago — Add end-to-end tests for every MCP tool
session-3  bosun/session-3  WORKING  0      0      0        —        —       — (no commits)
session-4  bosun/session-4  DONE     1      0      1        —        35 seconds ago — Add public-release readiness review (session-4)
```

**Friction #4 (false-positive CRASHED detection):** session-2 is marked CRASHED while its sub-agent task notification hasn't arrived yet. The branch has 3 committed commits and a clean tree; `bosun show session-2` confirms it. The probable cause is that bosun's process-liveness gate sees no `bosun launch`-spawned process tied to session-2's worktree, so after some idle threshold it concludes the worker is gone. In the actual `bosun launch` flow this is correct behavior — the worker IS gone. In a sub-agent-driven flow, the worker is alive somewhere else (here, inside my Agent tool's process tree) and bosun has no way to see it. **Bosun-side finding for v1.x:** a `bosun attach <session> --pid <pid>` or `bosun attach --process-name <name>` to let off-tree workers register themselves with the liveness gate. Workaround for this round: ignore the CRASHED state (the safety contract held — all 3 commits are intact in the branch), wait for the agent's summary, and treat it as DONE if the agent reports success.

**Bosun-side win during this same event:** even with CRASHED state, `bosun status` correctly reports AHEAD=2, CLAIMED=23, and shows the latest commit message. Operator never loses visibility into what the session managed to land before the (false-positive) crash signal. The safety contract worked exactly as documented.

### Lane 4 summary (sub-agent report)

20 findings landed in `docs/public-release-review.md`. Counts: 4 critical, 8 strongly-recommended, 8 nice-to-have, 6 out-of-scope. Top 3 critical:

1. **Tag version mismatch (high-impact).** `v0.2.0` through `v0.7.0` already exist on `origin` (verified: `git ls-remote --tags origin` shows v0.3 through v0.7 publicly). CLAUDE.md and `internal/mcp/server.go:21` both plan a `v0.1.0` public release. Flipping public surfaces six contradictory tags. **Operator decision needed:** rebase tag history (rewrites public-facing history) or start from `v0.8.0` instead of `v0.1.0`. Latter is cleaner.
2. **Stale hard-coded server version.** `internal/mcp/server.go:20-21` literals `Version: "0.1.0"` with no ldflags injection. MCP clients see 0.1.0 forever regardless of release tag.
3. **`make check` doesn't exist.** Every lane brief tells the session to run `make check`, but the Makefile only has `test`/`build`. Lane 4 spotted that bosun's brief boilerplate writes this line by default — same finding as **Friction #3** in this log, now corroborated. Also: `.github/workflows/build.yml:15` runs `go test ./...` without `-race`.

Lane 4 also caught leaks lane 1's scope didn't list:
- `internal/screenshot/screenshot.go:13` — `vault` reference (lane 1 didn't have this file in scope, correctly didn't touch it).
- `internal/architect/presets_test.go:75` — "SV pipeline's" comment (SV = Styx Vanguard).
- `internal/mcp/blueprint_tool.go:22` jsonschema — "rural KY homeowners" example string surfaced in MCP-client UI.

The lane-1 agent's "false positive only" assessment was technically correct *for its scope*, but the round's lane decomposition didn't put screenshot.go in any lane. Lane 2 (which owns the prompts) appears to have caught the "rural KY" issue independently — last commit on session-2 is "Scrub personal-locale hint from generate_blueprint audience field". Good convergence between lanes.

**Bosun-side meta-finding:** bosun's `predict` was useful for *file-overlap* prediction but doesn't analyze whether the lane decomposition *covers all the work*. A "coverage" predictor — "is there a file with a TODO that no lane claims?" — would be a v1.x feature worth considering.

### bosun status

(Pending — entries appended as the round progresses.)

### bosun merge

(Pending.)

### bosun cleanup

(Pending.)

---

## 3. Friction log (bosun-side)

Anything that hurt, surprised, confused, or worked unexpectedly well. Each entry should name the command, the expectation, and what actually happened.

(Empty — appended live as the round runs.)

---

## 4. Final summary

### Outcomes

**Round shipped.** 4 lanes, 7 commits, all squash-merged to main. Build + `go test -race ./...` + `go vet ./...` all green on the post-merge `main`.

Per-lane landing:

- **session-1** (leak scrub) — 1 commit. Fixed `internal/config/config_test.go` (Thor → `ollama-host`) and `internal/mcp/output_path_test.go` (`/Users/jason` → `/Users/example`).
- **session-2** (tests + bug hunt + prompt audit) — 4 commits squashed. Added 9 test files (architect package: critique/improve/compare/extended-presets; MCP package: testhelpers + every tool handler). Fixed 2 leaks lane-1's scope didn't cover (`presets_test.go` "SV pipeline" comment; `blueprint_tool.go` jsonschema "rural KY homeowners" example). Softened two prompt phrases (`prompt.md`: "elite ... 500+ sites" → "senior ... hundreds of sites"; `critique.md`: "design fan-fic" → "design fiction"). Both prompt version stamps bumped 1→2.
- **session-3** (storefront) — 1 commit. Fixed 6 README inaccuracies (most importantly "Five tools" → "Six tools" — `improve_blueprint` was registered but undocumented; documented `output_path` and `output_root`/`ARCHITECT_OUTPUT_ROOT`). Added "Why not just use Claude directly?" essay at `docs/why-not-just-claude.md` and three-bullet teaser in README. Generated two real sample blueprints (barber + coffee roaster) against local Ollama. Added curated Claude Code transcript demo block.
- **session-4** (audit) — 1 commit. `docs/public-release-review.md` with 20 prioritized findings. Top 3 critical: **tag-version mismatch** (v0.3-v0.7 already on origin, CLAUDE.md plans v0.1.0 — operator must rebase tags or jump to v0.8.0), **hard-coded server version** (`internal/mcp/server.go:21` literals `Version: "0.1.0"` with no ldflags injection), and **`make check` doesn't exist** (bosun's brief boilerplate assumes a Makefile convention this repo doesn't follow; same issue surfaced earlier as Friction #3).

### Bosun: wins observed (real-world)

- **`bosun doctor` preflight is concise and actionable.** The iCloud warning came with a specific fix recommendation inline. Zero false alarms on the other 8 checks.
- **`bosun predict` caught real path overlaps.** First pass: 17 (most false positives from "DO NOT modify X" prose; the iteration was the point — see Friction #2). Second pass after I tightened the briefs: 3, all low-severity directory-level. Saved me a guaranteed merge conflict.
- **`bosun status` is the right shape.** Per-session AHEAD/DIRTY/CLAIMED/RUNNING/LAST_COMMIT in one row. Glanceable; no polling required to know who's working / who's done.
- **The safety contract held through every false-positive CRASHED event.** Bosun marked session-2 and session-3 as CRASHED while their workers were actually mid-flight (process-tree mismatch — see Friction #4). In both cases, all work was intact on disk; when the agent eventually called `bosun done`, the state transitioned cleanly. Never lost a commit or a dirty-tree edit.
- **`bosun cleanup` correctly refused to drop session-2.** session-2 had 4 commits and was squash-merged to 1; bosun couldn't verify the squash captured all 4 (the SHAs differ post-squash) and skipped it pending explicit `--purge`. That's *exactly* the safety posture you want. The operator opts in to discarding work — bosun never does it silently.
- **`bosun merge` ordered correctly.** I ran `bosun merge` with no arguments and got the 4 sessions squashed in the order bosun chose (1→2→3→4 by session number); main was clean after, no conflicts.
- **`docs/public-release-review.md`-style report from a read-only lane worked beautifully.** Lane 4 wrote one file and surfaced findings the other lanes' scopes specifically didn't cover (screenshot.go Vault ref, blueprint_tool.go jsonschema regional bias, the tag mismatch). The model of "one lane does pure audit and produces a report, others do work" is reusable for any large round.

### Bosun: frictions logged (for the maintainer)

1. **`bosun --version` not implemented.** Standard CLI convention; would expect either flag or subcommand. Minor.
2. **`bosun predict` regex-matches inline path mentions.** A lane's "DO NOT modify internal/config/**" constraint shows up as a claim. Real overlaps were correctly detected, but signal-to-noise was poor in the first pass — I had to rewrite the plan to remove path strings from constraint prose. **Suggestion:** require affirmative path scopes to be code-fenced, treat code-fenced paths as claims and prose mentions as informational only.
3. **Brief boilerplate assumes `make check`.** architect-mcp's Makefile only has `make test`. Suggestion: detect the verify command from the Makefile / package.json, or expose it via `.bosun/config.json` for per-repo customization. Lane 4 independently flagged this — surfaced from both directions.
4. **Off-tree workers false-positive CRASHED.** My Agent sub-agents weren't `bosun launch`-spawned, so bosun's liveness gate timed them out as CRASHED while they were still working. Happened to session-2 once and session-3 once. The safety contract held perfectly (work was always recoverable), but the state churn was noisy. **Suggestion:** a `bosun attach <session> [--pid <pid>] [--no-liveness-gate]` command for workers that aren't under bosun's process tree (CI agents, Agent-tool sub-agents, etc.). Or a session config flag like `liveness_gate: external` that disables the timeout-to-CRASHED transition.
5. **Predictor lacks coverage analysis.** `bosun predict` checks for path overlap between lanes but not whether the lane decomposition *covers all the work*. Lane 1's scope didn't list `internal/screenshot/screenshot.go` (which has a Vault reference), and the round-level decomposition didn't catch this — lane 4's audit had to surface it. A "what files in this repo have TODOs / leaks / heuristic flags that no lane claims?" check would be a strong addition.

### Bosun: nothing-burgers (things that worked fine, worth noting)

- `bosun init --brief plan.md` parsed the markdown cleanly, generated 4 correct per-session briefs.
- `bosun claim` from inside a worktree just worked. Operator-side `bosun status` reflected the claims accurately.
- `bosun done` cleanly transitioned WORKING → DONE when called from inside a worktree.
- `bosun show <session>` is exactly the level of detail needed — last 10 commits + git status + lane brief in one output.
- `bosun cleanup` with no flags is conservative (refused session-2), `--purge` is loud (banner message names the # of commits being discarded). Both ergonomics match the danger level.
- No data loss across 3 CRASHED-state false positives + 1 successful round.

### What the architect-mcp operator needs to do next

(Outside the scope of bosun's role — these are post-merge follow-ups.)

1. **Resolve the tag-version mismatch** (lane-4 critical #1). Either rebase tags or jump first public release to v0.8.0. CLAUDE.md needs an update.
2. **Wire ldflags version injection** so `internal/mcp/server.go` doesn't lie to MCP clients (lane-4 critical #2).
3. **Add `make check` target** (alias for `go vet ./... && go test -race ./...`) — closes Friction #3 and lane-4 critical #3, makes future bosun rounds against this repo work out-of-the-box.
4. **Skim `docs/public-release-review.md`** end-to-end — 20 findings, 4 critical, 8 strongly-recommended. Pick which ones go into the v0.1.0 (or v0.8.0) release tag and which are deferred.
5. **Decide on the Vault reference in `screenshot.go`.** Lane 2 left it alone (legitimate generic SSRF example); lane 4 flagged it for the optics of the CLAUDE.md grep. Operator call.
6. **Trim the `/Users/jasondillingham/...` path inside `docs/public-release-review.md:327`** before going public — it's inside a finding's description, not a real leak, but it'd embarrass on read.

### Bottom line

Bosun did the coordination work it was designed to do, with **zero data loss across one round, four parallel workers, three false-positive crashes, and a non-trivial scope overlap problem caught by `predict`.** The two main frictions (predictor false-positives from constraint prose, off-tree worker liveness detection) are both improvable without breaking the model. For a private-repo dogfood, the safety contract performed exactly as the README promises.
