# macOS worktree corruption — forensic findings (2026-05-17/18)

The v0.9 bug hunt round (firing four lanes to investigate trial
#3c findings) was supposed to fix the spawn lifecycle. Instead, the
hunt's environment broke before the lanes could land a single
finding. All four worktrees lost their git admin metadata within ~5
minutes of `bosun init`, and the broken state matched the same
shape that trial #3c surfaced on a different filesystem (SneakerNet
USB SSD vs iCloud-managed APFS). **This is not a v0.9 spawn bug —
it's a broader macOS worktree-creation bug** that affects every
multi-worktree `bosun init` on this hardware.

This doc is the forensic record before any cleanup. Operator
chose "forensics first" — the corrupt state on disk has been
preserved for follow-up investigation. Kill / wipe disposition
is deferred to a separate decision.

## What was running

- macOS host: Darwin 25.2.0 (assumed Sequoia variant)
- iCloud Drive: enabled; `~/Documents/` is the path. `brctl status`
  reports "Client zone not found" for the bosun repo path —
  i.e., this specific dir is NOT currently a syncing iCloud zone,
  but it lives inside the iCloud-managed parent.
- File Provider daemon: FPCKService PID 1446 (running since 6:00PM)
- Spotlight daemons: distnoted, mdworker, managedcorespotlightd,
  spotlightknowledged — all running.
- Recent history on this machine:
  - 2026-05-17 ~12:00 — v0.9 release work (committed + pushed)
  - 2026-05-17 ~16:00 — v0.9.1 round 1 (4 lanes init+launch on this
    same path, completed successfully, no corruption)
  - 2026-05-17 ~17:00 — UID-planning round (3 lanes, also clean)
  - 2026-05-17 ~22:14 — Trial #3c started on `/Volumes/SneakerNet/`
  - 2026-05-17 ~22:45 — Bug hunt round, this corruption observed
- Same hardware, same bosun version, same `init` codepath worked
  fine 6 hours earlier. Something changed in the environment
  between the v0.9.1 round and this hunt.

## What `bosun init 4` was supposed to produce

For each of session-{1,2,3,4}, git worktree add normally creates:

```
~/Documents/Homelab/bosun/.git/worktrees/bosun-bosun-N/
├── HEAD          ← current commit ref (~50 bytes)
├── commondir     ← path to main .git dir (~7 bytes)
├── gitdir        ← path back to worktree (~70 bytes)
├── index         ← staged-tree state
├── logs/HEAD     ← HEAD ref history
└── refs/         ← worktree-local refs
```

And the worktree dir itself (`~/Documents/Homelab/bosun-bosun-N/`)
gets a single `.git` text file containing
`gitdir: /path/to/.git/worktrees/bosun-bosun-N`.

## What's actually on disk

Per-session, after the round:

| File / dir | session-1 | session-2 | session-3 | session-4 |
|---|---|---|---|---|
| `.git/worktrees/<name>/HEAD` | ❌ missing | ❌ missing | ❌ missing | ❌ missing |
| `.git/worktrees/<name>/commondir` | ❌ missing | ❌ missing | ❌ missing | ❌ missing |
| `.git/worktrees/<name>/gitdir` | ❌ missing | ❌ missing | ❌ missing | ❌ missing |
| `.git/worktrees/<name>/index` | ✓ | ✓ | ✓ | ❌ missing |
| `.git/worktrees/<name>/logs/HEAD` | ✓ | ✓ | ❌ missing | ❌ missing |
| `.git/worktrees/<name>/refs/` | ✓ (empty) | ✓ (empty) | ✓ (empty) | ✓ (empty) |
| `<worktree>/.git` text pointer | ✓ | ✓ | ✓ | ✓ |
| Worktree dir contents (BOSUN_BRIEF.md, etc.) | ✓ | ✓ | ✓ | ✓ |

**The same files are missing across all four sessions:** every
top-level admin file (HEAD, commondir, gitdir) — exactly the files
git uses to bidirectionally link a worktree to the repo. Without
them, `git worktree list` skips the entry, `git status` from inside
the worktree fails with "fatal: not a git repository", and bosun
can't see the session at all.

