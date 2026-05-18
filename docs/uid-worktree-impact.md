# UID-worktree impact map

Survey of every site in the codebase that produces, parses, or
reasons about the worktree directory name. Compiled to give the
implementer of session-1's chosen UID design (path → opaque UID +
session label) a complete catalog of touchpoints — read this
instead of grepping cold.

Default suffix today is `-bosun-{N}`, so the canonical worktree
path for `session-3` against `/code/myproj/` is
`/code/myproj-bosun-3/`. The {N} substitution accepts either a
bare integer (numeric sessions) or a bare label (named sessions);
`session-N` labels strip the prefix before substitution so the
v0.1 numeric-mode paths stay byte-identical.

Conventions in this doc:
- **`file:line`** citations are anchors a reader can jump to.
- "Producer" = code that *generates* a worktree path string.
- "Consumer" = code that *reads a path off disk* and derives a
  label or other session identity from it.
- "Persisted reference" = on-disk artifact that records the path
  (or a derivative) and could go stale across a UID rename.
- "Display surface" = anywhere the path is shown to a human.

The session label (`session-3` / `auth`) is the canonical identity
inside bosun's `.bosun/` state — claims, state markers,
heartbeats, init.state, spawn-tree, events log are all
**label-keyed**, not path-keyed. The path appears as a
*derivable* convenience field in some on-disk records and as a
display element, but the underlying state would survive a path
rename. The big exception is `git worktree list --porcelain`,
which is git's own record of the path; that's the one place where
the path itself is load-bearing for session discovery.

---

## 1. Producers — where the worktree path is *generated*

### Primary producers

- **`internal/config/config.go:271`** — `Config.WorktreeSuffix(n int)`. Wrapper that
  formats the integer as a label and delegates.
- **`internal/config/config.go:280`** — `Config.WorktreeSuffixForLabel(label string)`.
  The substitution point. Strips a leading `session-` for numeric labels
  so `session-3` → `-bosun-3` (byte-identical with the v0.1 numeric
  path); other labels substitute whole (`auth` → `-bosun-auth`). Reads
  `Config.WorktreeSuffixPattern` (default `-bosun-{N}`).
- **`internal/session/session.go:338`** — `WorktreePathForLabel(repoRoot, cfg, label)`.
  Joins `filepath.Dir(repoRoot)` + `filepath.Base(repoRoot) +
  cfg.WorktreeSuffixForLabel(label)` to yield e.g.
  `/code/myproj-bosun-3`. **Every call site below ultimately reaches this
  function.**
- **`internal/session/session.go:330`** — `WorktreePath(repoRoot, cfg, n int)`.
  Numeric-wrapper that delegates to `WorktreePathForLabel`.

### Call sites that materialize the path

| Site | What the caller does with the path |
| ---- | ---------------------------------- |
| `cmd/bosun/cmd_init.go:298` | Pre-flight: refuses to clobber if dir exists (unless `--force` or `--resume` for the matching label). |
| `cmd/bosun/cmd_init.go:321` | `--force` cleanup: removes existing worktree at this path before re-creating. |
| `cmd/bosun/cmd_init.go:368` | `--resume` reconciliation: verifies the worktree for each previously-completed label is still on disk; refuses to proceed if missing. |
| `cmd/bosun/cmd_init.go:392` | Main create loop: passes to `git worktree add <path> <branch>`. |
| `cmd/bosun/cmd_remove.go:63` | Resolves the worktree path so `bosun remove` can check for gitdir corruption (`git.WorktreeGitdirCorruption`) and dispatch to the corrupted-recovery path if needed. |
| `cmd/bosun/cmd_rescue.go:77` | Same corruption pre-check; rescue won't proceed into `Derive` against a torched gitdir. |
| `internal/mcp/tool_spawn.go:85` | Auth gate #3 ("parent identity"): builds the parent's expected worktree path and asks `proc.Running(worktreePath)` whether a live agent's CWD matches; refuses spawn if no agent is detected in the parent worktree. |
| `internal/spawn/spawn.go:104` | Sub-session create pipeline: passes to `git worktree add` for each `<parent>.<suffix>` sub-session brief block. |

### Glob/match producers (translate the *pattern*, not a specific label)

- **`internal/git/git.go:550`** — `ScanOrphanDirs(repoRoot, suffixPattern)`.
  Builds a `filepath.Match` glob by translating `{N}` → `*`, so any
  sibling dir matching `<base><pattern-with-*-sub>` is a candidate.
  Used by `bosun cleanup --orphan-dirs`.
