# Bosun security audit — May 2026

Deep review of the bosun codebase as it stands at commit `1caa6fe`, post Phase 5. Five attack surfaces were inspected in parallel by independent agents, then load-bearing findings were verified against the code directly. Findings are ranked by severity, with the highest-impact fixes called out at the top.

## Fix status

| Finding | Status | Commit |
|---|---|---|
| C1 — MCP socket world-accessible | **Fixed** | next commit |
| C2 — Session identity not verified | **Reclassified Low** after C1 (within-operator-trust only; full caller-credential enforcement deferred) | — |
| H1 — Webhook FormatPlain env leak | **Fixed** (env now allowlist-filtered) | next commit |
| H2 — events.log unbounded growth | **Fixed** (rotation at 10MB, keeps 5 copies) | next commit |
| M1–M7, L1–L4 | Open — track separately | — |

## Threat model

Bosun runs as a single local user on their workstation. The trust boundary contains:

- **The operator** — runs the bosun CLI, edits `.bosun/config.json`, writes briefs, defines hooks, defines custom MCP tools. Has full shell access. Anything they configure runs as them by design; misuse is operator error, not a vulnerability.
- **The agent** — connects to the MCP socket from within a session worktree. Can call any MCP tool. Operates on a string `session` argument that it self-asserts. Briefs and `bosun done --message` content are agent-supplied free text.
- **Other local users on the same host** — should NOT be able to spoof the operator's sessions, read their briefs, or alter their state files.
- **External endpoints** (Anthropic API, webhook URLs, remote Docker daemons) — operator-configured, but the response/connection is partially adversarial-shaped (could send oversized bodies, malformed headers).

What's **not** in the threat model:
- Defending the operator from their own malicious config or hooks (those run as the operator anyway).
- Defending against root on the same host.
- Defending against a compromised git repo that's already been cloned.

## Findings, severity-ranked

### CRITICAL

#### C1. MCP Unix socket has world-accessible permissions

**Where:** `internal/mcp/server.go:183` — `net.Listen("unix", socketPath)` with no follow-up `os.Chmod`.

`net.Listen` creates the socket with the process umask (typically `0o022` → socket mode `0o755`-equivalent, which is world-connectable on Linux). The parent dir is `0o750` (good), but the socket file inside has no explicit restriction. Any local process can `connect(2)` to it.

Combined with C2 below, this means **any local user on the host can invoke every MCP tool against any session.**

**Fix:** Call `os.Chmod(socketPath, 0o600)` immediately after `net.Listen`. Or use `syscall.Umask(0o077)` around the Listen call. Either path closes the socket to non-owner users.

#### C2. MCP tools don't verify the caller IS the session they claim to be → **Reclassified Low after C1 fix**

**Where:** every `internal/mcp/tool_*.go` — `bosun_done`, `bosun_claim`, `bosun_attach`, `bosun_usage`, `bosun_spawn`, etc.

Every tool takes `session` as a JSON string argument. The server does **not** verify the connecting process is actually running in that session's worktree. `bosun_done {"session": "session-2"}` from session-1's agent succeeds.

**Post-C1 severity:** With the socket locked to mode `0o600`, the threat surface shrinks from "any local user on the host can spoof any session" to "within the operator's own trust boundary, one of their agents can claim to be another." Since all agents run the operator's `agent_command` and are equally privileged inside the operator's account, this is now a **multi-agent integrity** concern, not a security boundary violation. The operator could trivially achieve the same effect via `bosun done session-2` from their own shell.

**Deferred enforcement design** (for a future PR, not blocking the release):
- Capture peer PID via `SO_PEERCRED` (Linux) / `LOCAL_PEERCRED` (macOS) at connection accept.
- Thread the PID through the MCP SDK context via `context.WithValue`.
- For session-targeted tools, resolve the caller's `/proc/<pid>/cwd` and verify it's inside the claimed session's worktree.
- Best-effort: when peer-cred is unavailable (Windows, edge cases), log a warning and allow — full graceful degradation.

`bosun_spawn` partially gates via `runningFn(worktreePath)` to check the parent's liveness, but that's a different check — it doesn't confirm the *caller* is that parent.

### HIGH

#### H1. `webhooks` FormatPlain leaks the full env map (including agent-controlled `BOSUN_DONE_MESSAGE`) to operator-configured webhook URLs

**Where:** `internal/webhooks/webhooks.go:256-260` — `envelope["env"] = env` dumps the whole map.

The `env` map at the `post-done` callsite contains `BOSUN_DONE_MESSAGE`, which is set from `bosun done --message "..."` — agent-controlled free text. If an agent's done-message contains anything secret-shaped (an API key it found while debugging, a credential surfaced in an error), the FormatPlain webhook POSTs it verbatim to the configured URL.

The Slack/Discord formats only embed selected fields via `buildText()`, so they're safer; FormatPlain is the footgun.

**Fix options:** (a) Filter `env` to a known-safe allowlist before envelope-marshal. (b) Document that FormatPlain may include free-form agent text and warn against pointing it at endpoints that log payloads. (c) Replace `env: full map` with a curated subset (session, status, ahead-count, but not message body).

