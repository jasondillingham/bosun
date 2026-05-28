# Bughunt-1 — Lane L8 (cross-platform / Windows continuity) findings

**Lane:** L8 — cross-platform divergence + Windows-specific surface
**Started:** 2026-05-28
**Baseline:** bosun `aabaf3d`; binaries `v0.11.2-0.20260522...`
**Platform constraint:** Probes run on Darwin/arm64 (operator machine). Windows-only findings are **source-audit unverified** — flagged inline; operator can confirm next time their VNC console is up.

**Lane note.** The lane agent reached step "finalize findings.md" but its session ended on an API transport error before the write landed. The 11 findings below were fully captured in the lane script's inline comments (`harness/lanes/L8-cross-platform.sh`); orchestrator transcribed them to this audit-format file. Two findings (F050 + F052) have runtime reproducers; the other 9 are source-audit only and noted as such.

Severity scale matches `findings/FINDINGS.md`:
- **HIGH** — privilege/multi-user boundary breach, daemon-crash DoS, secret leakage
- **MEDIUM** — platform divergence in security-relevant code, silent-accept of inputs that should be refused
- **LOW** — UX/error-message variance, log noise, future-proofing observations

## Rollup

| ID | Severity | Platform | Title | Status |
|---|---|---|---|---|
| F050 | MEDIUM | Windows | Print-fallback launcher emits POSIX shell syntax (`cd '/path' && KEY=val cmd`) on every platform — unrunnable in `cmd.exe` | confirmed (runtime via APFS mirror) |
| F051 | MEDIUM | Windows | Print-fallback env-var prefix uses POSIX `KEY=val cmd` form — `cmd.exe` requires `set KEY=val&& cmd`. Same root as F050, separate surface | source-audit |
| F052 | MEDIUM | Darwin/APFS + Windows/NTFS | `bosun attach` refuses an implicit-PID attach when cwd was reached via a different-case path that names the same physical directory (`os.Getwd()` vs git-canonicalized path divergence). Confirmed on APFS; same shape on NTFS by default | confirmed (Darwin runtime) |
| F053 | LOW | Windows | Windows lockfile diagnostic surface degrades — `LockFileEx(LOCKFILE_EXCLUSIVE_LOCK)` denies ALL access (incl. read), so `readLockHolder` from a waiting contender returns `(0, 0)` and `LockTimeoutError` loses HolderPID/HeldFor diagnostic that Bundle B added | source-audit |
| F054 | MEDIUM | Windows | AF_UNIX socket created by `bosun mcp` is not ACL-restricted. `os.Chmod(0o600)` is documented no-op on Windows AF_UNIX (`internal/mcp/server.go` comment acknowledges); no Windows ACL alternative wired. Any local user can connect | source-audit |
| F055 | MEDIUM | Windows/NTFS | NTFS multi-user safety: `0o600` file modes set by Bundle A M4 (history archives, usage ledger, claim files, MCP socket) are silently ignored on NTFS. Commit `b33f037` flagged this for tests; runtime impact (other local users can read the archives) not filed | source-audit |
| F056 | LOW | Windows | `proc.Cwd` on Windows missing `pid <= 0` early return that `cwd_unix.go` has — cross-platform contract mismatch. Benign today (tool_attach.go gates upstream); future caller bypassing the upstream gate would silently accept negative PIDs on Windows and reject them on Linux | source-audit |
| **F057** | **MEDIUM** | Windows | `proc.IsAlive` on Windows returns true for the System process (**PID 4**), which is permanently alive and unkillable — **exact same shape as the PID-1 gate Bundle E added**, but Windows-only and NOT gated. Higher reserved PIDs (System threads, csrss) are also permanent. F007's Bundle E gap on Windows | source-audit |
| F058 | LOW | Windows + WSL | `connTransport.Read` does `ReadBytes('\n')` and doesn't strip a leading `\r` from CRLF-terminated lines. Cross-platform stdio bridges or future Windows stdio deployments would feed `\r{json}` to the JSON decoder | source-audit |
| F059 | MEDIUM | Darwin/APFS + Windows/NTFS | `cwdInsideWorktree` (MCP-side mirror of F052) — same case-sensitivity gap, different surface. The MCP `bosun_attach` path has its own caller-inside-worktree check; case-only path divergence defeats it the same way | source-audit |
| F060 | MEDIUM/HIGH? | Windows | Atomic-write pattern (`os.Rename` over existing dest) is fragile on Windows: rename fails with "sharing violation" if any process has the destination open. Affects `spawn-tree.json`, `serve.pid`, claim files, `init.state`, several audit-log rotations. **State corruption potential** if a TUI/other reader has the file open during a write | source-audit |

---