- **`cmd/bosun/cmd_cleanup.go:521`** — only call site of
  `ScanOrphanDirs`.

### Hidden "producer" — git's own admin-dir naming

`git worktree add <path> <branch>` creates
`<main-repo>/.git/worktrees/<basename-of-path>/` to hold per-worktree
admin files (HEAD, commondir, gitdir, config, info/exclude). The
basename is git's choice, not bosun's, but it derives directly from
the worktree path bosun computed. Citations:

- **`internal/git/worktree_health.go:17–27`** — corruption check walks
  `<worktreePath>/.git` → `gitdir: <main>/.git/worktrees/<name>/` →
  stats `HEAD` and `commondir`.
- **`internal/git/git.go:728`** — `AppendWorktreeExclude` writes to
  this admin dir's `info/exclude` (via `git rev-parse --git-path`,
  not by string-building, so the lookup is robust to any future
  naming the git binary chooses).

A UID rename of the worktree path changes git's admin-dir basename
too. Bosun never hardcodes that basename, so no code change is
required — but any operator-side scripts or tooling that addresses
`.git/worktrees/<basename>/` by name would have to adapt.

---

## 2. Consumers — path → label

Bosun discovers sessions primarily via `git worktree list --porcelain`
+ a *branch-name* regex, **not** a path regex. So the canonical
"is this worktree a bosun session" decision avoids parsing the
path. The path-shape parsers are narrow:

### Discovery (branch-driven, not path-driven)

- **`internal/session/session.go:115`** — `Derive(...)`. Calls
  `git.ListWorktrees`, then matches each worktree's branch ref
  against `^refs/heads/<SessionPrefix>/([a-z][a-z0-9-]*)$`
  (`session.go:124`). The label is captured from the branch, not
  the path. `wt.Path` is recorded into `Session.Path` (line 216)
  as the absolute path git reports, so downstream code uses what
  git canonicalized rather than re-deriving.
- **`internal/git/git.go:431`** — `ListWorktrees(ctx, dir)` runs
  `git worktree list --porcelain` and `internal/git/git.go:439`
  parses it. `Path`/`HEAD`/`Branch`/`Locked`/`Prunable`/`Bare`
  fields are filled from the porcelain stream. This is the
  authoritative on-disk source for worktree paths.

### Path → label (only one real call site)

- **`internal/claudehook/resolve.go:28`** — `LabelFromWorktreePath(repoRoot, worktreePath, cfg)`.
  The lone reverse-direction parser. Takes
  `filepath.Base(worktreePath)`, strips `filepath.Base(repoRoot)`
  prefix, splits the remainder against the literal prefix/suffix
  flanking `{N}` in `cfg.WorktreeSuffixPattern`, validates the
  middle as either a bare integer (→ `cfg.SessionName(n)`) or a
  bare label (→ `ValidateLabel`). Returns `""` for non-bosun
  paths so the hook silently no-ops.
- **`internal/claudehook/hook.go:136`** — only caller. The
  PreToolUse hook uses this to figure out which session owns the
  edit so the path gets claimed in `.bosun/claims/<session>.json`.

Both sites are sensitive to the UID design: any new UID scheme
that decouples the on-disk basename from the label must update
`LabelFromWorktreePath` (or replace it with a label lookup that
doesn't depend on the basename's literal shape).

### Cross-VLAN / cross-process: proc.Running

- **`internal/proc/detect.go:101`** — `Running(worktreePath)`. Walks
  the process table and matches an agent's CWD against the given
  worktree path. `canonicalize()` (`detect.go:170`) runs
  `filepath.EvalSymlinks` on both sides so `/tmp` vs `/private/tmp`
  doesn't trip the comparison.

This isn't a path-to-label consumer per se, but it's the third
spot that compares worktree paths as strings: the agent's
recorded CWD vs the path bosun computed. Always reduces both
sides through `filepath.EvalSymlinks`, so a UID rename mid-flight
would simply mismatch and the RUNNING column would degrade to
"—" until the agent's CWD catches up.

---

## 3. Persisted references — where the path lands on disk

The headline: **bosun's own `.bosun/` state is label-keyed, not
path-keyed.** Filenames inside `.bosun/state/`, `.bosun/claims/`,
`.bosun/rescues/`, plus `.bosun/spawn-tree.json` and
`.bosun/init.state`, all key on session label. A worktree rename
would *not* invalidate these files.

The only on-disk record of the worktree path itself is git's
worktree admin (`.git/worktrees/`) plus the worktree's own
`.git` pointer file. Bosun reads those through `git` subprocesses,
never by direct file parsing.