#### H2. `bosun_announce` events.log grows unbounded

**Where:** `internal/events/` — events.log is append-only with no rotation, unlike `spawn.log` (rotates at 10MB).

An agent calling `bosun_announce` every few seconds will bloat `.bosun/events.log` to GB scale over a long-running session. No size cap, no rotation. Eventually fills the disk.

**Fix:** Add the same rotation pattern used in `internal/mcp/spawn_audit.go` to events.log. 10 MB cap, 5 rotated copies.

### MEDIUM

#### M1. Brief size in `bosun_spawn` is unbounded

**Where:** `internal/mcp/tool_spawn.go` — `BriefMarkdown` arg has no size cap.

An agent (malicious or merely confused) can pass a 100MB brief. The MCP server allocates and parses it. DoS vector.

**Fix:** Cap brief size at e.g. 256KB. Refuse with a clear error message. (256KB is already huge for a markdown brief; real briefs are typically <10KB.)

#### M2. Anthropic API response has no size limit

**Where:** `internal/suggest/claude.go:380` — `raw, err := io.ReadAll(resp.Body)`.

A compromised/proxied Anthropic endpoint could send GB-scale data. Lower likelihood (operator controls which endpoint they hit) but standard defense-in-depth.

**Fix:** `raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))` — 10MB is generous for any sane Claude response.

#### M3. TOCTOU race in `bosun_spawn` concurrent quota

**Where:** `internal/mcp/tool_spawn.go:142-148` — `CountChildren()` reads then `spawn.Run()` writes, not atomically gated.

Two concurrent spawn calls against the same parent can both pass the quota check before either writes. The quota cap is breachable.

**Fix:** Take a lock around the count+spawn pair, or use an atomic increment with rollback on failure.

#### M4. History archive files written with mode `0o644`

**Where:** `internal/history/history.go:115, 137, 171` — `brief.md`, `commits.log`, `metadata.json` are `0o644`.

If a brief contains an inline secret and the host has multiple users, group/world-readable archives leak long-term. Even on single-user hosts, this is the wrong default — history is operator-only debugging data.

**Fix:** Change `0o644` to `0o600` for all archive files.

#### M5. Lock acquisition has no timeout

**Where:** `internal/lockfile/lockfile_unix.go:25` — `syscall.Flock(..., syscall.LOCK_EX)` blocks indefinitely.

A hung process holding a lock pins every subsequent bosun command waiting on it. No deadline, no cancellation.

**Fix:** Wrap in a `select` with `time.After(30 * time.Second)` and refuse with a clear "lock held by PID N for X seconds" message.

#### M6. HTTP serve missing security headers + body size limit

**Where:** `internal/web/server.go` and `internal/web/handlers.go`.

- No `X-Frame-Options`, `X-Content-Type-Options`, `Content-Security-Policy` headers.
- `http.Server` has no `MaxHeaderBytes`.
- Handlers don't wrap request bodies with `http.MaxBytesReader`.

Default bind is `127.0.0.1` (good) and an operator binding to non-loopback gets a warning, so the primary surface is small. But defense-in-depth for the localhost case (e.g., a malicious browser tab making CSRF-ish requests if the operator visits a malicious site while bosun serve is running) needs `X-Frame-Options: DENY` at minimum.

**Fix:** Add a middleware that sets the three headers above and wraps body with `http.MaxBytesReader(w, r.Body, 1<<20)`.

#### M7. PIPE_BUF assumption on macOS for audit/usage lines

**Where:** `internal/usage/usage.go`, `internal/mcp/spawn_audit.go`, `internal/mcp/subtask_audit.go` — atomic-append relies on `write(2)` < `PIPE_BUF`.

`PIPE_BUF` is **512 bytes on macOS**, 4096 on Linux. A single usage entry or audit entry that marshals over 512 bytes can tear across process boundaries on macOS. The comment in `usage.go` acknowledges this but no runtime check enforces it.

**Fix:** Cap the marshaled line at 480 bytes (under the macOS limit with headroom). Truncate `Model` and `TurnLabel` if needed. Add a unit test that synthesizes a max-size entry and confirms the encoded length stays under the cap.

### LOW

#### L1. Anthropic 5xx error body logged verbatim (200-char truncated)

**Where:** `internal/suggest/claude.go:396` — `body: %s` with `truncate(string(raw), 200)`.

If Anthropic ever returns an error response containing the API key or other sensitive field, the first 200 chars get logged. Defensive truncation isn't a redaction. Lower probability but easy to harden.

**Fix:** For non-2xx, log only `status code` and the parsed `error.message`. Drop the raw-body fallback.

#### L2. `bosun_attach` allows arbitrary PIDs

**Where:** `internal/mcp/tool_attach.go:58-59` — only checks `args.PID > 0`.

An agent can register PID 1 (init), PID of `sshd`, etc. as its liveness PID. Doesn't escalate privilege but creates false "RUNNING" claims. The liveness gate then trusts the bogus PID until proc.IsAlive disproves it (which it won't for PID 1).

