# UID worktree naming — migration + safety contract

**Lane:** session-3 (Migration + safety contract). Companion to the
naming-scheme work happening in sessions 1 and 2.

**Scope.** Narrative. No code lands from this doc. The deliverable is
a shared decision record that the implementation lanes and the
trial-#4 operator can both build on.

**Assumption about the scheme.** Sessions 1 + 2 are picking the
specific shape; this doc treats the UID portion as an opaque
`<uid>` substring that lengthens the worktree directory name and
the session label. Concretely, today's

```
<repo>-bosun-<session>          e.g. myproj-bosun-1, myproj-bosun-auth
```

becomes some variant of

```
<repo>-bosun-<uid>-<session>    e.g. myproj-bosun-a1b2c3-1
```

…or whichever ordering the naming lanes settle on. Every section
below is intentionally written against the abstract shape so the
specific scheme is not load-bearing.

---

## 1. Existing-user impact

**Who has worktrees on disk today.** Anyone who has run `bosun init`
since v0.1 has `<repo>-bosun-<session>` directories sitting beside
their repo. These directories carry real branches (`bosun/session-N`)
and may carry uncommitted work, DONE markers, claims, and rescue
snapshots. Treating them as disposable is a safety-contract break.

**Options considered:**

| Option | Behavior | Pros | Cons |
|---|---|---|---|
| A — Honor legacy names forever | The two naming shapes coexist; bosun looks up sessions by both globs forever. | No operator action ever required. Safest from a contract standpoint. | Doctor / orphan / phantom / cleanup code keeps two prefix shapes alive permanently. Drift inevitable. |
| B — One-shot migration on next `bosun init` | First `init` after upgrade detects legacy dirs, refuses to start until `bosun migrate` runs (or `--migrate` flag does the renames in-line). | Clean cutover. Single shape after migration. | Operator hit, but bounded. Need a real migrate command. |
| C — Hard cutover | New binary refuses to operate on any legacy worktree. Operator drains everything manually. | Simplest code. | Hostile to any operator with live sessions. Violates the spirit of the safety contract — bosun should not strand work. |

**Recommendation: B — one-shot migration on `bosun init`, with
read-only legacy compatibility everywhere else.**

Specifically:

- **`bosun status`, `bosun list`, `bosun show`, `bosun done`,
  `bosun claim`, `bosun rescue`, `bosun merge`, `bosun cleanup`,
  `bosun remove`** all continue to resolve legacy `<repo>-bosun-<session>`
  paths. These are the commands that operate on *existing* sessions
  and must keep working through the migration window. The session-
  lookup function (today the one inside `internal/session/session.go`
  Derive) gets taught both prefix shapes.
- **`bosun init` is the only command that refuses on legacy state.**
  When a fresh init would create *new* worktrees and any legacy-named
  sibling exists, bosun stops with:
  > `bosun: detected legacy-named worktrees (myproj-bosun-1, myproj-bosun-2).`
  > `Drain them via 'bosun merge && bosun cleanup', or run`
  > `'bosun migrate' to rename them in place.`
- **`bosun migrate`** (new, narrow command) walks legacy worktree
  dirs, runs `git worktree move <old> <new>` for each, and updates
  any pinned paths in `.bosun/spawn-tree.json` or `.bosun/init.state`.
  Branches are not renamed — they already live under `bosun/<session>`
  which is naming-scheme-independent.
- **`bosun doctor`** flags legacy worktrees as a Warn (not Fail) with
  fix text pointing at `bosun migrate`.

**Why this and not A.** Permanently honoring both shapes means the
orphan scanner, the phantom regex, the doctor checks, and the README
glob description all carry both shapes forever. That's exactly the
kind of drift that bites a future maintainer at the worst possible
time. Bosun's pre-1.0 adoption is small enough that a one-shot
migration is feasible, and a `migrate` subcommand keeps the operator
fully in control of when the rename happens.

**Why this and not C.** The safety contract says bosun does not strand
in-flight work. An operator with a CRASHED session and a rescue
snapshot they haven't extracted yet must be able to recover that
data through the upgrade. Hard cutover punishes the exact users the
safety contract is meant to protect.

---

## 2. `.bosun/init.state` and `--resume` compat

**What init.state stores today.** Per `internal/init/state.go`:

```go
type InitState struct {
    Version           string    // currently "v0.6"
    StartedAt         time.Time
    PlanPath          string
    TotalSessions     int
    Labels            []string  // e.g. ["session-1", "auth", "session-3"]
    CompletedSessions []string
    CurrentSession    string
    CurrentStep       string
}
```

**Key observation: init.state does not store worktree paths.** It
stores *labels*. The worktree path is derived from `(repoRoot, label)`
by whatever the naming function is at the time of derivation. That's
a stroke of architectural luck — the on-disk shape doesn't carry
the naming scheme in it.

**Implication for `--resume`.** When the new binary calls
`Load(repoRoot)` against an old-binary-written init.state and tries
to resume, the labels come back fine. But the *derivation* of the
worktree path for an already-completed label will now point at the
new-shape directory, while the actual directory on disk is the old
shape.

Concretely:

```
init.state CompletedSessions: ["session-1"]
old binary derived:           myproj-bosun-session-1          (on disk)
new binary derives:           myproj-bosun-a1b2c3-session-1   (NOT on disk)
new binary thinks session-1 was never completed → re-runs branch_create →
  collides with existing bosun/session-1 branch → init halts.
```

**Recommendation: refuse to resume across schema versions.**

- Bump `stateVersion` from `"v0.6"` to `"v0.10"` (or whatever lane
  ships the UID rename).
- `Load` returns a typed `ErrSchemaVersionMismatch` when the on-disk
  version is older than the binary's version.
- `bosun init --resume` translates that error to:
  > `bosun: .bosun/init.state was written by an older bosun (v0.6).`
  > `Resume across naming-scheme migrations is not supported.`
  > `Options:`
  > `  1. Run the older binary to finish or abort this init.`
  > `  2. Run 'bosun migrate', then 'rm .bosun/init.state' and re-init.`

**Why refuse-not-translate.** init.state exists for *seconds-to-
minutes-scale* resumability after a transient failure mid-init. It is
not a long-lived format and a stranded init.state from a previous
binary version is a rare-and-recoverable case. Writing translation
logic for it is more risk than reward — and the failure mode of
silent mis-resume (binary thinks it completed a session it actually
half-completed under the old shape) is worse than the loud refusal.

**What about `--resume` mid-init while *upgrading*?** Don't. Document
in the migrate command's help text that upgrading bosun while an init
is mid-flight is unsupported. The whole-system invariant the operator
needs to maintain is: "no init.state present" before swapping
binaries. Doctor should warn when both an older binary is on PATH and
a `.bosun/init.state` exists.

---

## 3. Safety contract messaging

The README's "Safety contract" section is load-bearing trust copy.
Per the root [`CLAUDE.md`](../CLAUDE.md) release stance: *"The safety
contract in the README is load-bearing trust. Don't weaken it without
surfacing the change explicitly and updating the README in the same
change."*

**The good news.** The phrase **`<repo>-bosun-*`** in the contract is
already a glob. It still matches the UID-extended shape. The contract
*spirit* doesn't change. What changes is the **example** rendering
and the **session-1 → directory** mapping in the demo block.

### Audit — README.md

- **Line 11–15 (the demo block).** Today: `../myproject-bosun-1`.
  After UID: `../myproject-bosun-<uid>-1` or whatever the lane
  settles on. Needs updating for accuracy; not contract-load-bearing.
- **Line 49 (safety contract bullet — what bosun creates without
  command).** Today: *"named `<repo>-bosun-<session>` (e.g.
  `myproj-bosun-1`)"*. This is the line that materially needs new
  wording.
- **Line 62 (never-bullet — write boundary).** Today: *"Writes
  outside `<repo>` and the `<repo>-bosun-*` sibling worktrees."*
  The glob still holds. **No change needed**, but worth re-stating in
  the trial as a contract-still-true checkpoint.
- **Line 163 (macOS iCloud warning).** Refers to phantom-duplicate
  files inside worktrees, not worktree names. No change.

### Audit — root CLAUDE.md

(Brief said `docs/CLAUDE.md`; the actual files are root `CLAUDE.md`
and `.claude/CLAUDE.md`. Treating both.)

- **Root `CLAUDE.md`** has no `<repo>-bosun-*` text. The release
  stance line about the safety contract is what governs — and it
  governs *the README*, which means the README diffs below are the
  load-bearing change. No edit to root `CLAUDE.md` needed.