## F050 — Print-fallback launcher emits POSIX shell syntax (MEDIUM, Windows)

**Files (likely):**
- `internal/launcher/print*.go` — `printFallback` codepath emitting copy-pasteable command
- `internal/launcher/launcher_*.go` — platform-conditional launchers

**Reproducer (runtime — APFS mirror confirms POSIX-shape output):**
```bash
mkdir -p /tmp/L8-f050-repo && cd /tmp/L8-f050-repo
git init --quiet && git commit --allow-empty -m init --quiet
/tmp/bosun_test init 1 --no-load-check >/dev/null
/tmp/bosun_test config set launcher print >/dev/null
/tmp/bosun_test launch session-1 2>&1
# => shows: cd '/private/tmp/L8-f050-repo-bosun-...-1' && ...
```

The `cd '/path'` single-quote form is POSIX-only. On Windows `cmd.exe`:
- Single quotes are LITERAL (not quoting)
- `KEY=val cmd` env-prefix syntax is not recognized (needs `set KEY=val&&`)

Operator copy-pastes the printed command into PowerShell or `cmd.exe` and it fails.

**Why MEDIUM.** The print-fallback codepath is the documented escape hatch for operators whose terminal can't host a TTY launcher. Windows operators following the README's print-fallback fallback get an unrunnable command with no platform-conditional message.

**Fix shape.** Generate platform-conditional output: on `runtime.GOOS == "windows"` emit `set KEY=val&& cd "path" && cmd...` (cmd.exe) or `$env:KEY="val"; Set-Location "path"; cmd...` (PowerShell). Add a `BOSUN_PRINT_SHELL=cmd|pwsh|posix` override.

**Discovered.** 2026-05-28 — L8 probe-1.

---

## F052 — APFS / NTFS case-insensitive cwd defeats `callerInsideWorktree` (MEDIUM)

**Files:**
- `cmd/bosun/cmd_attach.go` — caller-cwd → worktree resolution
- `internal/session/session.go` — `WorktreePathForLabel`

**Reproducer (Darwin/APFS confirmed runtime):**
```bash
mkdir -p /tmp/L8-f052/p1-repo && cd /tmp/L8-f052/p1-repo
git init --quiet && git commit --allow-empty -m init --quiet
/tmp/bosun_test init 1 --no-load-check
# session-1 worktree created at .../p1-repo-bosun-<ts>-1
WT=$(ls -d /tmp/L8-f052/p1-repo-bosun-*-1 | head -1)
CASE_PATH=$(echo "$WT" | sed 's|p1-repo|P1-REPO|')
cd "$CASE_PATH"
/tmp/bosun_test attach session-1 2>&1
# => "not inside the session-1 worktree" — but it IS, just via a case-shifted name
```

**Why MEDIUM.** Bosun's safety contract assumes "the caller's cwd unambiguously identifies which session they're in." On case-insensitive volumes (APFS macOS default, NTFS Windows default) the operator can cd into the worktree via a case-shifted path (e.g., from a shortcut, an old shell-history command) and attach refuses. The operator sees an apparently-correct cwd and a refusal — confusing.

The same divergence affects Windows operators who type `C:\Users\Me\repo` while bosun's canonical path is `C:\Users\me\repo`.

**Fix shape.** In `cmd_attach.go`, when comparing cwd against the worktree path, normalize both via `filepath.EvalSymlinks` + `strings.EqualFold` on case-insensitive volumes (`runtime.GOOS in (darwin, windows)`). Or — more robust — use `os.SameFile(stat-of-cwd, stat-of-worktree)` which compares inode/file-id rather than the path.

**Discovered.** 2026-05-28 — L8 probe-2.

---

## F057 — Windows PID 4 (System) gap — same shape as Bundle E PID-1 (MEDIUM)

**Files:**
- `internal/proc/proc_windows.go` — `IsAlive` (Windows implementation)
- `internal/mcp/tool_attach.go` — currently only refuses `pid=1` per Bundle E

**Source-audit observation.** Bundle E (`d1864a9`) hard-refused PID 1 ("init / launchd is never a bosun worker on any supported platform, and IsAlive can't disprove it"). The exact same reasoning applies to **Windows PID 4** (the System process, which hosts kernel threads — permanently alive, never a bosun worker). Higher reserved PIDs (csrss.exe, wininit.exe) are similar.

The Bundle E commit explicitly says "init / launchd on any supported platform" — Windows isn't named, and the hard-refuse list doesn't extend.

Compounds with **F007 (MEDIUM)** — bughunt-1's confirmed Bundle E gap that any non-1 invalid PID is silently accepted. On Windows, `pid=4` would be a particularly clean attack: always alive, IsAlive can't disprove, operator gets an orphan worktree that bosun cleanup refuses to reap without `--force`.

