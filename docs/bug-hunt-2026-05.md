# Bosun bug hunt — May 2026

Companion to the security audit at `docs/security-audit-2026-05.md`. Same methodology — five parallel Explore agents over distinct bug classes — but pure correctness this time, no security overlap.

## Fix status

| Finding | Status | Severity |
|---|---|---|
| B1 — `/tmp` hardcoded in `mcp.DefaultSocketPath` | **Fixed** (`os.TempDir()`) | High |
| B2 — `/tmp` hardcoded in `cmd_tour.go` | **Fixed** (`os.MkdirTemp("", ...)`) | Medium |
| B3 — `state.Clear` leaves `.heartbeat` behind, Phase 5 #63 regression | **Fixed** | Medium |
| B4 — `web/handlers.go` silently drops encode errors | **Fixed** (logs to stderr) | Medium |
| B5 — `cmd_events.go` silently drops marshal errors | **Fixed** (logs to stderr) | Low |
| B6 — `cmd_config.go` silently drops marshal errors | **Fixed** (emits diagnostic body) | Low |
| B7 — `cmd_show.go` dead nil check on `spawntree.NewStore` | **Fixed** (removed) | Low (cleanup) |
| Windows lock stubs are no-ops | **Open, tracked** (separate project — needs `LockFileEx`) | Medium |
| Concurrent merge of same session — `merges.log` write race | **Open** | Low (git's index.lock provides partial serialization) |
| Test coverage gaps | **Open** (see "Test gaps worth filling" below) | Low to Medium |

## Findings detail

### B1 — `/tmp` hardcoded in MCP fallback socket path

`internal/mcp/server.go:73` used `filepath.Join("/tmp", ...)` for the deeply-nested-repo socket fallback. `/tmp` doesn't exist on Windows, so the fallback would fail to bind. Fixed by switching to `os.TempDir()`, which resolves to `/tmp` on POSIX and `%TEMP%` on Windows.

### B2 — `/tmp` hardcoded in `bosun tour`

`cmd/bosun/cmd_tour.go:65` used `os.MkdirTemp("/tmp", "bosun-tour-")`. Same Windows breakage. Fixed by passing empty string for the dir arg, which makes `MkdirTemp` consult `os.TempDir()` itself.

**Note:** `cmd_init.go:813` also has a `/tmp` hardcode for the remote SSH-tunnel socket path. That one is intentional and correct — the path is on the **remote** docker host (per Phase 3 design, always Linux), not the local one. Resolving via `os.TempDir()` locally would point at the Mac's `/var/folders/...` which doesn't exist on the remote.

### B3 — `state.Clear` leaves `.heartbeat` behind (Phase 5 #63 regression)

`internal/state/state.go:Clear()` used to omit `.heartbeat` and `.attached-pid` from the per-session marker cleanup, with a comment explaining they're "observability" / re-attach state.

The omission was safe before Phase 5 #63. After Phase 5 #63 (heartbeat-as-running fallback in `session.Derive`), a fresh heartbeat is now treated as evidence of liveness. Result: a merged session with a recent heartbeat would re-appear as RUNNING in `bosun status` until cleanup ran.

Fix adds `heartbeat` to the cleanup list. `.attached-pid` stays — the operator may re-attach to the same label and `attach()` rewrites it.

### B4 — Silent JSON encode error in `/api/show`

`internal/web/handlers.go:198` did `_ = enc.Encode(row)`. If encoding failed mid-response (client disconnect, corrupt session shape), the operator's HTTP dashboard would silently get a half-formed reply with no diagnostic. Fixed: encode errors now log to stderr. The status code can't be changed at that point because headers are already flushed, but at least the daemon log shows what happened.

### B5 — Silent marshal error in `bosun events --json`

`cmd/bosun/cmd_events.go:172` returned silently on `json.Marshal` failure inside the print loop. If marshal ever did fail (Go runtime bug, basically), the operator would see their event stream cut off mid-flight with no indication. Fixed: marshal errors log to stderr.

### B6 — Silent marshal error in `bosun config init`

`cmd/bosun/cmd_config.go:429` did `body, _ := json.MarshalIndent(c, ...)`. The Config struct is well-formed standard types, so marshal realistically can't fail, but if it did the generated `.bosun/config.example.json` would be empty — and operators rely on that file as their authoritative reference. Fixed: on failure, body becomes a small diagnostic JSON object naming the error.

### B7 — Dead nil check in `cmd_show.go`

`cmd_show.go:198` wrapped a block in `if tree := spawntree.NewStore(rc.repoRoot); tree != nil {`. `NewStore` always returns `&Store{...}` — the nil check is dead. Cleaned up; behavior is unchanged.

## Open, not fixed

### Windows lock stubs are no-ops

`internal/state/lock_windows.go`, `internal/claims/lock_windows.go`, `internal/init/lock_windows.go`, and `cmd/bosun/mcp_autostart_lock_windows.go` are all no-op stubs with `TODO` comments. POSIX uses `syscall.Flock`; Windows needs `LockFileEx`. Documented gap, known by the codebase, separate project from this bug hunt.

**Impact:** Concurrent `bosun done` invocations or two MCP daemons starting at once can interleave on Windows. Low likelihood on single-user dev machines; higher on shared Windows CI runners.

### Concurrent merge of same session — `merges.log` write race

Two simultaneous `bosun merge session-1` invocations could race the `merges.log` write at `cmd_merge.go:454`. The git `merge --squash` itself is serialized by git's own `index.lock`, so a double-squash is unlikely — the second invocation would fail at the git layer. But the audit log write is unprotected. Low severity; worth a flock around the `appendMergeLog` call if it ever becomes a real problem.

### Agent overcalls (verified safe, listed so they don't get re-flagged)

- **"Watcher goroutine leak in `mcp/server.go:236`"** — agent missed the `defer close(done)` on line 235. Watcher exits cleanly on `<-done` when Serve returns.
- **"Mutex held across flock call in `state.go` / `claims.go`"** — intentional. In-process mutex serializes goroutines; flock serializes processes. Both layers needed for correctness; holding both for the duration is fine.
- **"Flock unlock loses error in `lockfile_unix.go`"** — unlock of a held lock with a valid FD can't realistically fail. The `_ =` is appropriate.
- **"Cross-platform symlink handling for `/tmp` on macOS"** — `internal/proc/detect.go` already canonicalizes via `filepath.EvalSymlinks` to handle the `/tmp → /private/tmp` symlink on macOS. Correctly implemented.
- **"Forward slashes in docker container paths"** — those are paths **inside** the container, which is always Linux even when Docker is on Windows host. Correct.

## Test gaps worth filling

Not bugs per se, but identified by Agent 5 as load-bearing surfaces with insufficient coverage. Track as follow-up work:

1. **Stress test for webhook concurrency** — 50+ webhook defs all timing out; verify goroutine count returns to baseline.
2. **PIPE_BUF boundary test for `usage.Append`** — synthesize a >512-byte Entry on macOS to confirm the documented assumption (or that we catch oversize entries).
3. **Truncated marker file read** — what happens when `state.Read` encounters a half-written `.done` file from a crash mid-write?
4. **Large stdout from custom MCP tool** — does a 50MB stdout from a Phase 5 #61 tool OOM bosun?
5. **`liveness_gate=external` + fresh heartbeat interaction** — does external mode suppress the heartbeat-as-running fallback as intended, or do both fire?

## Methodology note

Five parallel Explore agents covered: concurrency/leaks, state-machine/lifecycle, error-handling/edge-cases, cross-platform, test-coverage gaps. Each was given a focused brief with specific files to inspect and specific bug patterns to look for. Findings were then verified directly against the code — agents overcalled in three places (the watcher-goroutine claim, the mutex-across-flock claim, and the lockfile-unlock-error claim). Verification before believing is mandatory; the report above only includes findings that survived direct inspection.

Out of scope for this pass:
- Performance bugs (no profiling done)
- UX bugs (separate audit)
- Documentation drift (separate pass)
- Race-detector-only issues (covered by `go test -race ./...` already)