| File | Keying | Stores worktree path? | Citation |
| ---- | ------ | --------------------- | -------- |
| `.bosun/init.state` | label list | No — `Labels` / `CompletedSessions` / `CurrentSession` are session labels. | `internal/init/state.go:59-86`, `:113-127` |
| `.bosun/claims/<label>.json` | session label (file basename) | No — `Paths` field is repo-relative file globs being claimed, not worktree paths. | `internal/claims/claims.go:57-59`, `:35-39` |
| `.bosun/state/<label>.{done,stuck,heartbeat}` | session label | No — body is RFC3339 timestamp ± message. | `internal/state/state.go:99-101`, `:109-141` |
| `.bosun/rescues/<label>-<timestamp>/` | label + timestamp | No — files inside preserve repo-relative paths from the rescued worktree. | `cmd/bosun/cmd_rescue.go:119-127`, `cmd/bosun/cmd_remove.go:250-251` |
| `.bosun/spawn-tree.json` | label | No — `Sessions` map keys are labels; `Parent` / `Children` lists hold labels. | `internal/spawntree/spawntree.go:64-71` |
| `.bosun/events.log` | label per record | No — each JSONL row carries `session` (label), `kind`, `message`, `at`. | `internal/mcp/events.go:28-33` |
| `.bosun/briefs/plan.last.md` | none | No — archived plan markdown. | `internal/brief/brief.go:17`, `:331-343` |
| `.bosun/config.json` | none | No — only the *pattern* (`worktree_suffix_pattern`), not specific paths. | `internal/config/config.go:32` |
| `.bosun/.metadata_never_index` | none | No — Spotlight marker file (empty). | `internal/state/state.go:33-61` |
| `.bosun/mcp.sock` | repo-keyed | No — daemon's Unix socket. Lives under the **main** repo's `.bosun/`, not per worktree. | `internal/mcp/server.go:55-73` |

### Git-owned persistence (the one place path matters)

- **`<main-repo>/.git/worktrees/<basename-of-worktree>/`** — git's
  per-worktree admin dir. Contents (`HEAD`, `commondir`, `gitdir`,
  `config`, `info/exclude`) are managed by `git`. Bosun never
  hardcodes the basename — it accesses these files via `git
  rev-parse --git-path …` (`internal/git/git.go:733`). Health
  inspection at `internal/git/worktree_health.go:36-86`.
- **`<worktree>/.git`** — a pointer file containing `gitdir:
  <abs-path-into-main-repo>`. Read by
  `WorktreeGitdirCorruption` (`worktree_health.go:52-67`) and
  used by `looksLikeLiveWorktree` in cleanup
  (`cmd/bosun/cmd_cleanup.go:573-584`) to refuse removal of
  dirs that still carry a live pointer.

A UID-driven rename of the worktree path itself would have to
go through `git worktree move` (or full delete + re-add) so git's
admin records stay consistent — bosun has no in-tree handler for
operator-driven `mv`.

### Best-effort in-memory snapshots that carry the path

These aren't persisted, but they record `Session.Path` in flight
and should be noted because they're surfaced to consumers:

- **`internal/session/session.go:50`** — `Session.Path` field; populated
  by `Derive` from `wt.Path` (git's view).
- **`internal/status/status_json.go:75`** — `path` field in the
  `bosun status --json` / `/api/status` payload (wire-stable per
  the doc comment at `status_json.go:8-52`).
- **`cmd/bosun/cmd_show.go:97`** — `worktree` field in `bosun show
  --json`.
- **`internal/web/handlers.go:97`** — `path` field on `/api/show/`.
- **`BOSUN_WORKTREE_PATH`** env var passed into the `pre-remove`
  hook (`cmd/bosun/cmd_remove.go:165`). A UID design that
  changes the displayed string also changes what hooks see.

---

## 4. Display surfaces — where the path or its truncation shows up

### CLI text rendering

- **`cmd/bosun/cmd_init.go:577`** — "Created N session(s):" summary
  block prints `<label> → <worktreePath> (branch: <branch>)` per
  session. **Length matters**: long UIDs would wrap or push past
  reasonable terminal widths.
- **`cmd/bosun/cmd_init.go:311`** — error path: "worktree path
  already exists: %s (use --force to overwrite)".
- **`cmd/bosun/cmd_init.go:324, 334, 377`** — operator-facing
  progress lines that name the worktree path during `--force`
  cleanup and `--resume` reconciliation.
- **`cmd/bosun/cmd_rescue.go:133, 136`** — rescue output mentions
  the destination dir under `.bosun/rescues/`, not the source
  worktree path directly, but the source `s.Path` is implicit
  via the snapshot copy.
- **`cmd/bosun/cmd_remove.go:184, 189, 192, 215, 232, 234`** —
  warnings/errors during remove paths quote `s.Path` /
  `worktreePath`.
- **`cmd/bosun/cmd_show.go:137`** — `bosun show <label>` text mode
  prints `Worktree: <path>` as its third header line.
- **`internal/doctor/checks_orphans.go:88, 124`** — orphan-worktree
  check Message lists up to 3 absolute paths, with " (and N
  more)" tail. Truncation already present.
