# Lane L7 — real-dogfood UX

**Lane:** L7-dogfood (bughunt-1)
**Operator:** L7
**Date:** 2026-05-28
**Baseline:** bosun HEAD `aabaf3d`, binary `/tmp/bosun_test` reports `v0.11.2-0.20260522171635-aabaf3d7bd25`
**Sandbox:** `/tmp/bosun-redteam-L7/test-repo` — toy multi-package Go project (cmd/myapp, internal/handler, internal/store)
**Runlog:** `/tmp/bosun-redteam/runlog/run-2026-05-28-L7-dogfood.md`
**Lane script:** `/tmp/bosun-redteam/harness/lanes/L7-dogfood.sh` (+ harness wrapper `/tmp/L7-run-with-rt.sh`)

> Severity scale matches the project rollup: CRITICAL / HIGH / MEDIUM / LOW.
> L7's mandate was *qualitative dogfooding* — closest lane to "is bosun pleasant to use?" — so most findings are workflow/UX shape, with two genuine HIGHs surfaced by walking the operator path that any real Claude Code session would take.

## Rollup

| ID  | Severity | Title | Status |
|-----|----------|-------|--------|
| F030 | LOW    | `bosun init` on an already-init'd repo writes a NEW `init.state` and surfaces git's "already used by worktree" error instead of detecting the existing sessions; subsequent inits then refuse with "previous init didn't finish" until operator hand-removes the file | confirmed |
| F031 | MEDIUM | `bosun attach` claims to register PIDs the liveness gate trusts; `bosun_spawn`'s liveness gate ignores the attached-pid file. After the documented escape hatch, spawn still refuses | confirmed |
| F032 | **HIGH** | `bosun merge` returns exit 0 even when the working tree is left in an unresolved "both modified" conflict state — CI scripts get a green status on a wedged repo, and `git merge --abort` doesn't work (no MERGE_HEAD from squash) | confirmed |
| F033 | LOW    | `bosun merge`'s conflict-stop message tells operators how to resolve+continue, not how to abandon — the recovery dance (`git restore --staged && git checkout --`) is non-obvious | confirmed |
| F034 | LOW    | `bosun remove --help` has no long description — single line for a destructive op (compare `bosun cleanup --help`'s 5 paragraphs) | confirmed |
| F035 | LOW    | `bosun cleanup` with `removed 0, skipped N` exits 0 — scripted teardown can't distinguish "cleaned everything" from "cleaned nothing because all sessions have work" | confirmed |
| F036 | LOW    | `bosun show <unknown>` says "not found (use `bosun list` to see active sessions)"; `bosun done <unknown>` says only "not found" — error UX inconsistent across commands sharing the same shape | confirmed |
| F037 | MEDIUM | `bosun debug` reports `version` as literally `"dev"` even when `bosun --version` correctly reports `v0.11.2-…` — every triage bundle is mis-labeled | confirmed |
| F038 | **HIGH** | `bosun init session-<word>` (e.g. `session-clean`) creates a fully-functional worktree + branch + spawn-tree entry, but `bosun list`/`status`/`show`/`remove` then treat it as nonexistent. Doctor reports "all checks passed." Silent orphan worktrees | confirmed |
| F039 | LOW    | After successful `bosun merge`, the source session can transition to CRASHED (stale attached-PID after the operator's transient login session exits); `bosun cleanup` then says "skipped — 1 ahead" because squash merge gave main a new SHA — `git cherry` would mark the commit equivalent but cleanup doesn't check | confirmed |

---

## F030 — `bosun init` on already-init'd repo: misleading error + leftover init.state (LOW)

**Files:**
- `cmd/bosun/cmd_init.go` (init pipeline entry point — wherever `init.state` is written and the existing-session preflight should live)
- `internal/session/session.go:186-189` (`Derive` already knows how to enumerate existing sessions; init isn't checking)

**Observed.** Fresh repo, `bosun init` → 4 sessions created successfully. A second `bosun init` (no args) re-enters the init pipeline, writes `.bosun/init.state` with a NEW round timestamp (`20260528-194443-67741`), then fails on the first `git worktree add` because branch `bosun/session-1` already exists. The error surfaced to the operator is git's internal:

```
$ bosun init
Creating worktree session-1 (1/4)...
Error: bosun: add worktree /private/tmp/.../test-repo-bosun-20260528-194443-67741-1:
       git worktree add /private/tmp/.../test-repo-bosun-20260528-194443-67741-1 bosun/session-1:
       exit status 128: Preparing worktree (checking out 'bosun/session-1')
fatal: 'bosun/session-1' is already used by worktree at '/private/tmp/.../test-repo-bosun-20260528-194443-67706-1'
```

No hint that the original 4 sessions are still intact. The leftover init.state then poisons every future bosun init:

```
$ bosun init
Error: bosun: previous bosun init didn't finish (see .bosun/init.state).
  run `bosun init --resume` to continue, or `rm .bosun/init.state` to start fresh
```

`--resume` would attempt to resume the *failed* round (creating a 5th session set with the doomed timestamp), and `rm` is the right answer — but neither message tells the operator that `bosun list` already shows the original 4 sessions are healthy.

**Why LOW.** No data loss; the original sessions survive. But operator-facing flow ("I forgot if I init'd; I'll just run it again") leaks git internals and leaves the state-machine in a degenerate "previous init didn't finish" loop until the operator hand-edits `.bosun/`. For a tool whose pitch is "low-friction session orchestration," this is a stumble on step 1.

**Fix shape.** At the top of `cmd_init`, before writing init.state, enumerate existing bosun-managed worktrees (the regex from `session.Derive`). If any match the labels the user asked for (or the default count is already met), refuse with:

```
Error: bosun: 4 session(s) already initialized — run `bosun list` to see them,
       `bosun init --force` to recreate, or `bosun init <N>` with a higher count.
```

And — independently — don't write `.bosun/init.state` until the init pipeline has passed its preflight checks. A failure in the first git worktree add shouldn't leave the operator in "previous init didn't finish" purgatory.

**Discovered.** 2026-05-28, the very first thing L7 tried on a fresh sandbox after `bosun init` succeeded.

---

## F031 — `bosun attach` advertises the escape hatch, `bosun_spawn` ignores it (MEDIUM)

**Files:**
- `internal/mcp/tool_attach.go:23` — docstring: *"Write `.bosun/state/<session>.attached-pid` so the liveness gate recognizes external workers that the proc-scan can't see."*
- `internal/mcp/server.go:171-179` — `defaultRunningFn` calls `proc.Running(worktreePath)` (process-table scan only)
- `internal/proc/detect.go:127-147` — `RunningWith` walks the process list, matches by name+cwd. **Does not consult the attached-pid file.**
- `internal/mcp/tool_spawn.go:137` — `s.runningFn(worktreePath)` is the only check; falls into the same proc-only path
- `internal/session/session.go:300-340` — `session.Derive` (the source of `bosun status`'s RUNNING column) has the full attach-then-proc-scan ladder. Spawn diverged.

**Observed.** Sandbox at `/tmp/bosun-redteam-L7/test-repo`, fresh `bosun init`, `agent_spawn.enabled=true` in config. From the session-1 worktree:

```bash
$ cd /tmp/bosun-redteam-L7/test-repo-bosun-…-1
$ /tmp/bosun_test attach session-1 --pid $$
bosun: session-1 attached pid=68661 (liveness gate now trusts this PID)
$ cat /tmp/bosun-redteam-L7/test-repo/.bosun/state/session-1.attached-pid
68661

$ BOSUN_MCP_SOCK=…/test-repo/.bosun/mcp.sock python3 mcp_sock.py call bosun_spawn \
    '{"parent":"session-1","brief":"## httph\n\nadd http handler\n","launch":false}'
{"text":"no live agent detected in session-1's worktree;
         bosun_spawn requires the caller to be running inside the named parent"}
```

The attached-pid file is right there. `attach`'s docstring claims it satisfies "the liveness gate." But spawn's liveness gate (`runningFn → proc.Running`) only walks the process table — it never reads `.bosun/state/<session>.attached-pid`.

The same gap covers `bosun_subtask` (`tool_subtask.go:113`) and `bosun_check_tree` (`tool_check_tree.go:77, 157`) — all three tools call `s.runningFn` and inherit the gap.

**Why MEDIUM (and combined with L3's F009/F010, near-HIGH).**

- This is the **third independent reason `bosun_spawn` refuses** on the default install, after L3's F009 (wrong worktree-path shape) and F010 (ignores `cfg.AgentCommand`). An operator who has hit and worked around F009/F010 (or runs on a config where neither applies) STILL can't satisfy the gate via the documented `bosun attach` path.
- The fix is contained: `defaultRunningFn` should first call `s.state.ReadAttachedPID(label)` and `os.FindProcess(pid).Signal(syscall.Signal(0))` — same ladder `session.Derive` already runs at line ~300.
- For a feature whose pitch is "low-friction sub-session orchestration," requiring the operator to spawn a real `claude` binary in the worktree (to be seen by the process scan) is an enormous setup tax. The escape hatch exists; it's just not wired.

**Repro path also surfaces in audit.** Every refused call lands in `.bosun/audit/spawn.log`:

```
$ bosun audit
2026-05-28T19:46:49   spawn   refused   session-1   gate=parent-liveness   no live agent detected ...
2026-05-28T19:47:10   spawn   refused   session-1   gate=parent-liveness   no live agent detected ...
```

— a nice signal that the audit log is operator-debuggable. The gate is just looking in the wrong place.

**Discovered.** 2026-05-28 during the "act like a Claude Code session" workflow probe — `bosun_spawn` was the immediate first call I'd want to make from a parent session, and even after following the attach docstring's recipe it refused.

---

## F032 — `bosun merge` returns exit 0 on conflict; CI scripts pass on wedged repo (HIGH)

**Files:**
- `cmd/bosun/cmd_merge.go` (likely exit-code path)
- `internal/merge/` (where `--squash` is invoked and conflict is detected)

**Observed.**

```
$ bosun merge session-2
system load is 8.29; merge may be slow (--no-load-check to skip)
  ✗ session-2: conflict — merge conflict — resolve manually then commit

bosun: stopped at first conflict. Resolve, commit, then re-run `bosun merge`.

$ echo $?
0

$ git status
On branch main
Unmerged paths:
	both modified:   internal/handler/handler.go
```

The repo is in a half-applied merge state — the file has `<<<<<<<`/`=======`/`>>>>>>>` markers — but the bosun process exits cleanly. A scripted pipeline like:

```bash
bosun merge "$@" && bosun cleanup && git push
```

continues marching: `bosun cleanup` sees the session is still ahead and skips (also exit 0 — see F035), `git push` pushes nothing new (the conflict isn't committed), and the operator's CI is green. The repo on disk is wedged.

Worse, the standard git escape hatch doesn't apply:

```
$ git merge --abort
fatal: There is no merge to abort (MERGE_HEAD missing).
```

`--squash` doesn't write MERGE_HEAD. To recover, the operator must `git restore --staged <file> && git checkout -- <file> && rm .git/MERGE_MSG .git/AUTO_MERGE`. The bosun message doesn't mention any of this.

**Why HIGH.** This is exactly the "workflow path silently loses commits / data" shape the L7 brief promotes to HIGH. The data isn't lost (the source branches still exist), but the operator's *belief* about the repo state is wrong — and any CI that runs `bosun merge` in `set -e` or relies on `$?` to gate next steps will declare success on a broken merge.

**Fix shape.**

1. `bosun merge` should exit nonzero (suggest `exit 4` — keeps `0=clean, 2=usage, 3=internal`, frees 4 for "conflict, manual resolution required") when ANY session refused due to conflict.
2. The conflict-stop message should include the recovery recipe and an explicit "to abandon, run `git restore --staged && git checkout --` (squash merges don't set MERGE_HEAD)."
3. Optional but high-value: a `bosun merge --abort` subcommand that wraps the recipe, so operators don't have to drop to git.

**Preempting the obvious counter.** If the design intent for exit-0 was "partial progress is OK for `bosun merge && next-step` pipelines" (the friendly "come back and re-run" message hints at this), the bug still stands: the working tree is wedged regardless of whether other sessions merged. Minimum fix: exit nonzero when ZERO sessions merged AND a conflict halted the rest. Stronger fix: always exit nonzero on any conflict — operators who want best-effort behavior can wrap with `bosun merge || true`.

**Discovered.** 2026-05-28 during the conflict scenario probe — set up two sessions both touching `internal/handler/handler.go`, mark both DONE, run `bosun merge`. The exit-0 was unexpected enough that I re-ran without piping to confirm.

---

## F033 — Conflict message doesn't tell operator how to abort (LOW)

**Files:**
- `internal/merge/` — wherever the "stopped at first conflict" string is emitted

**Observed.** The current message reads:

```
✗ session-2: conflict — merge conflict — resolve manually then commit

bosun: stopped at first conflict. Resolve, commit, then re-run `bosun merge`.
```

Many operators in this situation would prefer to abandon — the conflict wasn't expected; they want to look at the brief, think about it, maybe pair, and come back. `git merge --abort` fails (no MERGE_HEAD from squash). The recovery dance is non-obvious.

**Why LOW.** Pure documentation/messaging quality. No state corruption. But this compounds F032: an operator who reads the message and decides to abandon doesn't know how.

**Fix shape.** Extend the message:

```
bosun: stopped at first conflict in <file>.
       To resolve:  fix the conflict, `git add <file>`, `git commit`,
                    then re-run `bosun merge`.
       To abandon:  `git restore --staged <file> && git checkout -- <file>`
                    (squash merges don't set MERGE_HEAD, so `git merge --abort` won't work).
```

Or — pair with F032 — add `bosun merge --abort` and have the message recommend it.

**Discovered.** 2026-05-28, same probe as F032.

---

## F034 — `bosun remove --help` is one line for a destructive op (LOW)

**Files:**
- `cmd/bosun/cmd_remove.go` — `cobra.Command{Short: "..."}` is set; the longer description (`Long: ...`) appears to be missing.

**Observed.**

```
$ bosun remove --help
Tear down a session's worktree + branch

Usage:
  bosun remove <session> [flags]

Flags:
      --force            remove even if dirty or unmerged
  -h, --help             help for remove
      --ignore-running   bypass the live-agent safety gate (discards uncommitted work the agent is editing)
```

Compare `bosun cleanup --help` (5 paragraphs explaining `--force`, `--orphan-dirs`, `--orphans=N`, `--purge`, `--tree` semantics and when each applies). `bosun remove` is destructive (deletes worktree dir + branch + state files) and takes flags that override safety gates, but the operator gets one sentence.

**Why LOW.** Pure docs. No behavior change needed.

**Fix shape.** Add a `Long:` that explains:

- What dirty/unmerged state plain `bosun remove` refuses (and points at the exact error messages).
- That `--force` overrides "dirty or unmerged" but not the "session in spawn tree with children" check (if that's still a gate).
- That `--ignore-running` is for crashed-agent / abandoned-tmux recovery, not normal use.
- That `bosun remove` is irreversible — point at `bosun history` for a record of what was removed if the operator needs to reconstruct.

**Discovered.** 2026-05-28 while writing up cleanup-vs-remove ergonomics for the workflow narrative.

---

## F035 — `bosun cleanup` exit 0 when 0 acted on (LOW)

**Files:**
- `cmd/bosun/cmd_cleanup.go` — wherever the post-loop summary + exit path lives

**Observed.**

```
$ bosun cleanup
  ⏭ session-2: skipped — would discard 1 ahead — run `bosun merge session-2` first, or pass --purge to drop it

bosun: removed 0, skipped 1
$ echo $?
0
```

Exit 0 here is the same as `removed 4, skipped 0` (the "ideal teardown"). A CI pipeline like `bosun merge && bosun cleanup` can't tell whether cleanup did its job. Operators who script "after-merge sweep" workflows have to grep stdout for "removed 0" — which is fragile.

**Why LOW.** Cosmetic for interactive use. Becomes more important as bosun moves into CI/CD (which is one of its pitches — "treat sessions like CI jobs"). The fix is small.

**Fix shape.** Two options:

- Always-strict: if `removed==0` and `skipped>0`, exit nonzero (say `exit 5` — "cleanup deferred all work"). Document.
- Opt-in: leave the default at 0; add `--strict` that flips the behavior. Lower friction, but operators who want it have to know to ask.

I'd ship the opt-in.

**Discovered.** 2026-05-28 while watching the cleanup output in the workflow narrative.

---

## F036 — `bosun done <unknown>` missing the `bosun list` hint (LOW)

**Files:**
- `cmd/bosun/cmd_done.go` — wherever the "not found" path emits

**Observed.**

```
$ bosun show nonexistent
Error: bosun: nonexistent not found (use `bosun list` to see active sessions)

$ bosun done nonexistent
Error: bosun: nonexistent not found
```

Same shape (unknown session label), different ergonomic outcome. The operator who fat-fingers a label gets handheld by one command and stonewalled by the other.

Likely affects `bosun remove`, `bosun rescue`, and possibly `bosun attach` similarly — quick `grep -rn 'not found' cmd/bosun/` would catch them all.

**Why LOW.** Trivial Edit. Adds polish.

**Fix shape.** Centralize the "not found" error in a helper:

```go
func notFoundErr(label string) error {
    return fmt.Errorf("bosun: %s not found (use `bosun list` to see active sessions)", label)
}
```

Use everywhere a label lookup fails.

**Discovered.** 2026-05-28 during error-message-audit probes.

---

## F037 — `bosun debug` reports `version="dev"` regardless of binary version (MEDIUM)

**Files:**
- `cmd/bosun/cmd_debug.go:125-127` — `writeSection(bw, "bosun --version", func(w io.Writer) { fmt.Fprintf(w, "%s\n", version) })`
- `cmd/bosun/root.go:13-42` — `var version = "dev"` + `resolvedVersion()` (the helper that reads `runtime/debug.ReadBuildInfo()` for `go install`-style builds)
- `cmd/bosun/root.go:34` — `if version != "dev" { return version }` — the gated path

**Observed.**

```
$ /tmp/bosun_test --version
bosun version v0.11.2-0.20260522171635-aabaf3d7bd25

$ /tmp/bosun_test debug | head -12
============================================================
 BOSUN DEBUG REPORT — 2026-05-28 19:52:16 UTC
============================================================
repo: /private/tmp/bosun-redteam-L7/test-repo
redaction: ON (re-run with --no-redact to disable)

============================================================
 bosun --version
============================================================
dev
```

The binary's `--version` correctly uses `resolvedVersion()`, falls through to `runtime/debug.ReadBuildInfo()` for go-installed builds, and reports `v0.11.2-0.20260522171635-aabaf3d7bd25`. But `cmd_debug.go:126` calls the *raw* `version` variable — which is only set via `-ldflags "-X main.version=..."` (release builds) and otherwise stays `"dev"`.

Every operator who got bosun via the README-recommended path (`go install github.com/.../bosun/cmd/bosun@latest`) generates debug bundles labeled `dev`. Maintainers triaging an issue can't tell which version produced the report.

**Why MEDIUM.** `bosun debug`'s explicit purpose is "gather everything a maintainer needs to triage a bug report into a single plain-text bundle." Mis-labeling the version defeats half the bundle's value — every other section (doctor, git state, config, audit) is anchored on "what version of bosun emitted this."

Same shape as **F001** (L1's "stale ServerVersion constant"): bosun has two version sources (raw `version`, and the runtime-aware `resolvedVersion()`); the wire MCP server uses one constant, the debug bundle uses the raw var, only `bosun --version` uses the resolved helper. Pinning all three through a single helper would close the class.

**Fix shape.** Two-line change at `cmd_debug.go:126`:

```diff
-       _, _ = fmt.Fprintf(w, "%s\n", version)
+       _, _ = fmt.Fprintf(w, "%s\n", resolvedVersion())
```

Add a regression test (`cmd/bosun/cmd_debug_test.go`) that asserts the debug bundle's `bosun --version` section matches what `bosun --version` itself prints.

Independently, a unified version path (used by F001's MCP `ServerVersion`, the debug bundle, and `bosun --version`) would prevent the next reviewer from finding the same shape in a fourth location.

**Discovered.** 2026-05-28 while running `bosun debug` to inspect the bundle format during the workflow narrative — the `dev` next to the date header was conspicuous.

---

## F038 — `bosun init session-<word>` silently orphans the session (HIGH)

**Files:**
- `internal/session/session.go:444` — `labelRe = regexp.MustCompile(...)` — accepts `session-clean` (it's `[a-z][a-z0-9]*(-[a-z0-9]+)*...`)
- `internal/session/session.go:455-459` — `ValidateLabel("session-clean")` returns nil
- `internal/session/session.go:232-240` — `Derive`'s exclusion block:
  ```go
  if rest, ok := strings.CutPrefix(label, "session-"); ok && !strings.Contains(rest, ".") {
      if n, err := strconv.Atoi(rest); err == nil && n >= 1 {
          number = n
      } else {
          // "session-foo" or "session-0" — not a bosun-managed branch.
          continue
      }
  }
  ```
- `cmd/bosun/cmd_init.go` — no gate matching the Derive exclusion

**Observed.**

```
$ bosun init session-clean
system load is 10.23; init may be slow (--no-load-check to skip)
Creating worktree session-clean (1/1)...
Created 1 session(s):
  session-clean → /private/tmp/.../test-repo-bosun-…-clean  (branch: bosun/session-clean)

$ bosun list
session-2                  # ← only the *other* existing session
                           # ← session-clean is INVISIBLE

$ bosun status
1 session — 1 DONE · 1 commit ahead total
SESSION    BRANCH           STATE  AHEAD  DIRTY  ...
session-2  bosun/session-2  DONE   1      0      ...

$ bosun show session-clean
Error: bosun: session-clean not found (use `bosun list` to see active sessions)

$ git worktree list
/private/tmp/.../test-repo                                    91fed64 [main]
/private/tmp/.../test-repo-bosun-…-2                          83e945b [bosun/session-2]
/private/tmp/.../test-repo-bosun-…-clean                      91fed64 [bosun/session-clean]   # ← it's right there

$ cat .bosun/spawn-tree.json
{
  "version": "v1",
  "sessions": {
    "session-2": { "depth": 0, ... },
    "session-clean": { "depth": 0, ... }       # ← bosun's OWN ledger knows
  }
}

$ bosun doctor
... ✓ orphan-worktrees: no orphan worktree directories
All checks passed.                              # ← doctor doesn't notice

$ bosun remove session-clean
Error: bosun: delete branch bosun/session-clean:
       git branch -D bosun/session-clean: exit status 1:
       error: cannot delete branch 'bosun/session-clean' used by
       worktree at '/private/tmp/.../test-repo-bosun-…-clean'
```

`bosun init` accepted the label, created the worktree, created the branch, recorded the session in `spawn-tree.json`, and printed a green-checkmark success message. Every command that *reads* the session set (list, status, show, doctor) silently excludes it.

**Confirmed at the structural layer, not the renderer.** Sanity-checked with `bosun list --json`:

```json
{
  "version": "v0.4.0",
  "sessions": [
    { "name": "session-1", "branch": "bosun/session-1", "state": "CRASHED" },
    { "name": "session-2", "branch": "bosun/session-2", "state": "DONE"    },
    { "name": "session-3", "branch": "bosun/session-3", "state": "WORKING" },
    { "name": "session-4", "branch": "bosun/session-4", "state": "WORKING" }
  ]
}
```

`session-clean` is absent from the structured output too — bug is at `session.Derive` (line 232-240 exclusion), not the table renderer. Yet `cat .bosun/spawn-tree.json` shows bosun knew it created the session. Three sources-of-truth divergence: spawn-tree.json (knows), git worktree list (has it), Derive (excludes). `bosun remove` half-runs (the git branch delete fails because the worktree is still present), leaving the operator to drop to raw git (`git worktree remove …; git branch -D …; manual edit of spawn-tree.json`) to recover.

**Why HIGH.** The brief's HIGH threshold: *"workflow path silently loses commits / data; OR an operator-mode error that's catastrophic."* This is silent-state-divergence between bosun's writes (init) and bosun's reads (list/status/show). An operator running through the tour-equivalent flow with named sessions ("I want a session for the http handler — `bosun init session-http`") gets a clean success message, then nothing they do can find or manage the session. The fix is to align: either Derive should accept `session-<word>`, or init should reject. The error message at init time can point at the right shape:

```
Error: bosun: 'session-clean' is reserved — use a bare label like 'clean' or the numeric form 'session-1'.
       Examples:
         bosun init clean              # named session, branch=bosun/clean
         bosun init session-1          # numbered session
         bosun init clean storage      # two named sessions in one round
```

**Why this lurks.** L7 only hit it because I was acting like a real operator who'd say `bosun init session-clean` to make a "clean test workspace" — the label felt natural. The Derive exclusion's comment (*"session-foo / session-0 — not a bosun-managed branch"*) suggests someone knew about the class but only fixed half the path. Same fix-the-class shape as L3's F009/F010 (where the spawn-vs-status divergence was the bug).

**Fix shape.** Cheapest: add a gate to `ValidateLabel` (or to a new `ValidateInitLabel` if other call sites need the looser charset):

```go
if rest, ok := strings.CutPrefix(s, "session-"); ok {
    if n, err := strconv.Atoi(rest); err != nil || n < 1 {
        return fmt.Errorf("invalid session label %q (the 'session-' prefix is reserved for numbered sessions like session-1; for named sessions, drop the prefix — e.g. 'clean' instead of 'session-clean')", s)
    }
}
```

Add the rejection at the init pipeline entry point so the cmd_init flow refuses before any git worktree is created. Add a regression test (`cmd/bosun/cmd_init_test.go`) that asserts `bosun init session-clean` exits nonzero with a useful message.

Independently, a `bosun doctor` check would catch existing orphans for users already in the broken state:

```
  ✗ session-naming: 1 worktree(s) on disk match the 'session-<word>' shape that bosun cannot manage:
      bosun/session-clean → /private/tmp/.../test-repo-bosun-…-clean
    To recover: `git worktree remove <path> && git branch -D <branch>`,
                then re-init with the bare label (e.g. `bosun init clean`).
```

**Discovered.** 2026-05-28 during the "test what `bosun_done` does over MCP for a session with zero ahead commits" probe — I created a session-clean to have a known-clean state, then MCP returned "session-clean not found." The contradiction between init's success message and every subsequent command's "not found" was the smoking gun.

---

## F039 — DONE+merged session shows CRASHED, cleanup says "1 ahead" (LOW)

**Files:**
- `cmd/bosun/cmd_cleanup.go` — the "would discard N ahead" message lives here
- `internal/git/` — should learn `git cherry` (or equivalent) for squash-equivalence detection

**Observed.** Workflow:

1. `bosun attach session-1 --pid $$` from a transient login subshell.
2. Operator edits files, commits, `bosun_done`, `bosun merge session-1` → squash merge to main.
3. Subshell exits; the attached PID is now stale → `bosun status` flips session-1 to CRASHED.
4. `bosun cleanup` says:

```
⏭ session-1: skipped — 1 ahead
```

But the "1 ahead" commit IS already on main (via squash). `git cherry main bosun/session-1` would mark it `equivalent` (lowercase `-`). Cleanup just runs `rev-list --count` and refuses.

The operator-facing advice — "run `bosun merge session-1` first, or pass --purge to drop it" — is wrong in this case:
- `bosun merge session-1` would either be a no-op (the squashed content is already on main, so no diff) or create a duplicate squash commit (worse).
- `--purge` drops "commits not on base" — for a squash-equivalent commit, the operator would be losing nothing, but the message implies they would be.

**Why LOW.** No data loss. The session can be removed with `bosun remove session-1 --force`. But for users in the workflow where any non-trivial cleanup happens between merge and teardown, the message is misleading.

**Fix shape.** Before printing the "would discard N ahead" message, run `git cherry <base> <session-branch>`. If every commit is marked `-` (equivalent), treat the session as squash-merged: remove it, print `removed (squash-equivalent)`. Same message bosun TOUR uses on the happy path:

```
$ bosun tour
  ✓ session-1: removed (squash-merged)
  ✓ session-2: removed (squash-merged)
```

— exactly the message that should fire here.

**Discovered.** 2026-05-28 while running the operator workflow and noticing the tour-path message ("removed (squash-merged)") didn't match the cleanup-path message ("skipped — 1 ahead") for the same on-disk state. The divergence was visible because L7 ran them in different orders.

---

## Workflow narrative — what the L7 lane actually tried

For future readers / lanes / bug-hunt rounds, here's the path that surfaced the findings above — bosun's UX held up better than feared, with two genuine HIGH bugs falling out of two unrelated workflows.

1. **Fresh sandbox, `bosun init`.** Worked. Created 4 sessions in <3s. Output is clean.
2. **`bosun init` a second time** (operator forgot they'd init'd). **F030**. Misleading error, leftover state.
3. **`bosun --help` audit.** Help is organized by lifecycle stage (Setup / During a round / Finishing a round / Wiring + advanced). Reads cleanly. Stand-alone subcommand --help is mostly good; one stands out: **F034** (`remove --help`).
4. **`bosun status` / `bosun list` / `bosun show --json`.** All produce versioned JSON (`"version": "v0.4.0"`). Scriptable. Good.
5. **`bosun tour`** with `BOSUN_TOUR_AUTO=1`. **Excellent UX** — walks a 5-step round end-to-end on a throwaway sandbox; the output explicitly shows what each command does and the "removed (squash-merged)" cleanup message on the happy path. This is the single biggest "make bosun a game-changer" surface in the binary — first-time users should be told about it on `bosun init` success.
6. **`bosun_spawn` over MCP**, no config. Refuses with actionable "Operator: set agent_spawn.enabled=true in .bosun/config.json". Good.
7. **`bosun config set agent_spawn.enabled true`.** Worked. But spawn still refused.
8. **L3 already documented why** (F009/F010 — wrong worktree path resolution, wrong predicate). I tried the documented escape hatch (`bosun attach`) which **F031** — attach doesn't satisfy spawn's gate either.
9. **Pivoted to a real workflow**: edit files in session-1, `bosun_claim`, commit, `bosun_done` over MCP, `bosun merge`. All worked smoothly. The merge dry-run output is particularly nice.
10. **Conflict scenario**: two sessions touching the same file. `bosun merge` correctly reports the conflict. But **F032** — exit 0 with the working tree wedged. Promoted to HIGH; the brief's bar for HIGH is exactly this shape.
11. **`bosun debug` bundle inspection** (operator filing an issue). **F037** — version is "dev" in the bundle. Two-line fix.
12. **`bosun init session-clean`** to make a known-clean test session. **F038** — the second HIGH. Init succeeds, every read says doesn't exist.
13. **`bosun audit`** for the failed spawn attempts. Excellent — clean, filterable, says exactly why each call was refused. This is a high-value debugging surface; should be linked from the spawn error message.
14. **`bosun cost`, `bosun heartbeat`, `bosun events`.** Cost gracefully reports "no usage recorded" with a pointer to the `bosun_usage` MCP tool. Heartbeat works. Events would need `bosun serve` running; not pursued.
15. **`bosun tui` headless** correctly refuses with `exit 3` (good — distinguished from `exit 1/2`).

**What L7 deliberately didn't pursue.**

- The `bosun_subtask` tool surface (out of scope for the parent-spawn UX angle; the same liveness-gate gap covers it per F031).
- Cross-session race conditions on `bosun_claim` (L3's F016 area; L7 stayed at single-session for the workflow narrative).
- The `bosun serve` HTTP surface (L5 ground).

**What L7 would do next, given budget.**

- Probe `bosun new-brief --pattern recipe` → `bosun init --brief <file>` end-to-end to test the operator's "from goal to running sessions" path. Briefly looked: `new-brief --help` says it writes a starter brief with `{{placeholders}}`; full path not exercised.
- Test what happens when an operator runs `bosun init` from inside an existing session's worktree (recursive bosun-on-bosun) — likely either refuses cleanly or creates worktrees-of-worktrees in a confusing way.
- Wire `bosun serve` and inspect what the dashboard exposes for a real workflow (this is the closest surface to "make bosun visible to non-CLI-power-users").

---

## Closure record — non-findings worth noting

- **`bosun tour`** is genuinely good. First-time-user UX, in the binary, no docs to read. Recommend `bosun init` mention it on first success.
- **`bosun audit`** is the high-leverage debugging surface. Should be cross-linked from spawn/subtask refusal messages ("see why bosun refused this call: `bosun audit --tail 5 --session <label>`").
- **`bosun --version`, `bosun debug --include all`, `bosun doctor`** triad is the right shape for issue triage — modulo F037.
- **JSON output (`--json` on `list`, `show`, `status`, `cost`, `events`)** carries a `"version"` field and consistent structure. Scriptable.
- **Errors mostly exit nonzero** with distinct codes (1, 2, 3 distinguishable). The two failures of this pattern (F032 merge, F035 cleanup) are the two scripted-use bugs to fix.