**Fix:** Validate that the PID's `/proc/<pid>/cwd` (Linux) or equivalent (macOS) is inside the session's worktree. Best-effort — falls back to current behavior on platforms without the check.

#### L3. Custom MCP tools inherit the bosun process env

**Where:** `internal/mcp/tools_custom.go:91` — `exec.CommandContext` with no `cmd.Env` override.

Operator-defined tools see the parent's `ANTHROPIC_API_KEY` and any other secrets in the daemon's env. Documented as expected ("operator trusts their own tools"), but worth surfacing in the docs so operators know not to define a custom tool that hands argv to an unrelated subprocess they don't control.

**Fix:** Add to the Phase 5 #61 docs a note that custom tools inherit the daemon env. No code change unless we want explicit env-scrubbing per def.

#### L4. `BOSUN_BRIEF.md` not in repo's `.gitignore`

**Where:** `.gitignore`.

If an operator runs `git add BOSUN_BRIEF.md` inside a session worktree, the brief (which may contain context/secrets) gets committed. `.bosun/` is gitignored but the brief lives in the worktree root.

**Fix:** Add `BOSUN_BRIEF.md` to `.gitignore` (or document the risk in the brief-writing guide).

### NOTES (verified safe but worth recording)

#### N1. Agent_command shell injection is by design, not a vulnerability

`internal/launcher/launcher.go` and `internal/launcher/docker.go` interpolate `agent_command` into shell strings (Terminal.app AppleScript, cmd.exe `cmd /K`, remote-docker `sh -c`). Agents flagged this as injection, but `agent_command` comes from operator-edited `.bosun/config.json` — the operator is choosing what to exec. There's no third-party trust gap. Document the trust model, don't change the code.

#### N2. `removeIfSocket` is safe against symlink races

`internal/mcp/server.go:286-295` uses `os.Lstat` (doesn't follow symlinks) then `ModeSocket` check before `os.Remove`. A planted symlink at the socket path would show `ModeSymlink`, not `ModeSocket`, and the function correctly refuses to delete it. An agent flagged this as High; verification disproved it.

#### N3. Worktree removal trusts `git worktree list` output

`cmd/bosun/cmd_remove.go:269` calls `os.RemoveAll(worktreePath)` where the path comes from `git worktree list`. A tampered `.git/worktrees/` could redirect the path. But anyone who can write `.git/worktrees/` already controls the repo. Not a real attack vector at the bosun-process trust level.

#### N4. Hooks use `sh -c` by design

`internal/hooks/hooks.go` runs operator-defined commands through `sh`. This is documented and intentional — operators want pipes/redirects/`&&`. The code is appropriately scoped to operator config.

#### N5. TLS verification is good everywhere

No `InsecureSkipVerify: true` anywhere in the codebase. All outbound HTTPS uses system default verification. Keep it that way.

#### N6. Session label sanitization is solid

`internal/session/session.go` `ParseLabel` enforces a strict charset (no `/`, no `..`). Path-traversal via session label is closed off.

#### N7. Claims path normalization is fine

`internal/claims/` stores paths for overlap detection only — never uses them for filesystem operations. Traversal attempts via claim paths don't lead anywhere.

## Recommended fix order

If you want a quick hit-list to prioritize, in dependency order:

1. **C1 + C2 together** (close the MCP socket; once it's owner-only, the session-identity issue is much less severe). One small chmod call closes the worst-case attack surface.
2. **H1** (webhooks env-map leak) — change FormatPlain to a curated subset, or document loudly that done-message content goes to the URL.
3. **H2** (events.log rotation) — copy the spawn_audit rotation pattern.
4. **M1, M2, M3, M5** (DoS vectors) — bound brief size, response size, lock timeout. M3 is a race fix in tool_spawn.
5. **M4** (history archive perms) — one-line change to `0o600`.
6. **M6** (HTTP serve headers + body limit) — small middleware.
7. **M7** (PIPE_BUF) — add a length cap on audit/usage lines.

Lows and notes are documentation and defense-in-depth — handle as scope permits.

## What was checked and found clean

- TLS verification (no InsecureSkipVerify)
- Session label sanitization (no path traversal via labels)
- Claims path handling (no filesystem-op surface)
- `removeIfSocket` symlink behavior (uses Lstat correctly)
- `os.MkdirAll(.bosun/...)` uses `0o750` (group readable, world denied)
- Atomic write semantics for state files (temp + rename)
- Anthropic API key is never logged
- No `eval`-style dynamic code execution anywhere
- No SQL (so no SQL injection surface)
- No template rendering with operator content into HTML (no template-injection surface)

## Out of scope, worth a future pass

- A real penetration test against `bosun serve` with deliberately-malicious browser behavior.
- Race analysis of the spawn-tree updates under high concurrency (this audit only looked at the obvious TOCTOU; deeper invariants need a dedicated review).
- Audit of how `bosun rescue` interacts with corrupted state (rescue is the disaster-recovery path; a malformed state file driving rescue into an unsafe operation would be bad).
- The MCP transport itself (JSON-RPC framing, oversized message handling) — relies on the upstream `go-sdk/mcp` library; worth confirming the SDK has its own size limits.
