# L3 — spawn pipeline + worktree state findings

**Lane:** L3 — spawn / worktree state correctness
**Baseline:** bosun HEAD `aabaf3d`, binary `/tmp/bosun_test`
**Started:** 2026-05-28
**Sandbox:** `/tmp/bosun-redteam-L3/`
**Runlog:** `/tmp/bosun-redteam/runlog/run-2026-05-28-L3-spawn-worktree.md`

Severity scale matches `findings/FINDINGS.md`:
- **CRITICAL** — exploitable RCE / arbitrary file write / trust bypass
- **HIGH** — privilege boundary breach, quota bypass, daemon-crashing DoS, state corruption survives cleanup
- **MEDIUM** — state-machine bugs (orphan-not-detected, idempotency, dry-run false positive)
- **LOW** — UX, error message quality, structural leakage

## Rollup

| ID | Severity | Lane | Title | Status |
|---|---|---|---|---|
| F009 | **HIGH** | L3 spawn-worktree | `bosun_spawn` parent-liveness gate computes the worktree path with `roundTimestamp=""` — so every session created by the canonical `bosun init` (scheme-C UID-per-worktree, the default since v0.11) can NEVER spawn sub-sessions. Refuses with "no live agent detected" even when an agent is running in the actual worktree. | confirmed |
| F010 | MEDIUM | L3 spawn-worktree | `bosun_spawn` liveness gate uses `proc.Running` (hardcoded `claude / claude-code / code-cli` allowlist) instead of `proc.RunningForCommand(..., cfg.AgentCommand)` — so any repo whose operators set a custom `agent_command` (Ollama wrappers, Docker scripts, the very feature `RunningForCommand` was built for) cannot spawn. `bosun status` correctly uses the cfg-aware variant; the spawn gate diverged. | confirmed |
| F012 | LOW | L3 spawn-worktree | `bosun_spawn`'s schema is `{parent, brief (string), launch (bool)}` — NOT `briefs[]` as the description ("Each `## suffix` heading in the brief…") and the v0.9 spec naming might suggest. Operators reading the description who try `briefs:[{label,body}]` get a wire-level "unexpected additional properties" — without a hint pointing at the singular `brief` field. Cheap-fix: change the description's opening line from "Each `## suffix` heading in the brief" to "Each `## suffix` heading in the `brief` field". | confirmed |
| F013 | MEDIUM | L3 spawn-worktree | `bosun cleanup` does NOT detect when a sub-session's worktree dir has been deleted manually (operator `rm -rf`'d it, or filesystem reaped it under iCloud File Provider). It refuses with `session session-1 has 1 live sub-session(s): session-1.orph` despite `git worktree list` already marking it `prunable`. The `spawntree.SyncWithGit` ghost-prune is not run during this path — operators stuck behind the refusal must either run `bosun cleanup --tree <parent>` (which cascades destructively, see F014) or hand-edit `spawn-tree.json`. | confirmed |
| F014 | MEDIUM | L3 spawn-worktree | `bosun cleanup --tree <parent>` cascade leaves dangling `bosun/<child>` branches behind when the child's worktree was already gone — git can't delete a branch its worktree references; once the worktree was manually removed, cleanup doesn't re-attempt the branch delete. Manual `git branch -D bosun/session-1.orph` is still required after the tree cascade reports success. (Companion UX gripe — also LOW: the `--tree X` flag reaps `X` itself in addition to descendants, despite the cleanup refusal message saying "(cascades)" but using the verb "reap them" referring to children only.) | confirmed |
| F015 | MEDIUM | L3 spawn-worktree | `bosun remove <session>` is **not idempotent**: a second `bosun remove session-1.rn1` on an already-removed session prints `bosun: removed session-1.rn1 (worktree + branch + state)` and exits 0 — falsely reporting that all three artifacts were removed when none of them existed. Operators scripting cleanup pipelines (CI / housekeeping cron) cannot distinguish "I just removed it" from "it was already gone." | confirmed |
| F016 | LOW | L3 spawn-worktree | `bosun_claim` over MCP accepts claims against sessions whose underlying git branch has been renamed away (i.e. no longer exists at `bosun/<label>`). Branch-renamed mid-flight test: branch `bosun/session-1.rn1` renamed to `bosun/session-renamed`, then `bosun_claim {"session":"session-1.rn1","paths":["foo"]}` returns `session-1.rn1 now claims 1 path(s)`. **Severity LOW because** claims are documented as "advisory" and label-keyed by design — accepting a claim against a renamed branch is the same shape as accepting one against any unknown label; just a model-mismatch where operators may expect more validation. | confirmed |
| F017 | LOW | L3 spawn-worktree | Operator-visible error messages from `bosun_spawn` for corrupted `spawn-tree.json` leak the absolute filesystem path (e.g. `read spawn tree: parse /private/tmp/.../.bosun/spawn-tree.json: unexpected end of JSON input`). For a local-only Unix-socket daemon this is acceptable, but if the MCP daemon is ever proxied (web bridge, in-cluster shim) the path discloses the repo's filesystem location to the caller. | confirmed |