**The asymmetry between sessions is also informative.** session-1
and -2 still have logs/HEAD (the HEAD-ref history file), but
session-3 and -4 don't. This is consistent with "something is
deleting files in some order" rather than "git never wrote them" —
git would write the same set of files for every session.

## Phantom directories — the iCloud fingerprint

`find .git/worktrees -maxdepth 2` shows the standard admin dirs PLUS
**phantom-suffixed duplicates** with the iCloud File Provider naming
pattern:

```
.git/worktrees/bosun-bosun-1 2/index 3
.git/worktrees/bosun-bosun-2 2/                  ← empty
.git/worktrees/bosun-bosun-3 2/COMMIT_EDITMSG 2
.git/worktrees/bosun-bosun-3 2/index 3
.git/worktrees/bosun-bosun-5/                    ← ZOMBIE from 2026-05-16
```

The ` 2` / ` 3` suffix is the documented iCloud File Provider
conflict-resolution pattern — when File Provider sees two versions
of a file/dir name that conflict, it appends ` 2` to one to
disambiguate. This is the same pattern bosun's `internal/phantom/`
package was built to detect (but only for files inside worktrees,
not inside `.git/worktrees/` admin dirs).

`bosun-bosun-3 2/COMMIT_EDITMSG 2` is the most diagnostic: it's
an iCloud-renamed copy of a `COMMIT_EDITMSG` file (which git creates
during a commit operation) from MAY 17 08:26 — i.e., from work
that's been done and reaped LONG before today's round. iCloud has
been caching phantom copies of these admin files across multiple
bosun rounds, leaving them in `.git/worktrees/` even after the
"real" admin dirs were purged.

The `bosun-bosun-5` zombie from 2026-05-16 13:27 confirms this:
bosun has never had a session-5 in this round, but the dir
exists. Its `logs` and `refs` subdirs have a reported `nlink`
of **65535** (the max value) with size 2097120 — clearly nonsense
metadata, probably from a partial-restore or a half-corrupted
inode.

## xattr signature

Every file and dir under `.git/worktrees/` has the
`com.apple.provenance` extended attribute. This is set by macOS
Endpoint Security Framework to track file lineage — typically
attached when files cross sandboxed-app boundaries or are created
by certain system services. The presence on EVERY file (not just a
few) is unusual and suggests the entire `.git/worktrees/` tree has
been touched by some EndpointSecurity-aware process.

`com.apple.provenance: ` (empty value) doesn't decode to anything
human-readable; need a privileged tool to inspect further.

## Why git worktree repair didn't help

`git worktree repair` regenerates HEAD/gitdir/commondir from the
worktree's `.git` text pointer — but only if the worktree dir has
enough state to reconstruct them. The implementation requires the
admin dir to exist; it doesn't recover when key files are missing
WITHOUT a tracked source of truth.

In our case: `git worktree repair` returned silently with no
changes. The worktree's `.git` pointer files are intact, the admin
dirs exist with `refs/` and partial state, but git's repair logic
either can't write to the admin dir (perms? race?) or doesn't
recognize the corrupt-state shape as repairable.

## Process state — agents are alive but stuck

`ps aux | grep "Read BOSUN_BRIEF"` shows ~20 claude processes
hanging in the four worktrees. Each process is alive (some at low
CPU, some at low %MEM), with cwd pointing into the corrupt worktree
dirs. They can't `git status` (returns the "not a git repository"
fatal). They're waiting for something that won't come.

Modification-time check (`find <worktree> -newer ~/Documents/Homelab/bosun/main.go`)
returns ZERO files in any of the four worktrees. **Nothing was
written.** The agents had no chance to do work — they were
launched into a broken environment.

## Hypotheses for root cause

In rough order of plausibility:

1. **iCloud File Provider race**. The most likely culprit.
   `bosun init 4` writes the four admin dirs sequentially, but
   ~3-4 seconds apart. iCloud File Provider scans these as they
   land, decides they conflict with cached state from prior rounds,
   and "resolves" the conflict by suffixing duplicates (` 2`)
   and stripping originals. Some files survive (refs/, partial
   logs/, index) because File Provider doesn't recognize them as
   "documents"; the top-level metadata files (HEAD/gitdir/commondir,
   which look like plain-text files with no extension) get treated
   as documents and get the phantom treatment.
