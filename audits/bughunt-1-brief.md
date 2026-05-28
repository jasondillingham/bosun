# Bughunt-1 — brief

**Status:** in progress
**Started:** 2026-05-28
**Baseline:** bosun `aabaf3d` (aabaf3d7bd2572e24a2f185fb1c1bff0c0e8f7a9) — `bosun version v0.11.2-0.20260522171635-aabaf3d7bd25`
**MCP wire identity:** `name=bosun, version=0.2.0-alpha` (stale — see F001)
**Test bench:** `/tmp/bosun-redteam/` (sandbox, not committed); findings will be promoted to `bosun/audits/bughunt-1-*.md`
**Convention:** establishing the audit format for bosun (no prior `audits/` dir; predecessor format was `v0.X-bughunt-plan.md` + fixes shipped in commits)

## Trigger + prior context

The team has been running their own iterative bughunt loop:

- **v0.12 Bundles A-E** (commits `7b044a3..d1864a9`, 2026-05-21) — input caps, file modes, log scrubbing, bounded lock acquisition with holder diagnostics, HTTP serve security headers + body caps, sub-PIPE_BUF atomic-append ledger, bosun_attach PID validation
- **fbb3223** — spawn TOCTOU on per-parent quota closed via `AddChildIfUnderQuota` (independently flagged HIGH by their bug-hunt agent + MEDIUM by their security audit; ranked HIGH conservatively)
- **Windows trials** (commits `a760e2a..aabaf3d`, 2026-05-26..28) — second VNC-console round, NTFS test-infra failures, +2 new findings filed in commits

This round is **bughunt-1 by name only** — it picks up where their 2026-05-21 + Windows loop left off. Goal: find what their threat model hasn't yet covered.

## Methodology

Reusable harness adapted from Leonard's projectdogwalker red-team:

- `harness/mcp_sock.py` — JSON-RPC unix-socket client (`list` / `call` / `raw` / `rawnohandshake`). Bosun's MCP server binds to `.bosun/mcp.sock`, so we speak `AF_UNIX SOCK_STREAM` rather than stdio.
- `harness/rt.sh` — logging helpers (sourced into each lane script). `rt_init` / `rt_section` / `rt_run` / `rt_finding` / `rt_summary`. **Lesson from Leonard L3/L5: `export -f` any helper functions or `bash -c` subshells silently `exit=127`.**
- `harness/verdict.py` — Leonard-specific pretty-printer, here as a template
- Sandbox lives at `/tmp/bosun-redteam/test-repo` because bosun (correctly) refuses to `init` under `~/Documents` (iCloud-synced — issue #15)

## Lanes (8 designed, weighted toward concurrency / lifecycle)

Re-weighted from Leonard's plan per advisor calibration: bosun's primary attack surface is the spawn / lockfile / worktree / ledger surface that the 14 MCP tools wrap, not the MCP layer itself.

| # | Surface | Why this lane |
|---|---|---|
| **L1** | Build invariants & version drift | F001 source already located; cheap, durable |
| **L2** | MCP tool cap edges (incl. Bundle A/D regression check) | Lighter than Leonard's L2 — fewer probes, parity-check the recent caps (oversized briefs, ledger line limits) |
| **L3** | Spawn / worktree state correctness | HEAVY — N-way `bosun_spawn` + concurrent merge/cleanup. Verify `AddChildIfUnderQuota` holds under adversarial load; orphan worktrees; rename / delete races |
| **L4** | Lockfile, ledger, daemon lifecycle | HEAVY — Bundle B (30s bounded lock), Bundle D (PIPE_BUF) regression + extend. Concurrent appenders, hung holder, socket relifecycle |
| **L5** | `bosun serve` HTTP hardening | Verify Bundle C contracts (CSP, body cap, headers on error paths, slowloris). Note CSP allows `'unsafe-inline'` — XSS surface if any user input lands in a template |
| **L6** | `bosun_attach` PID validation | Re-audit Bundle E + extend: PID 2 (kthread), launchd descendants, macOS/Windows no-cwd-check sibling of the PID-1 case |
| **L7** | Real dogfood / TUI / actual workflow | Use bosun to coordinate a real-feeling parallel-session run; capture UX friction |
| **L8** | Windows / cross-platform | Continuation of the team's active Windows trial — extend, don't duplicate |

## Findings format

Mirrors `~/Documents/Homelab/leonard/audits/bughunt-11-findings.md`:

- Severity scale: CRITICAL / HIGH / MEDIUM / LOW (definitions identical to Leonard's — bosun's recent commits use the same scale)
- Rollup table with one row per finding (`ID | Severity | Lane | Title | Status`)
- Per-finding sections with: Files / Reproducer / Why this severity / Fix shape / Discovered
- IDs sequential `F001 ..`
- `bughunt-1-findings.md` = consolidated rollup + all detail sections (single canonical doc)
- Per-lane files (`bughunt-1-<lane>.md`) for substantive deep-dives (L3, L4, L5 likely; L1 will fold into the consolidated)

## Promotion criteria

A finding is "promoted to bosun/audits/" once it has:
1. A reproducer that runs verbatim against this sandbox (or notes platform constraints)
2. Source-file references via repo-relative paths (`internal/X/Y.go:LINE`)
3. A "Fix shape" sized to bosun's scope-discipline rule (smallest change that closes the bug)
