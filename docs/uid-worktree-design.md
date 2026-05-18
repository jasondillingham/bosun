# UID-per-worktree directory naming — design proposal

**Scope.** Directory names only. Branches stay `bosun/<label>`, labels stay
`session-N`, state file keys stay the same. This proposal changes the
substituted value of `{N}` in `WorktreeSuffixPattern` so the on-disk path
carries a per-round identifier in addition to the session number.

## 1. Pain-point survey

Concrete cases the current `-bosun-{N}` naming has produced, drawn from
v0.4–v0.9 incidents:

1. **"worktree path already exists" on fresh init.** v0.6 trial #1
   ([v0.6-trial-findings.md:122](v0.6-trial-findings.md)) and
   v0.8 trial #1 both hit this. A prior round's `repo-bosun-1` dir
   survived `cleanup` or a SIGBUS-killed init, so the next `bosun init`
   refused unless the operator passed `--force`. Each round reuses
   slots 1..N, so collisions are guaranteed whenever any prior dir
   lingers.
2. **Orphan-dir cleanup is ambiguous.** `ScanOrphanDirs`
   ([internal/git/git.go:537](../internal/git/git.go)) finds any sibling
   matching the glob, but a human staring at three `repo-bosun-1` dirs
   on disk (from rolled-back trials) has no way to tell which is the
   live one without `git worktree list`. Trial repos accumulate cruft
   over time and the operator has to guess.
3. **Cross-round identity confusion.** Both v0.8 and v0.9 trials shipped
   work on `bosun/session-1`. "Which session-1 was the auth lane?" has
   no answer from the dir name — operators have been using the merge
   log + branch SHAs to disambiguate, but the worktree dir is anonymous.
4. **Half-finished worktree from SIGBUS.** v0.8 trial #1 — `git worktree
   add` died mid-checkout. The breadcrumb resume path works
   ([cmd_init.go:291](../cmd/bosun/cmd_init.go)), but resume is doing
   extra work (unlock, re-register) to avoid a collision a unique dir
   name would sidestep entirely.
5. **Stale-branch + reused-dir compound failure.** v0.7 roadmap §3 calls
   out the case where `bosun/session-1` lingers from a prior round; the
   matching dir slot makes the silent reuse possible. Per-round
   identifiers in the dir would at least surface the divergence
   visually.
6. **Phantom *files* (`session-1 2.json`) are already handled** by
   `internal/phantom`. Spotlight/iCloud duplicate files within
   `.bosun/`, not parent dirs, so UID dir naming does **not** retire
   the phantom filter. Worth stating explicitly so the design doesn't
   over-claim.

## 2. Candidate schemes

For each: the existing config's `WorktreeSuffixPattern` is a literal
`strings.ReplaceAll` on `{N}` ([config.go:280](../internal/config/config.go)).
The cleanest implementation path is to **enrich the value substituted
for `{N}`** rather than introduce new placeholders — pattern validation,
the orphan-dir glob (`{N} → *`), and operator config files all keep
working unchanged.

### A. UUID v7 suffix

- **Format.** `repo-bosun-1-018f2a7c` (8 hex chars of a UUID v7 round id).
- **Human read.** "bosun-1, round 018f2a7c" — needs a lookup against
  `bosun status` or the merge log to map id → meaning. UUIDv7 is
  time-ordered so newer dirs sort after older ones lexically.
- **Tooling fit.** Substitute the composite `1-018f2a7c` for `{N}`.
  Pattern stays `-bosun-{N}`. Glob `*-bosun-*` continues to match.
  Operators who set a custom `WorktreeSuffixPattern` are unaffected.
- **Length.** Suffix `-bosun-N-XXXXXXXX` = 17 chars (N ≤ 99). Windows
  MAX_PATH headroom is comfortable inside any reasonable parent dir.

### B. Round-scoped prefix (`bosun-{round-id}/session-N`)

- **Format on disk.** `repo-bosun-r3/session-1` — *nested* layout, one
  dir per round containing one subdir per session.
- **Human read.** "round 3, session 1" — most legible grouping; you can
  `rm -rf repo-bosun-r3/` to drop a whole round.
- **Tooling fit.** Doesn't survive the current
  `WorktreeSuffixPattern` shape — that pattern is concatenated onto the
  repo basename to form a sibling dir, not a nested layout. Would
  require new config (`worktree_layout: nested|flat`), new orphan
  scanner logic (recursive), and a place to persist `round-id`
  (a `.bosun/state/round-counter` file).
- **Length.** `repo-bosun-r3/session-1` ≈ 22 chars but adds a path
  separator. Branch/dir divergence (`bosun/session-1` branch vs
  `bosun-r3/session-1` dir basename) is a new cognitive load.

### C. Timestamp + N

- **Format.** `repo-bosun-20260517-205812-1` (UTC `YYYYMMDD-HHMMSS-N`).
- **Human read.** "the round at 20:58 UTC on May 17, session 1" — fully
  self-describing without a lookup. Lexical sort = chronological sort.
- **Tooling fit.** Substitute `20260517-205812-1` for `{N}`. Pattern
  stays `-bosun-{N}`. Glob and config unchanged. Existing orphan
  scanner finds these dirs without modification.
- **Length.** Suffix `-bosun-YYYYMMDD-HHMMSS-N` = 23 chars (N ≤ 99).
  Still well under MAX_PATH for typical parent dirs. Same-second
  retries (rare but possible during `--resume`) would collide — fix by
  taking the timestamp at *init invocation* and persisting it in
  `init.state` so resume reuses the same id.

### D. Round counter (`r{K}-{N}`)

- **Format.** `repo-bosun-r1-1`, `repo-bosun-r2-1`, …
- **Human read.** Shortest readable form; matches the colloquial
  "round 1" / "round 2" language already used in trial findings.
- **Tooling fit.** Substitute `r1-1` for `{N}`. Pattern + glob unchanged.
  Needs a persisted counter (`.bosun/state/round-counter`) — one more
  small piece of mutable state, with a write-during-init contention
  question already solved by `init.lock`.
- **Length.** Suffix `-bosun-rKK-NN` ≈ 12 chars. The most compact
  option.
- **Drawback.** Counter resets if `.bosun/` is deleted; operators
  comparing dirs across deleted-and-recreated state would see `r1`
  collisions between unrelated rounds.

## 3. Recommendation

**Adopt scheme C (timestamp + N).** It uniquely fixes pain points 1–5
without introducing new state files, new config placeholders, or new
layout shapes; the dir name itself answers "which round was that?"
without a lookup; lexical sort gives operators a free chronological
view of cruft on disk; and the implementation reduces to enriching
`WorktreeSuffixForLabel`'s substituted value plus persisting the round
timestamp in `init.state` so `--resume` produces the same path. The
accepted tradeoffs are (a) ~12 more chars of path versus UUID-suffix
and (b) a UTC timestamp that's slightly less talk-friendly than a
counter ("round 3" vs "round 20260517-205812") — both worth it for
zero new state and one-line config compatibility.