- **`internal/doctor/checks_orphans.go:21, 97`** — note the
  **hardcoded `repoRootName(repoRoot) + "-bosun-"` prefix**.
  The doctor check assumes the default suffix shape rather than
  reading `cfg.WorktreeSuffixPattern`. Pre-existing drift, not a
  UID-specific issue, but a UID change would have to revisit this
  string-build.

### Table renderers (path NOT shown)

The status table — both CLI and TUI — does **not** print the
worktree path:

- **`internal/status/status.go:67`** — CLI columns are SESSION,
  BRANCH, STATE, AHEAD, DIRTY, CLAIMED, RUNNING, LAST_COMMIT.
- **`internal/tui/control/view.go:88`** — TUI columns are SESSION,
  BRANCH, STATE, AHEAD, DIRTY, CLAIMED, LAST.
- **`internal/web/static/index.html:242-258`** — web brief panel
  shows name, state, branch, ahead, dirty, claimed, claimed
  paths, brief body. No worktree path field rendered.

So **column-width pressure from a longer UID is minimal** for the
default `bosun status` view. The pressure lives in the per-row
JSON payload (`path` field, consumed by scripts) and in the
`bosun show` text output. A UID change should keep `bosun show`
readable on an 80-column terminal.

### Brief preamble — label, not path

The brief preamble (`internal/brief/brief.go:247-273`,
`WriteSessionPointer` at `:319-329`) embeds the session **label**
into BOSUN_BRIEF.md and `.claude/CLAUDE.md` — not the worktree
path. So a UID change to the path leaves brief preamble copy
unaffected.

### Env vars exported to hooks

- `BOSUN_REPO_ROOT` — main-repo path. Stable across UID redesign.
- `BOSUN_WORKTREE_PATH` — `s.Path`; exported to `pre-remove`
  (`cmd_remove.go:165`). A UID change is observable here; hook
  authors who pattern-match this string should be flagged.
- `BOSUN_SESSION_LABEL` — emitted to `post-done`
  (`cmd_done.go:93`). Always the label.
- `BOSUN_SESSION` — emitted to `pre-remove` (`cmd_remove.go:163`).
  The label.
- `BOSUN_MCP_SOCK` — daemon's socket path; main-repo-scoped, not
  worktree-scoped.

---

## 5. Cross-OS hazards

### Windows path length (MAX_PATH ≈ 260)

A long UID inflates the worktree path. The path appears in two
places that compound:

1. The worktree itself: `/parent/<repo>-bosun-<label>` becomes
   `/parent/<repo>-bosun-<UID>`. A typical UID (UUIDv4 hex: 36
   chars) replaces a 2-char numeric label with 18× the length.
2. The git admin dir: `<main-repo>/.git/worktrees/<basename>/<file>`
   where `<basename>` is the worktree dir's basename. Long
   basenames inflate every path inside the admin dir.

Bosun's only explicit path-length check (`internal/mcp/server.go:53`
and `internal/doctor/checks_mcp.go:75`) covers Unix socket path
length (104/108 bytes) and only applies to the **main repo's**
`.bosun/mcp.sock`, not per-worktree paths. A UID change has no
direct effect on the socket-path budget.

For **Windows MAX_PATH**: no current check. The OS-level risk is
real for deeply-nested repos, but bosun doesn't currently warn
on it for either the present `-bosun-N` scheme or a hypothetical
UID one. **UID makes this worse by a factor of ~10× per session**
in characters. Recommendation: pair any UID design with a doctor
check that flags `len(worktreePath) + len(longest-relpath) > 240`
on Windows.

### macOS phantom regexes

- **`internal/phantom/phantom.go:25-29`** — `spotlightPattern` =
  `^.* \d+\.[^.]+$`, `iCloudPattern` = `^.* \(\d+\)\.[^.]+$`. Both
  require a literal `.` (file extension) to match. Bosun worktree
  basenames have no extension (`myrepo-bosun-3`, not
  `myrepo-bosun-3.json`), so the phantom regex doesn't fire on
  the parent dir entries.
