# Bughunt-1 — Findings rollup

**Round:** Bughunt #1 for bosun (establishing audits/ convention)
**Started:** 2026-05-28
**Baseline:** see `bughunt-1-brief.md`

> **Reading note.** In `runlog/*.md`, `PASS` means *"the session didn't time out and the call returned exit 0"* — it does NOT mean "the server gave the correct answer." Cross-check the per-call response in the runlog for any test where correctness matters more than liveness.

Severity scale:
- **CRITICAL** — exploitable RCE / arbitrary file write / trust bypass
- **HIGH** — privilege boundary breach, DoS crashing the daemon, secret leakage, trust bypass
- **MEDIUM** — resource exhaustion within bounds, error-swallowing that masks problems, weak input validation
- **LOW** — quality, races without practical exploit paths, structural leakage

## Rollup

| ID | Severity | Lane | Title | Status |
|---|---|---|---|---|
| F001 | LOW | L1 build invariants | `internal/mcp/server.go:42` hardcodes `const ServerVersion = "0.2.0-alpha"` — wire `serverInfo.version` reports `0.2.0-alpha` while binary is `v0.11.2-0.20260522...`. Stale since the "round-0 foundation" comment. Same shape as Leonard's F001 | confirmed |
| F002 | LOW | L2 caps | `bosun_attach` with `pid=9223372036854775807` (INT64-max) leaks Go-internal JSON unmarshal error (`json: cannot unmarshal number 9223372036854776000,"session":"s... overflows into Go struct field mcp.AttachArgs.pid of type int`). Float64 precision loss + stray frame-character bleed in user-visible error. **Exact shape as Leonard's F006** — class is portable across mcp-go-backed servers | confirmed |
| F003 | LOW | L2 schema | `bosun_check` with `paths=[null]` leaks `<invalid reflect.Value> has type "null", want "string"` — Go reflect internals reach the operator-visible error. **Exact shape as Leonard's F028** | confirmed |
| F004 | LOW | L2 protocol | MCP error responses use `code: 0` — JSON-RPC spec reserves nonzero codes. `tools/call` before `notifications/initialized` returns `{"error":{"code":0,"message":"method \"tools/call\" is invalid during session initialization"}}`. **Exact shape as Leonard's F008/F021** — the mcp-go-SDK spec violation propagates wherever it's used | confirmed |
| F005 | LOW | L2 protocol | Second `initialize` accepted silently within same session — server returns `serverInfo` again, no error, no state reset. **Exact shape as Leonard's F022** | confirmed |
| F006 | LOW | L2 protocol | Duplicate in-flight ids: server emits two responses with the same `id`. **Exact shape as Leonard's F026** — portable class | confirmed |
| F007 | — | (withdrawn) attach hang DoS suspected → **harness bug** | withdrawn |
| F008 | LOW (design) | L2 schema | `bosun_check` accepts `paths: null` (returns `"no conflicts"` / `conflicts:[]`) but rejects `paths: [null]`. The null-vs-empty distinction is inconsistent: `paths=null` silently passes; `paths=[null]` errors. Recommend either reject `paths=null` (matches `required:[paths]` intent) or document equivalence to `paths=[]` | confirmed |
| F009 | **HIGH** | L3 spawn-worktree | `bosun_spawn` parent-liveness gate computes the worktree path with `roundTimestamp=""` — so every session created by the canonical `bosun init` (scheme-C UID-per-worktree, the default since v0.11) can NEVER spawn sub-sessions. Refuses with "no live agent detected" even when an agent is running in the actual worktree. | confirmed |
| F010 | MEDIUM | L3 spawn-worktree | `bosun_spawn` liveness gate uses `proc.Running` (hardcoded `claude / claude-code / code-cli` allowlist) instead of `proc.RunningForCommand(..., cfg.AgentCommand)` — so any repo whose operators set a custom `agent_command` (Ollama wrappers, Docker scripts, the very feature `RunningForCommand` was built for) cannot spawn. `bosun status` correctly uses the cfg-aware variant; the spawn gate diverged. | confirmed |
| F012 | LOW | L3 spawn-worktree | `bosun_spawn`'s schema is `{parent, brief (string), launch (bool)}` — NOT `briefs[]` as the description ("Each `## suffix` heading in the brief…") and the v0.9 spec naming might suggest. Operators reading the description who try `briefs:[{label,body}]` get a wire-level "unexpected additional properties" — without a hint pointing at the singular `brief` field. Cheap-fix: change the description's opening line from "Each `## suffix` heading in the brief" to "Each `## suffix` heading in the `brief` field". | confirmed |
| F013 | MEDIUM | L3 spawn-worktree | `bosun cleanup` does NOT detect when a sub-session's worktree dir has been deleted manually (operator `rm -rf`'d it, or filesystem reaped it under iCloud File Provider). It refuses with `session session-1 has 1 live sub-session(s): session-1.orph` despite `git worktree list` already marking it `prunable`. The `spawntree.SyncWithGit` ghost-prune is not run during this path — operators stuck behind the refusal must either run `bosun cleanup --tree <parent>` (which cascades destructively, see F014) or hand-edit `spawn-tree.json`. | confirmed |
| F014 | MEDIUM | L3 spawn-worktree | `bosun cleanup --tree <parent>` cascade leaves dangling `bosun/<child>` branches behind when the child's worktree was already gone — git can't delete a branch its worktree references; once the worktree was manually removed, cleanup doesn't re-attempt the branch delete. Manual `git branch -D bosun/session-1.orph` is still required after the tree cascade reports success. (Companion UX gripe — also LOW: the `--tree X` flag reaps `X` itself in addition to descendants, despite the cleanup refusal message saying "(cascades)" but using the verb "reap them" referring to children only.) | confirmed |
| F015 | MEDIUM | L3 spawn-worktree | `bosun remove <session>` is **not idempotent**: a second `bosun remove session-1.rn1` on an already-removed session prints `bosun: removed session-1.rn1 (worktree + branch + state)` and exits 0 — falsely reporting that all three artifacts were removed when none of them existed. Operators scripting cleanup pipelines (CI / housekeeping cron) cannot distinguish "I just removed it" from "it was already gone." | confirmed |
| F016 | LOW | L3 spawn-worktree | `bosun_claim` over MCP accepts claims against sessions whose underlying git branch has been renamed away (i.e. no longer exists at `bosun/<label>`). Branch-renamed mid-flight test: branch `bosun/session-1.rn1` renamed to `bosun/session-renamed`, then `bosun_claim {"session":"session-1.rn1","paths":["foo"]}` returns `session-1.rn1 now claims 1 path(s)`. **Severity LOW because** claims are documented as "advisory" and label-keyed by design — accepting a claim against a renamed branch is the same shape as accepting one against any unknown label; just a model-mismatch where operators may expect more validation. | confirmed |
| F017 | LOW | L3 spawn-worktree | Operator-visible error messages from `bosun_spawn` for corrupted `spawn-tree.json` leak the absolute filesystem path (e.g. `read spawn tree: parse /private/tmp/.../.bosun/spawn-tree.json: unexpected end of JSON input`). For a local-only Unix-socket daemon this is acceptable, but if the MCP daemon is ever proxied (web bridge, in-cluster shim) the path discloses the repo's filesystem location to the caller. | confirmed |
| F018 | **HIGH** | Unbounded `bufio.Reader.ReadBytes('\n')` in `transport.go:51` — single attacker connection can pin arbitrary RSS in the bosun MCP daemon by streaming bytes without a trailing newline. Linear, no cap, no timeout. 8 conns × 16 MiB = ~330 MB resident; ~1 GiB / conn is feasible before OOM-kill. | confirmed |
| F019 | **MEDIUM** | Malformed-JSON frames are **silently dropped AND tear down the connection** with no JSON-RPC error response. Per JSON-RPC 2.0 §5 the server MUST emit `code:-32700 Parse error`. Bosun instead severs the conn; the client sees `BrokenPipe` on the next send with no diagnostic. Loses all subsequent in-flight requests on that connection. | confirmed |
| F020 | **MEDIUM** | F007 follow-up — invalid-PID attach (`pid=99999999`, `pid=2`, `pid=INT32_MAX`) writes `.bosun/state/<session>.attached-pid` on disk, which then makes `bosun cleanup` refuse the session with `"skipped — in-progress"` and `bosun status` mark it `CRASHED`. This is the exact "orphan worktrees that bosun cleanup refuses to reap without --force" symptom Bundle E was built to close. Reproducible across multiple sessions in a single sandbox. | confirmed |
| F021 | MEDIUM | UTF-8 BOM-prefix (`\xef\xbb\xbf{...}\n`) on a frame causes the whole frame to be silently dropped — no response, no error, daemon stays up. Sub-case of F019 (any parse error → silent drop + conn tear-down) but worth a separate row because the BOM is a real interop hazard from any client that flushes text-mode output. | confirmed |
| F022 | LOW | Duplicate JSON keys in `tools/call` arguments resolve **last-wins** (Go `encoding/json` default). Bosun's validation gates fire on the last value, so `pid=2,pid=1` correctly hits the pid=1 hard-refuse — no bypass surfaces in current code. Documenting because any future logger / WAF that reads the first value sees a different PID than what bosun acts on; flagged for the design-of-defense-in-depth reviewer. | confirmed |
| F023 | LOW | Deeply nested JSON (100,000+ levels, ~600 KB - 6 MB frame) is silently dropped (sub-case of F019). Daemon stays up; no stack overflow at 1,000,000 levels (Go's growable goroutine stack absorbs it). Worth noting that the *only* symptom is "no response" — operators cannot tell whether the payload was too deep, too long, or malformed. | confirmed |
| F024 | LOW | `mcp__bosun__attach` (Claude Code's namespaced tool form) returns `{"code":-32602,"message":"unknown tool \"mcp__bosun__attach\""}` from bare bosun. Reference: agents wiring against the bare daemon must drop the `mcp__bosun__` prefix; the error message echoes the user-supplied name verbatim, so debugging is straightforward. (Reference record — not a defect.) | reference |
| F025 | **MEDIUM** | JSON-RPC 2.0 §6 **batch frames** (`[{req1},{req2}]` array in one frame) are silently dropped AND tear down the connection. The spec requires servers either to process the batch or return a meaningful per-item error array. Bosun does neither. Same shape as F019 but specifically for a JSON-RPC feature that's part of the protocol contract. | confirmed |
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
| F040 | LOW | side-effect commits on connections that half-close pre-response | confirmed |
| F041 | MEDIUM | removing `mcp.sock` orphans daemon (no auto-detect / no auto-shutdown) | confirmed |
| F042 | MEDIUM | second `bosun mcp` silently steals socket — no inter-process guard | confirmed |
| F043 | LOW | Bundle B timeout overshoots wall clock by `pollInterval` (observation) | confirmed |
| F044 | **HIGH** | No Host-header validation on `bosun serve` — DNS rebinding lets any website the operator visits read `/api/status`, `/api/show/<label>` (including `BOSUN_BRIEF.md` body), and `/api/events` SSE stream | confirmed |
| F045 | MEDIUM | `/api/events` SSE handler has no `ReadTimeout` / `WriteTimeout` / `IdleTimeout`; 64 idle SSE conns saturate `limitListener` and lock out all subsequent users (default `MaxConnections=64`) | confirmed |
| F046 | LOW | `MaxBytesReader` body cap is unreachable code in v0.12 — every handler rejects non-GET methods before the body is read, so the `1 MiB` cap never engages. Defense-in-depth for a future POST handler is fine; flagging so anyone adding one knows the cap is "free" | confirmed |
| F047 | LOW | XSS-via-`'unsafe-inline'`-CSP closure — verified by inspection: `static/index.html` is fully embedded, all session-derived data flows through `escapeHTML()` before `innerHTML`, no Go-side template rendering touches user input. The `'unsafe-inline'` CSP allowance is currently harmless, but the closure should be re-evaluated each time a new HTML sink lands | closure |
| F048 | LOW | No `Permissions-Policy` / `Cross-Origin-Opener-Policy` / `Cross-Origin-Resource-Policy` headers — Bundle C's 4-header set is the floor, not the ceiling; on `--bind` non-loopback these omissions matter more | confirmed |
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

(Lane runs append rows as findings surface — see `runlog/` for full traces.)

---

## Round status

**Bughunt-1 substantively COMPLETE across all 8 designed lanes (L1–L8).**

| Lane | Sub-tests | New findings | Highest severity |
|---|---:|---:|---|
| L1 (build invariants) | 9 | 1 | LOW |
| L2 (MCP cap edges) | ~30 | 7 (F002–F008) | MEDIUM (F007) |
| L3 (spawn / worktree) | 31+ | 8 (F009-F010, F012-F017) | **HIGH** (F009 — spawn broken on default install) |
| L4 (lock / ledger / daemon) | 13 | 4 (F040-F043) | MEDIUM (F041 — `rm` sock orphans daemon) |
| L5 (`bosun serve` HTTP) | ~15 | 5 (F044-F048) | **HIGH** (F044 — DNS rebinding leaks secrets to malicious tab) |
| L6 (MCP protocol fuzz) | 30+ | 8 (F018-F025) | **HIGH** (F018 — unbounded `bufio.ReadBytes` pins arbitrary RSS) |
| L7 (real dogfood) | 12 | 10 (F030-F039) | **HIGH** (F032 — `bosun merge` exit 0 on conflict; F038 — underscore-name silent orphan) |
| L8 (cross-platform / Windows) | 2 runtime + 9 source-audit | 11 (F050-F060) | MEDIUM |

**Findings total: 54.** Severity mix: **0 CRITICAL, 5 HIGH, 20 MEDIUM, 27 LOW** (+ 2 other).

**Highest-ROI fix order:**

1. **L3 F009 (HIGH)** — `bosun_spawn` liveness gate computes wrong worktree path (`roundTimestamp=""`) so every scheme-C session (the v0.11+ default) **cannot spawn**. Complete feature outage on default install. `bosun status`'s `git worktree list`-based resolver is the correct fix model.
2. **L5 F044 (HIGH)** — DNS rebinding via no Host-header validation; confirmed cross-origin leak of `BOSUN_BRIEF.md` (including planted `AWS_SECRET=...`). Bundle C's 4 headers don't defend; needs a Host gate.
3. **L7 F032 (HIGH)** — `bosun merge` exits 0 on conflict (CI scripts get green status on a wedged repo). Two-line fix.
4. **L6 F018 (HIGH)** — Unbounded `bufio.Reader.ReadBytes('\n')` at `transport.go:51` — one connection can pin arbitrary RSS in the daemon. Two-line fix (bounded reader + read deadline).
5. **L7 F038 (HIGH)** — `bosun init session-<word>` creates a fully-functional worktree that `list/status/show/remove/doctor` treat as nonexistent. `session.Derive:232` excludes via `strconv.Atoi`. Silent orphan.
6. **F001 + L7 F037** — stale version constants (`internal/mcp/server.go:42` "0.2.0-alpha"; `cmd/bosun/cmd_debug.go:126` `"dev"`). Same fix class. ~5-line ldflags extension.
7. **F007 + L8 F057** — Bundle E PID-validation gap (non-PID-1 invalid PIDs accepted; Windows PID 4 same shape). Extend hard-refuse list.
8. **L6 cluster (F019, F021, F025)** — JSON-RPC spec compliance: `-32700` on parse error, BOM frames, batch frames. One helper closes all three plus L2's F004/F005/F006.

**Per-lane source files** (audit-format, ready to promote to `bosun/audits/bughunt-1-*.md`):
`findings/L3-findings.md`, `L4-findings.md`, `L5-findings.md`, `L6-findings.md`, `L7-findings.md`, `L8-findings.md`.

**Reusable harness for future bughunts** lives at `/tmp/bosun-redteam/harness/` (mcp_sock.py with the L4-discovered adaptive-timeout patch; rt.sh logging helpers; verdict.py, summarize.py, reconcile.py).