- **`.claude/CLAUDE.md`** is operator-facing brief boilerplate ("You're
  in a bosun-managed worktree (session-3)"). The label is naming-
  scheme-independent — no change.

### Audit — adjacent docs

- **`docs/v0.6-trial-protocol.md` line 122–124** mentions the
  `<repo>-bosun-*` glob as the safety boundary the trial verifies.
  Glob still matches; **no change needed**, but the new trial #4 will
  re-verify it.
- **`docs/v0.9-spawn-spec.md` line 397** states *"Worktrees still
  live in `<repo>-bosun-*` siblings."* — same glob, still true.

### Proposed README diff

Wrapped in `<!-- proposed -->` markers per the brief. Apply when the
naming-scheme lanes have landed and the exact `<uid>` shape is known.

```diff
<!-- proposed: README.md lines 11-15 -->
 $ bosun init 4
 Created 4 sessions:
-  session-1  → ../myproject-bosun-1  (branch: bosun/session-1)
-  session-2  → ../myproject-bosun-2  (branch: bosun/session-2)
-  session-3  → ../myproject-bosun-3  (branch: bosun/session-3)
-  session-4  → ../myproject-bosun-4  (branch: bosun/session-4)
+  session-1  → ../myproject-bosun-a1b2c3-1  (branch: bosun/session-1)
+  session-2  → ../myproject-bosun-a1b2c3-2  (branch: bosun/session-2)
+  session-3  → ../myproject-bosun-a1b2c3-3  (branch: bosun/session-3)
+  session-4  → ../myproject-bosun-a1b2c3-4  (branch: bosun/session-4)
<!-- /proposed -->
```

```diff
<!-- proposed: README.md line 49 -->
-- Creates worktrees as sibling directories of your repo, named `<repo>-bosun-<session>` (e.g. `myproj-bosun-1`). Nothing is created inside the repo root other than `.bosun/`.
+- Creates worktrees as sibling directories of your repo, named `<repo>-bosun-<uid>-<session>` (e.g. `myproj-bosun-a1b2c3-1`). The `<uid>` is a short identifier bosun generates per `init` run so two separate runs on the same repo never collide on disk. Nothing is created inside the repo root other than `.bosun/`.
<!-- /proposed -->
```

The line-62 glob (`<repo>-bosun-*`) **stays unchanged**. That's the
key contract point: even though the example shape gets longer, the
glob description is unchanged because `*` already covers it. The
trial verifies this still holds in practice.

### Proposed root `CLAUDE.md` (contributor instructions) diff

Optional — adds an explicit reminder for parallel sessions working
in this repo so future Claude Code sessions don't accidentally
hardcode the old shape into a test fixture. Wrap in `<!-- proposed -->`.

```diff
<!-- proposed: CLAUDE.md "Git operations that need special care" section -->
 ## Git operations that need special care

 - **Worktree paths:** Use absolute paths internally even if relative ones work — Windows sometimes resolves relatives differently
+- **Worktree naming:** The worktree-directory naming scheme is `<repo>-bosun-<uid>-<session>` since the UID migration. Never hardcode `<repo>-bosun-<session>` in tests, doctor checks, or doc examples — derive the name through `internal/session` so a future scheme change has one place to update.
 - **Branch deletion:** Use `git branch -D` ...
<!-- /proposed -->
```

---

## 4. Phantom detection

`internal/phantom/phantom.go` matches two file shapes — Spotlight
`name N.ext` and iCloud `name (N).ext` — and is called by callers
enumerating `.bosun/claims/`, `.bosun/state/`, etc. with an
extension allow-list. (See `internal/phantom/phantom_test.go` for
the full case matrix.)

**The phantom regex matches *file* names, not directory names.** So
the immediate concern — does the worktree dir's new shape break
phantom — is a non-event for the phantom package itself. The
worktree dir's phantom risk lives in **`internal/doctor/checks_orphans.go`**,
which simply prefix-globs `<reporoot>-bosun-` and is naming-scheme-
agnostic.

**Does the new naming make phantom detection easier or harder?**
**Easier, slightly.** Three reasons:

1. **More distinctive base names → fewer false-positive risks on
   coincidental matches.** Today, a hypothetical user-written claim
   file like `auth 2.json` could plausibly be confused with the
   phantom shape of `auth.json`. With UID-extended session labels,
   the legit claim file is `auth-a1b2c3 2.json` — still matches the
   phantom regex (good, it *is* a phantom shape), and the legit name
   `auth-a1b2c3.json` is the un-spaced original. The regex behavior
   is identical; the chance of *accidental* collision with a third-
   party file shape drops.
2. **The extension allow-list (`"json"`, `"done"`, `"stuck"`,
   `"heartbeat"`) is what does the real gating, and it doesn't
   change.** Phantom detection's load-bearing line is the allow-list
   intersection with the duplicate-shape regex — both axes are
   untouched.
3. **The orphan dir scanner in doctor still works.** A Spotlight
   phantom directory `myproj-bosun-a1b2c3-1 2` is caught by the
   `myproj-bosun-` prefix walk; it isn't in the `git worktree list`
   output, so it's reported as an orphan. Same flow as today.

**Proposed regex updates: none.** The patterns

```go
var spotlightPattern = regexp.MustCompile(`^.* \d+\.[^.]+$`)
var iCloudPattern    = regexp.MustCompile(`^.* \(\d+\)\.[^.]+$`)
```

are agnostic to the base-name content. They keep working.

**Proposed test additions.** Add representative UID-shaped fixtures
to `internal/phantom/phantom_test.go` so future readers see the
intended coverage:

```go
// proposed additions to TestIsLikelyPhantom cases:
{"session-a1b2c3-1 2.json",   []string{"json"}, true},
{"session-a1b2c3-1 (3).json", []string{"json"}, true},
{"session-a1b2c3-1.json",     []string{"json"}, false},  // legit, not phantom
```

That's the only phantom-package change. The `IsLikelyPhantom`
contract is unchanged.

---

## 5. Trial #4: UID-naming validation

Cross-reference: this trial follows the
[`docs/v0.6-trial-protocol.md`](./v0.6-trial-protocol.md) shape —
external repo, real but disposable task, safety-contract checkpoints
before/after each operation. The pre-trial setup (binary on PATH,
target repo selection, operator-side pre-flight) is identical and
not re-documented here.

What changes for trial #4 is the **watch list** — the UID-naming-
specific surfaces that need to fire correctly.

### Phases that mirror v0.6 trial

Same three phases (solo shakedown → multi-session round → teardown).
Same red flags carry over verbatim. Same safety-contract checkpoints
(`git rev-parse main` before and after every command).

### UID-specific surfaces to exercise

These are the *new* signals trial #4 must capture. Each becomes a
checkbox in the findings doc.

1. **Fresh-repo init creates UID-named worktrees.**
   - `bosun init 3` on a virgin repo produces three dirs matching the
     new shape (no legacy fallback for new inits).
   - Branches still live under `bosun/<session>` (naming-scheme-
     independent). Verify with `git branch --list 'bosun/*'`.

2. **Legacy compatibility for already-existing worktrees.**
   - Hand-craft (or carry from a prior bosun version) a worktree at
     `<repo>-bosun-1` with a real `bosun/session-1` branch.
   - `bosun status` lists it. `bosun show session-1` works. `bosun
     done session-1` works. `bosun merge session-1` works.
   - Critically: `bosun init 2` **refuses** with the legacy-detected
     message. Doesn't silently start.

3. **`bosun migrate` end-to-end.**
   - Run `bosun migrate` against the same legacy worktree.
   - `git worktree move` is invoked under the hood; verify with
     `git worktree list` afterward. The branch is unchanged.
   - Any DONE/STUCK markers and rescue snapshots still resolve.
   - Rerun `bosun status` — session shows up under the new shape, no
     duplicates.
   - Force-corrupt: rename a legacy dir *outside* of `bosun migrate`,
     then run migrate. Expect a clear "directory missing" surface,
     not a panic.

4. **init.state version mismatch refusal.**
   - Save an old-shape init.state (`"version": "v0.6"`) into a fresh
     `.bosun/`. Run `bosun init --resume`. Expect the schema-mismatch
     refusal with the operator-facing remediation text.
   - Negative case: confirm fresh inits *without* a legacy init.state
     are not affected.

5. **Phantom detection on UID-shaped names.**
   - On macOS only. Force a Spotlight duplicate: create
     `.bosun/claims/session-a1b2c3-1 2.json` with junk content.
   - `bosun status` and `bosun list --ready` ignore it.
   - Same with iCloud-shape: `session-a1b2c3-1 (1).json`.
   - Hand-create a *directory* phantom: `<repo>-bosun-a1b2c3-1 2/`.
     `bosun doctor` reports it as an orphan dir.

6. **Spawn-tree (sub-session) naming under UID.**
   - With `agent_spawn.enabled: true`, parent session spawns one sub.
   - Sub's worktree dir must include the UID and the dotted label
     suffix (whatever the naming lanes agree on for the dot
     separator). `<repo>-bosun-a1b2c3-1.auth` is one plausible shape.
   - `bosun status` tree rendering still indents the sub correctly
     (the v0.9.1 tree code reads labels, not paths).
   - `bosun merge --tree session-1` cascades cleanly with the new
     shape.

7. **Safety contract — glob still holds.**
   - Set a filesystem timestamp marker before `bosun init`:
     `touch /tmp/bosun-trial-4-marker`.
   - Run the full Phase 2 workflow.
   - After: `find ~ -newer /tmp/bosun-trial-4-marker -type f` should
     list only files under `<repo>` and `<repo>-bosun-*` siblings.
     The `*` glob still matches the UID extension — verifying this is
     the central safety claim of section 3.

8. **Cross-version operator scenario.**
   - Simulate an operator who runs `bosun status` with the new binary
     against a `.bosun/` directory written by an older binary, no
     migrate yet. Read-only commands must not crash; they should show
     the legacy session and may add a one-line warning suggesting
     `bosun migrate`.

9. **Doctor's orphan scanner under both shapes.**
   - Plant one orphan directory in each shape (`myproj-bosun-orphan`
     legacy + `myproj-bosun-a1b2c3-orphan` new) in the parent dir.
   - `bosun doctor` reports both as orphans.
   - `bosun doctor --fix` renames both to `_orphan-*` per existing
     behavior.

### Red flags specific to UID rollout

In addition to the v0.6 trial's red flags, **stop immediately** if:

- **A new init writes a worktree under a non-UID name.** The whole
  point of the scheme migration is that new inits use the new shape.
  Legacy-shape dirs should only exist if they were created by an
  older binary.
- **`bosun migrate` deletes data.** Migrate is a `git worktree move`
  operation, never a delete. Any data loss is a contract break.
- **A legacy worktree becomes unreachable after a binary upgrade.**
  An operator should never lose the ability to `bosun merge` an
  in-flight legacy session by upgrading.
- **init.state schema-mismatch silently succeeds.** Refusal must be
  loud; a silent re-resume risks half-state worktrees on disk.

### Findings template (mirrors v0.6 trial)

Write `docs/uid-trial-findings.md` with:

- **Pre-flight diffs** — binary version, target repo, presence of any
  legacy worktrees at start.
- **Each numbered surface above** — what fired, what didn't, exact
  command + output.
- **Safety-contract checkpoint table** — `git rev-parse main` before
  and after each command, `find -newer` snapshots.
- **Bugs surfaced** — severity-classified per the v0.6 + v0.9 trial
  norms (docs nit / real bug / safety break).
- **Recommendation to ship** — explicit go / no-go on whether the
  naming-scheme lane is ready for the next public-tagged release.

### Out of scope for trial #4

- Performance impact of longer paths (Windows `MAX_PATH=260` only
  bites on already-deeply-nested repos; bosun doctor's existing
  socket-path check covers the worst case).
- Backporting the naming change to an older bosun binary. The
  contract is forward-only; old binaries continue to write old-shape
  dirs.
- Re-validating every v0.6 / v0.8 safety surface from scratch. Trial
  #4 layers on top of the v0.8 trial findings; only the UID-touching
  surfaces are in scope.

---

## Open questions for the implementation lanes

These came up while writing this doc and should be answered by
sessions 1+2's recommendation before any code lands:

- **UID generation determinism.** Is `<uid>` per-init random, per-
  repo-stable, or per-session deterministic? Affects whether the
  same operator running `bosun init 3` twice on the same day
  produces colliding directory names. Recommend per-init random.
- **UID character set.** Must be filesystem-safe across macOS (case-
  insensitive HFS+), Linux, and Windows. Lowercase hex (`a1b2c3`) is
  the obvious safe set.
- **UID length.** Long enough that two concurrent operators on the
  same repo don't collide; short enough that paths stay readable.
  6–8 hex chars feels right; defer to the naming lane.
- **Branch naming.** Does `bosun/session-1` stay unchanged, or does
  the UID propagate into the branch name too? This doc assumes
  branches stay unchanged — branches don't have the same collision
  problem worktrees do (git refuses duplicate branch names natively).

These don't block the migration story above. They're flagged here so
the implementation lanes don't ship a scheme that, in retrospect,
should have been settled before the safety-contract diff was
written.