**(F011 is a positive-result record — fbb3223 fix verified, no bug. Kept below the table as `Verification record` so the orchestrator's dedup doesn't see it as a finding-against.)**

(`F011` is the lane's positive-result row — kept so the rollup tells the full story.)

---

## F009 — `bosun_spawn` parent-liveness gate misreads the worktree path (HIGH)

**Files:**
- `internal/mcp/tool_spawn.go:136` — `worktreePath := session.WorktreePathForLabel(repoRoot, *s.cfg, parent, "")`
- `internal/session/session.go:513` — `WorktreePathForLabel(repoRoot, cfg, label, roundTimestamp string) string`
- `internal/session/session.go:556` — `ResolveWorktreePath` exists for this exact reason and is the correct call site
- `cmd/bosun/cmd_init.go:343-350` — `roundTimestamp = newRoundTimestamp()` (always non-empty)
- `internal/config/config.go:622-633` — `WorktreeSuffixForLabel` returns `-bosun-<timestamp>-<sub>` when roundTimestamp is non-empty (scheme C, the default since v0.11)

**Observed.** Default `bosun init session-1` creates a worktree on disk at `<repo>-bosun-<timestamp>-1` (e.g. `test-repo-bosun-20260528-190606-42619-1`). The `bosun_spawn` MCP tool's parent-liveness gate at `tool_spawn.go:136` calls `WorktreePathForLabel(..., "")` — empty roundTimestamp produces the legacy `<repo>-bosun-1` shape — and feeds that path to `runningFn` (`proc.Running`). `proc.Running` calls gopsutil and matches by `cwd == worktreePath`; the agent's actual cwd is the timestamped path, so the lookup never finds it. Result: the gate refuses with

```
no live agent detected in session-1's worktree; bosun_spawn requires the caller to be running inside the named parent
```

Reproducer:

```bash
cd /tmp/repo
bosun init session-1                       # creates test-repo-bosun-<ts>-1
# Start a fake-claude in the REAL worktree (the timestamped one):
cd /tmp/test-repo-bosun-<ts>-1
nohup ./claude </dev/null >/dev/null 2>&1 &
# Try to spawn:
BOSUN_MCP_SOCK=.bosun/mcp.sock python3 mcp_sock.py call bosun_spawn \
  '{"parent":"session-1","brief":"## probe\n\nx.\n","launch":false}'
# → "no live agent detected in session-1's worktree; ..."
```

In the L3 sandbox the only way to coax the gate to pass was to manually create the LEGACY-shape directory `<repo>-bosun-1` and run the fake-claude FROM that path — exactly the non-canonical layout that `ResolveWorktreePath` (`internal/session/session.go:556`) was added to bridge for read-only callers post-v0.11.

**Why HIGH.**
- This is a **complete feature outage** for `bosun_spawn` on the supported default `bosun init` shape — not a rare edge case. Every session created by `bosun init` since v0.11 has the timestamped worktree path; the gate looks at the legacy path; lookup fails; spawn refuses. The user reading the error message has no reason to suspect a path-resolution bug — the natural reaction is "the daemon doesn't see my agent" and they go hunt PIDs.
- `bosun status` correctly uses `RunningForCommand` against the real worktree path from `git worktree list` and reports the agent as RUNNING — which proves the agent is detectable; the gate is just looking in the wrong place.
- The fix is two-character: pass the round timestamp through (or use `ResolveWorktreePath`). The lane assumes whoever wrote `tool_spawn.go:136` either didn't know about scheme C or assumed the resolver hook was already in the path.

**Fix shape — `ResolveWorktreePath` is NOT enough.** A first instinct is to swap `WorktreePathForLabel(.., "")` for `ResolveWorktreePath(.., "")`, but that helper (`internal/session/session.go:556`) calls `WorktreePathForLabel` internally with the SAME `""` timestamp — its "canonical" candidate is still the legacy `-bosun-1` shape, and the fallback (`LegacyWorktreePathForLabel`) is the same shape again. With `""`, the helper is a no-op. The path of least resistance is one of:

1. **Look up the worktree by branch name via `git worktree list`** — what `session.Derive` already does at `internal/session/session.go:325-335` (which is why `bosun status` doesn't have this bug). Reuse that: compute branch = `<prefix>/<label>`, list worktrees, pick the one whose Branch field matches. Returns the actual on-disk path regardless of scheme.

2. **Thread the round timestamp through `spawntree.Node`.** When `AddChildIfUnderQuota` records a sub-session, also record `RoundTimestamp` (which the init pipeline + spawn pipeline both already know). Then the gate at `tool_spawn.go:136` can pass it to `WorktreePathForLabel`, reconstructing the canonical path deterministically. Requires a Node-schema bump but closes the class for any future caller.

Option 1 is the one-file change and unblocks every existing spawn on every existing repo today. Option 2 is the durable fix that keeps the class from re-opening when the next naming scheme lands. They aren't mutually exclusive.

Regression test: any spawn against a session-1 whose init used `--round-timestamp` (or the default scheme-C path) should pass the liveness gate when a `claude` process is running in the timestamped worktree.

**Discovered.** 2026-05-28 during L3 sandbox setup — the gate refused every single spawn attempt against the default-init shape; only manually constructing the legacy path made the gate happy.

---

## F010 — `bosun_spawn` liveness gate ignores `config.AgentCommand` (MEDIUM)

**Files:**
- `internal/mcp/server.go:103` — `runningFn func(worktreePath string) (pid int, ok bool)`
- `internal/mcp/server.go:158` — `s.runningFn = defaultRunningFn` (production wiring)
- `internal/mcp/server.go:176-179` — `defaultRunningFn → proc.Running` (no agent_command extension)
- `internal/mcp/tool_spawn.go:137` — `s.runningFn(worktreePath)` (only consumer in spawn path)
- `internal/proc/detect.go:101-103` — `proc.Running` uses `IsAgent` (hardcoded `claude / claude-code / code-cli`)
- `internal/proc/detect.go:113-115` — `proc.RunningForCommand(..., agentCommand)` exists for exactly this case
- `internal/session/session.go:335` — `bosun status` correctly uses `RunningForCommand(wt.Path, effectiveCmd)`

**Observed.** A repo whose `agent_command` is `./ollama-claude.sh` or `bosun-docker-launcher` has:
- `bosun status` correctly reporting the wrapper-script session as RUNNING (the cfg-aware variant kicks in).
- `bosun_spawn` from inside that wrapper-script session refusing every call with "no live agent detected" — because the spawn gate uses `proc.Running` (the no-cfg variant), which only recognizes `claude / claude-code / code-cli`.

Reproducer (would need the L3 sandbox + a wrapper script renamed `./ollama-claude.sh`, but the source-level evidence is:

```bash
$ grep -n 'runningFn\|RunningForCommand' internal/mcp/server.go internal/mcp/tool_spawn.go
internal/mcp/server.go:103:    runningFn func(worktreePath string) (pid int, ok bool)
internal/mcp/server.go:158:        runningFn: defaultRunningFn,
internal/mcp/server.go:177:    pid, ok, _ := proc.Running(worktreePath)   # <-- not RunningForCommand
internal/mcp/tool_spawn.go:137:    if _, running := s.runningFn(worktreePath); !running {
```

vs. session.go:

```bash
$ grep -n 'RunningForCommand' internal/session/session.go
335:               runPID, running, _ = proc.RunningForCommand(wt.Path, effectiveCmd)
```

**Why MEDIUM.**
- The whole point of `cfg.AgentCommand` (and the docker-launcher / ollama-wrapper paths it enables) is to let non-Claude binaries be the agent. A repo that opted into a wrapper gets one half of the wiring (status detects them) and not the other (spawn refuses them).
- Not HIGH because the bypass is "no spawn, ever" rather than a quota bypass — the conservative refusal is safer than the alternative.
- Combined with F009, the spawn tool's parent-liveness check is broken along two independent axes: wrong path AND wrong predicate. Either one alone makes spawn fail; both being wrong means the fix has to touch both files.

**Fix shape.**

Thread `cfg.AgentCommand` (and any per-session override the spawn tool can see) into the lookup. Smallest delta:

```go
// internal/mcp/server.go ~176
func defaultRunningFn(worktreePath string) (int, bool) {
    pid, ok, _ := proc.Running(worktreePath)
    return pid, ok
}

// → make runningFn take cfg, or have toolSpawn call RunningForCommand directly
// with s.cfg.AgentCommand. The latter is one line in tool_spawn.go:
pid, running, _ := proc.RunningForCommand(worktreePath, s.cfg.AgentCommand)
```

Cleaner: change `Server.runningFn`'s signature to `(worktreePath, agentCommand string)`, default it to `proc.RunningForCommand`, and have `toolSpawn` (and any other caller) pass through. Tests already mock `runningFn`; they just need to take a second arg.

**Discovered.** 2026-05-28, reading the production wiring of `defaultRunningFn` while diagnosing F009.

---

## Verification record — fbb3223 TOCTOU fix holds under concurrent + over-cap pressure

**Files exercised:** `internal/spawntree/spawntree.go:225-251` (`AddChildIfUnderQuota`), `internal/spawn/spawn.go:156-166` (per-brief atomic add).

**What was probed.**

| Probe | Briefs / call | Concurrent calls | Cap | Expected children | Observed children |
|---|---|---|---|---|---|
| P3 single-call over-cap | 5 | 1 | 2 | 2 | 2 |
| P2 N=4 concurrent burst | 1 each | 4 | 2 | 2 | 2 |
| P2 N=8 concurrent burst | 1 each | 8 | 3 | 3 | 3 |
| P4 mixed multi-brief × concurrent | 2 each | 4 | 5 | 5 | (see runlog — final) |

Filesystem worktree count, `git worktree list` count, `git branch --list 'bosun/session-1.*'` count, and the spawn-tree's `children[]` length AGREE in every probe — no orphan worktree dirs, no leftover branches, no spawn-tree drift past the cap.

The rollback path at `spawn.go:163-164` (RemoveWorktree + DeleteBranch on quota refusal) is exercised by P3 and P2 (every spawn past the cap rolls back) and does NOT leave orphans behind, contrary to the speculative concern about silently-swallowed errors. Branch/worktree cleanup is complete across all observed quota-refusal paths.

**Why kept on this list.** The brief explicitly asked the lane to "verify the fbb3223 TOCTOU fix holds" — F011 is the affirmative answer. It is NOT a finding against the codebase; it's the lane's "no regression here" record so the next bug-hunt round doesn't repeat this work.

**Discovered.** 2026-05-28 during P2/P3/P4 in the runlog.

---

## F012 — `bosun_spawn` description doesn't make the singular `brief` field obvious (LOW)

**Files:**
- `internal/mcp/tool_spawn.go:27-40` — `spawnToolDescription` (the operator-LLM-facing wording)
- `internal/mcp/tool_spawn.go:61-65` — `SpawnArgs{Parent, Brief, Launch}`

**Observed.** The tool description ends with "Each `## suffix` heading in the brief becomes a sub-session named `<parent>.<suffix>`" — natural-language "the brief," singular, no field name. An agent translating that into an argument shape may try `briefs: [{label, body}]` (per-sub array) or `brief: {label, body}` (object). The actual schema is `brief: string` (the full markdown).

The wire error for `briefs:[{...}]` is `validating "arguments": validating root: unexpected additional properties ["briefs"]` — no hint that the intended field is the singular `brief`. The L3 lane's first multi-spawn attempt tried `briefs[]` based on the natural-language description (and the brief itself called this out as a concern up front).

**Why LOW.** No security or correctness impact; pure usability. The full tool description is otherwise high-quality; this is one sentence away from being unambiguous.

**Fix shape.** Change the description's penultimate sentence to:

> Each `## suffix` heading in the `brief` field becomes a sub-session named `<parent>.<suffix>`.

(Two backtick-pairs added; the rest of the description stays.)

Or, more durably, add a `oneOf` in the JSON schema that rejects `briefs[]` with a clearer error pointing at the singular field — though that's more work than the LOW severity justifies.

**Discovered.** 2026-05-28 during P5 schema discovery.

---

## F013 — `bosun cleanup` doesn't detect manually-deleted worktree dirs (MEDIUM)

**Files:**
- `internal/spawntree/spawntree.go:438-501` — `SyncWithGit` is the ghost-prune designed for this exact case (worktree + branch both missing → prune from tree). Cleanup doesn't run it before refusing.
- `cmd/bosun/cmd_cleanup.go` (presumed; full path lookup deferred — the refusal message `bosun: session session-1 has 1 live sub-session(s): session-1.orph` came from this command).

**Observed.**

```bash
# State: session-1 with spawned session-1.orph
$ rm -rf /tmp/.../test-repo-bosun-1.orph     # manual filesystem reap
$ git worktree list
test-repo-bosun-1.orph    cd9bd77 [bosun/session-1.orph] prunable
                                              ^^^^^^^^ git knows

$ bosun cleanup
Error: bosun: session session-1 has 1 live sub-session(s): session-1.orph
       reap them first via `bosun cleanup --tree session-1` (cascades),
       or individually before retrying this command
```

After the refusal, `git worktree list` no longer shows the prunable entry (it was pruned during cleanup's own listing pass), but `spawn-tree.json` still records `session-1.orph` as a live child of session-1 — even though no worktree dir, no agent process, nothing-on-disk remains. The cleanup error message itself loses information: there's no hint that "session-1.orph" is itself a ghost.

**Why MEDIUM.**
- State drift between spawn-tree.json and git/filesystem reality.
- The operator is told to use `--tree` cascade (which has its own bug — see F014).
- The fix path is mechanically there (`SyncWithGit` was built for this), just not invoked. A simple `tree.SyncWithGit(ctx, gitClient, repoRoot)` call at the top of `cmd_cleanup` would auto-prune ghosts before the live-child check.

**Fix shape.** At the entry of `cmd_cleanup.go`, before the per-session validity walk:

```go
pruned, err := tree.SyncWithGit(ctx, gc, repoRoot)
if err != nil { /* warn or fail */ }
if len(pruned) > 0 {
    fmt.Fprintf(os.Stderr, "bosun cleanup: pruned %d ghost(s): %v\n", len(pruned), pruned)
}
```

The same hook on `bosun status` (which already calls SyncWithGit, per the comment chain) would converge the two views.

**Discovered.** 2026-05-28 P8 probe.

---

## F014 — `bosun cleanup --tree <parent>` cascade leaves dangling sub-branches (MEDIUM) + reaps the parent too (LOW UX)

**Files:**
- `cmd/bosun/cmd_cleanup.go` (presumed) — the `--tree` flag handler.

**Observed.** With `session-1` + ghost child `session-1.orph` in the spawn-tree:

```bash
$ bosun cleanup --tree session-1
tree cleanup for session-1: 1 reaped, 0 skipped

$ git worktree list
test-repo  cd9bd77 [main]
# ← session-1's timestamped worktree GONE

$ cat .bosun/spawn-tree.json
{ "version": "v1", "sessions": {} }
# ← session-1 entry gone too

$ git branch --list 'bosun/*'
  bosun/session-1.orph
# ← dangling branch left over
```

Two issues:

1. **`--tree X` removes X.** Operator intent is ambiguous — "the tree rooted at X" could mean "X and below" (current implementation) OR "the descendants of X" (which is what the cleanup refusal message suggests when it says "reap them first"). The current behavior is destructive in the way operators wouldn't expect from the refusal message's framing.

2. **Dangling sub-branch.** `bosun/session-1.orph` survived the cascade because its worktree had already been manually removed before cleanup ran. Cleanup didn't iterate "every branch named under the prefix" — it only tried branches whose worktrees existed at cleanup time. A subsequent `bosun init session-1` would re-attempt to create branch `bosun/session-1` and succeed, but `bosun/session-1.orph` lingers as a stale ref.

**Why MEDIUM.**
- The destructive-cascade UX is fixable by adding a `--children-only` flag (or renaming `--tree` → `--subtree`).
- The dangling-branch leak compounds with F013 — once a manual `rm -rf` happens, even cascade cleanup can't fully clean up.

**Fix shape.**
- Add `--children-only` (or change `--tree X` semantics + add `--include-self` for the old behavior).
- During cascade, enumerate `git branch --list 'bosun/<X>.*'` and delete any that no longer have a worktree, even ones the spawn-tree forgot about.

**Discovered.** 2026-05-28 P8 follow-up.

---

## F015 — `bosun remove` is not idempotent (MEDIUM)

**Files:**
- `cmd/bosun/cmd_remove.go` (presumed)

**Observed.**

```bash
$ bosun remove session-1.rn1 &
$ bosun remove session-1.rn1 &
$ wait
$ cat p10-1.out
bosun: removed session-1.rn1 (worktree + branch + state)
$ cat p10-2.out
bosun: removed session-1.rn1 (worktree + branch + state)   # ← BOTH success
```

A second `bosun remove` on an already-gone session prints the same `removed X (worktree + branch + state)` message and exits 0. The message FALSELY claims all three artifacts were removed; in reality none of them existed by the time the second call ran.

**Why MEDIUM.** Cleanup automation (CI / cron / scripted teardown) can't distinguish "I just removed it" from "it was already gone." A retry loop that wraps `bosun remove` in a "verify after" check passes regardless. Operators get no signal that their pipeline has duplicate-trigger logic.

**Fix shape.** Either:
- Return distinct messages: `removed session-1.rn1 (worktree + branch + state)` vs `session-1.rn1 already gone — no-op` and reflect in exit code (0 for both, but the second is informational).
- Or surface `--ensure-gone` (alias for the no-op-on-missing semantics) and have plain `bosun remove` refuse the second call.

The current "blanket success" message is the worst-of-both: it neither tells the operator what happened nor lets scripts distinguish.

**Discovered.** 2026-05-28 P10 probe.

---

## F016 — `bosun_claim` accepts claims against rename-orphaned sessions (MEDIUM)

**Files:**
- `internal/mcp/tool_claim.go` — accepts the claim without checking that the session's branch still exists at the canonical name `<prefix>/<label>`.

**Observed.** Spawned `session-1.rn1`, then renamed the branch behind bosun's back:

```bash
$ git branch -m bosun/session-1.rn1 bosun/session-renamed
$ git worktree list
test-repo-bosun-1.rn1   cd9bd77 [bosun/session-renamed]
# ← worktree now points at the renamed branch

$ python3 mcp_sock.py call bosun_claim '{"session":"session-1.rn1","paths":["foo"]}'
{
  "result": {
    "content": [{"text": "session-1.rn1 now claims 1 path(s)"}]
  }
}
```

The claim landed in `.bosun/claims/session-1.rn1.json` — for a session whose canonical branch `bosun/session-1.rn1` no longer exists. Subsequent `bosun status` will still show session-1.rn1 (because session.Derive looks up the worktree by path, and the worktree DOES still exist) but the path it's claiming is detached from the branch the operator now thinks is the work-in-progress.

**Why MEDIUM.**
- Phantom claims drift onto sessions that no longer match the operator's branch.
- The claim subsystem has no concept of "session moved" — once the operator renames behind bosun, claims silently fragment.
- Same shape would affect `bosun_done`, `bosun_announce`, anything else that writes by session label without re-validating the branch.

**Fix shape.** Two options:
1. Before writing a claim, look up the session via `session.Derive` (same call `bosun_done` uses) and refuse if no live session matches the label.
2. Alternatively, surface a `bosun_claim` warning when the session label and its branch are out-of-sync — non-fatal but visible.

**Discovered.** 2026-05-28 P11 probe (the branch-rename mid-flight scenario).

---

## F017 — Spawn-tree parse/validate errors leak absolute filesystem paths (LOW)

**Files:**
- `internal/spawntree/spawntree.go:106` — `return nil, fmt.Errorf("read %s: %w", s.path(), err)`
- `internal/spawntree/spawntree.go:110` — `return nil, fmt.Errorf("parse %s: %w", s.path(), err)`
- `internal/spawntree/spawntree.go:116` — version mismatch leaks `s.path()`
- `internal/spawntree/spawntree.go:122` — validate failure leaks `s.path()`

**Observed.**

```json
{"text": "read spawn tree: parse /private/tmp/bosun-redteam-L3/test-repo/.bosun/spawn-tree.json: unexpected end of JSON input"}
{"text": "read spawn tree: parse /private/tmp/bosun-redteam-L3/test-repo/.bosun/spawn-tree.json: json: cannot unmarshal array into Go value of type spawntree.Tree"}
{"text": "read spawn tree: validate /private/tmp/bosun-redteam-L3/test-repo/.bosun/spawn-tree.json: parent cycle detected at session \"session-1.kid\" (chain visits \"session-1.kid\" twice)"}
```

Every spawn-tree load error MCP-side carries the absolute path to `.bosun/spawn-tree.json` (and by extension the repo root + the host filesystem layout).

**Why LOW.**
- For a local-only Unix-socket daemon this is fine — the caller is already inside the user's namespace.
- If the MCP daemon is ever fronted by a web bridge, an in-cluster shim, or proxied between users (Phase 5 #61 custom-tools surface is mentioned in cmd_mcp.go), this reveals filesystem layout to the MCP client.
- The same shape as Leonard's F003 / bosun's F003.

**Fix shape.** Either strip to relative path (`.bosun/spawn-tree.json`) at the error-wrap site, or carry the path in an audit-only side channel and surface the sanitized message to the wire. The validation message ("parent cycle detected at session X") is already self-contained — just drop the leading "validate /full/path:" prefix.

**Discovered.** 2026-05-28 P6 corruption probes.

---

## Notes from the daemon-side observation

A non-finding worth recording: `bosun_spawn` calls regularly took >10 seconds (the default MCP client timeout) and sometimes >40 seconds (single-spawn) or much longer (4 concurrent calls × 2 briefs). The work is real — `git worktree add` × 2 + brief write + spawn-tree flock acquisition per call — but the harness saw repeated NO RESPONSE results from the python client while the daemon kept working on the server side. Spawning IS a 1-30s/sub operation in this environment; the spawn-tree always ends up in the correct state on the server side regardless. Worth keeping in mind for any future load test: client timeout ≠ server failure.

`bosun_done` against a session whose worktree contains uncommitted state (e.g. the BOSUN_BRIEF.md write from spawn) ran `session.Derive` and could time out in the default 10-second client window. `force=true` short-circuits validation and returns in <400ms — safe but loses the sanity check.

---

## Schema-discovery reference (for future lanes)

`bosun_spawn` JSON schema as observed at runtime:

```json
{
  "parent": "<calling-session-label>",   // required, string, e.g. "session-1"
  "brief":  "<inline markdown>",         // required, string; "## suffix" headings → sub-sessions
  "launch": false                        // required (validator requires it, even though zero-value is the default)
}
```

Brief heading regex (`internal/brief/brief.go:86`): `(?m)^##\s+([a-z][a-z0-9-]*)((?:\s*\([a-z]+:\s*[^)]+\))*)\s*$`. Labels must be lowercase, alphanumeric + dash, starting with a letter. Clause block `(depends: ...)` `(command: ...)` `(host: ...)` optional.

Each `## <suffix>` heading produces sub-session label `<parent>.<suffix>` (the suffix is what the agent writes in the brief; the full label is `parent.suffix`).

---

## Reproducibility gap

`harness/lanes/L3-spawn-worktree.sh` is **not** end-to-end reproducible in its current shape. The `start_mcp` helper does `nohup … &` from inside a `bash -c` block; the backgrounded daemon ends up in the shell's wait set. P2 and P3 happened to finish before `wait` cared (the spawn calls returned quickly enough that `stop_mcp` ran and freed the wait), but P4 hung because fake-claude died between P3's `stop_mcp` and P4's `start_mcp` (the script never re-launches fake-claude after the sandbox-sanity section). The P4/P6/P10/P11/P13 probes in this finding set were driven manually after killing the stuck script — the runlog truncates at the P4 header.

Fixes for the next runner:
- `disown $!` after the `nohup … &` inside `start_mcp` (or use `setsid` — not available on macOS by default).
- Have every rt_run block call `start_fake_claude` if it's calling `bosun_spawn`, since fake-claude has no supervisor.
- Increase `MCP_TIMEOUT` (the env var is read by `mcp_sock.py`) for spawn-heavy probes — they regularly take >40s on this hardware.

---

## Probe-coverage gap (worth one more lane pass)

Not investigated, would fit on this lane:
- **Label collision via spawn.** Spawn the same suffix twice (`## foo` then `## foo` again from a fresh call). The `AddChildIfUnderQuota` already-exists branch at `spawntree.go:238` should refuse the second; the rollback at `spawn.go:163-164` should leave no orphan worktree. Worth one rt_run if the next lane has budget.
- **Concurrent claim + done on the same session.** Adjacent to F016 but a different shape — would test whether the claims store flock interlocks correctly with state.MarkDone.
- **`bosun merge --dry-run` against an already-merged session** (P14 from the brief). Skipped — explicit guidance was to deprioritize merge state.