2. **Spotlight metadata extraction**. The CoreSpotlight daemons
   index `.git/` contents (they shouldn't, but they have been known
   to in past macOS versions). Indexing under iCloud File Provider's
   eye may trigger a "this file is gone, please restore from cloud"
   path that's actually destructive when the cloud state is stale.
3. **Time Machine local snapshots interaction**. APFS local
   snapshots could be touching the dirs in ways that look like
   modifications to File Provider. Less likely given the consistent
   pattern.
4. **A specific macOS update introduced regression**. The same
   `bosun init` worked 6 hours earlier on the same machine. Either
   (a) some background process started running between those two
   rounds (Time Machine kickoff? Spotlight reindex?) or (b) iCloud
   was idle then and active now (just-finished sync left state
   that File Provider's now reconciling with).

## What this means for bosun

**Trial #3c's "sub-worktree disappears" wasn't spawn-specific.** The
spawn flow's three sub-worktrees and the bug hunt round's four
init worktrees suffered the same corruption. Two filesystems
(SneakerNet APFS, iCloud-managed APFS), same outcome, same
fingerprint (` 2`/` 3` phantom dirs).

**Bosun's `internal/phantom/` mitigation is scoped wrong.** It
filters phantoms out of bosun status / merge / cleanup outputs for
files INSIDE worktrees, but doesn't scan `.git/worktrees/` admin
dirs. The phantom dirs there sit invisible to bosun's existing
defenses.

**This blocks v0.9 launch checklist Phase 4a, not just 4b.** Phase
4a flips v0.7+v0.8 public. If `bosun init N` on a macOS user's
iCloud-managed home dir is unreliable, the first community trial
will surface this immediately and the trust contract that v0.8
established is broken before v0.9 is even visible.

## Recommended investigations

Before any code fix attempts:

1. **Reproduce in a controlled env.** Take this same machine to a
   path OUTSIDE iCloud Drive coverage (e.g., `/tmp/bosun-repro/`)
   and run `bosun init 4`. If the corruption doesn't reproduce,
   that confirms iCloud as the trigger.
2. **Reproduce with iCloud disabled.** Sign out of iCloud Drive
   temporarily; rerun. If the corruption stops, that's confirmation
   beyond doubt.
3. **Watch fileproviderd and Spotlight live.** `fs_usage -w -f
   filesys` while running `bosun init 4` — capture which process
   touches the admin files. Filter to PIDs in
   `ps | grep -i 'fileprovider\|spotlight'`.
4. **Check `xattr` change events.** Specifically look at when
   `com.apple.provenance` was applied to the admin files. If it's
   AFTER git wrote them, that names the culprit process via the
   EndpointSecurity hook.

## Recommended code changes (after root-cause is confirmed)

- **`bosun doctor`** should scan `.git/worktrees/` for phantom
  dirs (` N` suffix) and missing top-level admin files (HEAD,
  commondir, gitdir). Surface as ERROR not WARN — these break
  every multi-worktree command.
- **`bosun init`** should refuse to run on iCloud-managed paths
  (detect via `brctl status` or `mdls -name kMDItemFSCreationDate`).
  Point operator at `docs/macos-setup.md` recovery steps.
- **`bosun cleanup`** should reap phantom admin dirs (` N`
  suffixes) in `.git/worktrees/` alongside its normal cleanup.
- **`internal/phantom/` extension** to scan admin-dir-side
  phantoms, not just worktree-internal ones.

## State preserved for follow-up

The corrupt state is preserved as of 2026-05-17 22:50ish:

- 4 worktree dirs at `~/Documents/Homelab/bosun-bosun-{1,2,3,4}/`
- 4 admin dirs + 3 phantoms + 1 zombie at
  `~/Documents/Homelab/bosun/.git/worktrees/`
- ~20 stuck `claude` processes in the broken worktrees (PIDs
  observable via `ps aux | grep 'Read BOSUN_BRIEF'`)
- `bosun status` returns "no sessions" — bosun's view is empty

Operator decides cleanup disposition separately. Killing the
agents + wiping admin dirs + `git worktree prune` would resolve
the immediate state but lose forensic evidence. Recommend
investigating per the section above BEFORE wiping.
