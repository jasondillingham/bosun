# Audits

Bug-hunt + security-review history for Bosun. Format and discipline mirror
the convention used in [`leonard/audits/`](https://github.com/jasondillingham/leonard/tree/main/audits).
Every CRITICAL and HIGH-severity finding referenced here is expected to be
closed in a versioned fix round before the next minor bump (see top-level
[`RELEASES.md`](../RELEASES.md) and per-bundle commit messages for closure
references).

## Index

| Round | Surface | Consolidated findings | Per-lane |
|---|---|---|---|
| Bughunt #1 | v0.11+aabaf3d post-v0.12 surface; full L1–L8 sweep (cap edges, spawn/worktree, lockfile/ledger, HTTP serve, MCP protocol fuzz, real dogfood UX, cross-platform/Windows) — 54 findings, **5 HIGH** | [`bughunt-1-findings.md`](./bughunt-1-findings.md) | [`brief`](./bughunt-1-brief.md), [`spawn-worktree`](./bughunt-1-spawn-worktree.md), [`lock-ledger`](./bughunt-1-lock-ledger.md), [`http`](./bughunt-1-http.md), [`protocol`](./bughunt-1-protocol.md), [`dogfood`](./bughunt-1-dogfood.md), [`cross-platform`](./bughunt-1-cross-platform.md) |

> _Pre-bughunt-1 history lives in commits + the `v0.X-(round-Y-)?plan.md` docs at the repo root._
> _The v0.12 Bundle A–E + spawn-TOCTOU + Windows-trial commits (2026-05-21 → 2026-05-28) define the threat model
>  that bughunt-1 picked up from._

## How rounds are structured

Each round follows the same shape (matching Leonard's convention):

1. **Brief** — `bughunt-N-brief.md`. What surfaces to audit, what's in scope,
   how lanes are partitioned, harness baseline, prior-round context.
2. **Per-lane findings** — `bughunt-N-<lane>.md` per audit lane. Each file:
   - Severity scale (CRITICAL / HIGH / MEDIUM / LOW) — definitions identical
     across rounds.
   - Rollup table — one row per finding: `ID | Severity | Lane | Title | Status`.
   - Per-finding sections with: Files / Reproducer / Why this severity /
     Fix shape / Discovered.
3. **Consolidated findings** — `bughunt-N-findings.md` aggregates the
   per-lane rollups into one canonical table + appends each detail
   section. This is the single-doc you read first; per-lane files are the
   deep-dives.
4. **Round status** — included as a section in the consolidated doc:
   total findings, severity mix, highest-ROI fix order, lane-by-lane
   sub-test counts, and any explicit closures / open items.

## Severity scale

- **CRITICAL** — exploitable RCE / arbitrary file write / trust bypass
- **HIGH** — privilege boundary breach, DoS crashing the daemon, secret
  leakage cross-origin, trust bypass under known attack classes,
  state corruption that survives recovery commands
- **MEDIUM** — resource exhaustion within bounds, error-swallowing that
  masks problems, weak input validation, silent-accept of inputs that
  should be refused
- **LOW** — quality, races without practical exploit paths, structural
  error-message leakage, future-proofing observations