- The phantom filter operates on `.bosun/state/*.{done,stuck,…}`
  and `.bosun/claims/*.json` (which *do* have extensions). Those
  files are label-keyed (label has no `.`), and a numeric label
  embedded in a UID-shaped string still wouldn't match if it
  has no extension on the worktree dir.

**Verdict: UID change is neutral for phantom regexes.** A
new-shape label that introduces a `.` (e.g. UUIDv4 with the
hyphens flipped to dots) would change this analysis — current
labels reject `.` outside the v0.9 `parent.child` sub-session
separator anyway (`internal/session/session.go:274`), so the
analysis covers that case too.

### Linux case-sensitivity / mount points

No code in the survey assumes the worktree path is on the same
filesystem as `.bosun/`. `ScanOrphanDirs` uses `os.ReadDir(parent)`
+ `filepath.Match`, which works across mount boundaries. The
case-sensitivity concern is the case-insensitive macOS HFS+
default vs case-sensitive ext4: bosun's labels are forced to
lowercase by `ValidateLabel`
(`internal/session/session.go:267-290`), so the on-disk basename
is also lowercase. A UID alphabet should stay lowercase (or at
least case-consistent) to keep this property.

### macOS symlink canonicalization

`/var` → `/private/var` (and `/tmp` → `/private/tmp`) is handled
in three places:

- `internal/proc/detect.go:170` — `canonicalize()` for CWD vs path
  comparison.
- `internal/claudehook/hook.go:247-274` — `relativizePath` resolves
  both sides through `filepath.EvalSymlinks` before
  `filepath.Rel`. Already battle-tested for not-yet-existing files
  via `resolveExistingAncestor` (`hook.go:281-302`).
- `cmd/bosun/cmd_init.go:769` — `canonicalAbs` for the plan-file
  inclusion check.

A UID change keeps the path under the same parent (`filepath.Dir(repoRoot)`),
so all three sites continue to function unchanged.

### Hardcoded shape assumptions (not strictly cross-OS, but cross-config)

A UID redesign should also revisit these literal-`-bosun-` sites:

- `internal/doctor/checks_orphans.go:21,97` — `prefix :=
  repoRootName(repoRoot) + "-bosun-"`. Hardcoded; does not read
  `cfg.WorktreeSuffixPattern`. The doctor's orphan check would
  miss UID-shaped orphans if the new suffix omits `-bosun-`.
- `internal/config/config.go:20` — `DefaultSuffixPattern =
  "-bosun-{N}"`. Changing this default during a UID rollout has
  to be handled with care: every existing checkout that relied
  on the implicit default would suddenly compute a different
  worktree path on next init.

---

## Summary of UID-rename surface area

If session-1's recommendation is "decouple the on-disk worktree
basename from the human-readable session label" — i.e. the basename
becomes a UID and the label is looked up via a side index — the
implementation has to:

1. **Replace path-parsing with index lookup** at
   `internal/claudehook/resolve.go:28` (the lone path → label
   parser). This is the load-bearing change. Likely a new file under
   `.bosun/` mapping `<basename>` → `<label>`.
2. **Add an index writer** alongside every `WorktreePathForLabel`
   call site that mints a new worktree
   (`cmd_init.go:298/321/368/392`, `spawn/spawn.go:104`).
3. **Update `internal/git/git.go:550` (`ScanOrphanDirs`)** so the
   glob translation matches the new shape; pair with
   `cmd/bosun/cmd_cleanup.go:521`.
4. **Update `internal/doctor/checks_orphans.go:21,97`** to read the
   pattern rather than hardcoding `-bosun-`.
5. **Decide what `bosun show`'s `Worktree:` line and the JSON
   `path` field show**: the on-disk UID-path, the label-derived
   "logical" path, or both. The wire-stable `path` field
   (`status_json.go:75`) is the binding decision — changing it
   to anything other than git's authoritative absolute path is a
   schema break.
6. **Keep the brief preamble label-based** (no change required at
   `brief.go:247-273` or `:319-329`); the agent's BOSUN_BRIEF.md
   already references the label, not the path.
7. **Decide whether `BOSUN_WORKTREE_PATH`** (`cmd_remove.go:165`)
   surfaces the UID-path or the logical path. Hook authors who
   wired up scripts that pattern-match this var would be affected.

Everything else in the catalog (claims, state, rescues, spawn-tree,
init.state, events, briefs, MCP socket, phantom regex, macOS
symlinks, env vars, proc.Running) is already label-keyed,
path-string-canonicalized, or otherwise UID-neutral.