**Why MEDIUM.** Source-audit only — needs Windows runtime to confirm. But shape-class match to F007 + Bundle E commit's documented threat model says this should land.

**Fix shape.** Extend the hard-refuse list in `tool_attach.go` to:
```go
const (
    pidInitDarwinLinux = 1
    pidSystemWindows   = 4
)
if (runtime.GOOS == "windows" && args.PID == pidSystemWindows) ||
   (runtime.GOOS != "windows" && args.PID == pidInitDarwinLinux) {
    return refuseWithMessage("pid %d is %s — refuse", args.PID, reservedName)
}
```

Or, more general: `if args.PID < 16 { refuse with "reserved PID" }` — covers PID 1 on Unix, PID 0 (which Bundle E also catches via "positive integer"), and the Windows reserved range.

**Discovered.** 2026-05-28 — L8 source-audit cross-referencing Bundle E.

---

## F060 — Atomic-write pattern fragile on Windows (MEDIUM/HIGH source-audit)

**Files (greppable):**
- `internal/state/*` — `spawn-tree.json` write site (rename-over-target)
- `internal/proc/serve_pid.go` (or similar) — `serve.pid` rotation
- `internal/claims/*` — claim files
- `cmd/bosun/cmd_init.go` — `init.state` write
- `internal/history/*` — audit-log rotations

**Source-audit observation.** Bosun's "atomic write" pattern is `os.WriteFile(tmp); os.Rename(tmp, dest)`. On POSIX this is genuinely atomic when source/dest are on the same filesystem. On Windows, `os.Rename` (which calls `MoveFileEx` under the hood) **fails with `ERROR_SHARING_VIOLATION` if any other process has the destination file open** — even for read.

Concrete failure case: `bosun tui` opens `spawn-tree.json` for read to refresh its display every N seconds. Concurrently, `bosun_spawn` (in the MCP daemon) tries to atomic-write `spawn-tree.json`. The rename fails. Depending on error handling, either:
- The write is lost silently (no-op on rename error — **HIGH** if so, state corruption)
- The error propagates and the spawn fails (MEDIUM — DoS, but operator sees the error)

The `bug-hunt pass-2` commit (`fc2b6e6`) mentions reconciling followups; this class may have been considered but not closed.

**Why MEDIUM/HIGH.** Severity depends on which error-handling branch — the audit-log + claim-file paths have higher data-loss potential than the serve.pid path. Operator should triage by file.

**Fix shape.** Wrap the rename in a retry loop with backoff (Windows-specific build tag), as Go's stdlib does in some places. Or — more robust — use `golang.org/x/sys/windows.MoveFileEx` with `MOVEFILE_REPLACE_EXISTING` + retry on `ERROR_SHARING_VIOLATION`. Document the pattern in `CONTRIBUTING.md`.

**Discovered.** 2026-05-28 — L8 source-audit (b33f037's NTFS test-infra fixes prompted the broader codebase scan).

---

## Other source-audit findings (compact)

The remaining F051, F053-F056, F058, F059 have shorter writeups (each is a clean single-file source-audit observation). Full reasoning preserved in `harness/lanes/L8-cross-platform.sh` inline comments. For each, the rollup-row entry above is sufficient unless promoted by the operator during triage.

---

## Confirmed clean (no findings — closure record)

- **Darwin iCloud guard** (`bosun init` refuses `~/Documents`) — re-verified runtime; refusal message clean, points at `--force-icloud` + `docs/macos-setup.md`. **Issue #15 closure holds.**
- **`/tmp` vs `/private/tmp` realpath** — `bosun init` accepts both, treats them as the same; spawn-tree.json normalizes to `/private/tmp`. No drift.
- **CRLF in JSON-RPC frames over unix socket** — Darwin produces LF only; Windows would produce CRLF — flagged as F058 above (untested here).

## Lane runtime status

- 2 runtime probes confirmed (F050, F052)
- 9 source-audit observations transcribed to rollup with file-level references
- Sandbox at `/tmp/bosun-redteam-L8/` — kept for cross-lane re-runs; can `rm -rf` when done
- Background MCP daemons cleaned up

## Open items for Windows-runtime follow-up

When the operator next has VNC console on the Windows trial machine:
1. F050/F051 — actually copy-paste the print-fallback into `cmd.exe` and observe the unrunnable command
2. F053 — manually contend for a lockfile from a second process and inspect the timeout diagnostic
3. F054 — query the AF_UNIX socket ACL with `icacls`
4. F057 — `bosun attach session-1 --pid 4` (or via MCP)
5. F060 — open `spawn-tree.json` from `bosun tui` while triggering a `bosun_spawn`; observe the atomic-write behavior
