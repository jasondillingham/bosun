# Bosun MCP — Protocol Notes (v0.2.0-alpha)

This is the in-progress contract for the MCP server shipped in round 0.
Round-1 work (adding more tools, wiring `bosun init --launch` to set the
discovery env var, locking semantics) extends this document.

## Server lifecycle

- `bosun mcp` runs a foreground daemon bound to a Unix socket.
- Default socket: `<repo-root>/.bosun/mcp.sock` (auto-gitignored via the
  `.bosun/` pattern bosun already adds).
- `--socket <path>` overrides the default.
- One server per repo. Multiple agent sessions connect concurrently to
  the same socket; the server shares state across them.
- SIGINT / SIGTERM trigger graceful shutdown — in-flight connections
  drain, the socket file is removed (TODO: the round-0 code doesn't
  remove the socket on exit yet — fix in round 1).

## Discovery contract

Agent sessions discover the socket via the **`BOSUN_MCP_SOCK`**
environment variable. The value is the absolute path to the Unix
socket.

```
BOSUN_MCP_SOCK=/path/to/repo/.bosun/mcp.sock
```

`bosun init --launch` auto-exports this variable for every session it
launches. Resolution order at init time:

1. If `BOSUN_MCP_SOCK` is set in the parent environment and the socket
   accepts connections, reuse it.
2. Otherwise, if `<repo>/.bosun/mcp.pid` names a live process *and* its
   recorded socket accepts connections, reuse that.
3. Otherwise, spawn a detached `bosun mcp` daemon and wait for its
   socket to bind (3s timeout).

Socket-path policy: the default is `<repo>/.bosun/mcp.sock`. If the
repo path is long enough that the in-repo socket would exceed the
~100-byte Unix-domain limit, bosun falls back to
`/tmp/bosun-<sha256(repo_abs_path)[:12]>.sock`. The hash is
deterministic, so the same repo always resolves to the same fallback
socket and reconnects after a restart land on the same address.

Running `bosun mcp` manually also writes the pidfile, so a hand-rolled
daemon is auto-detected by a later `bosun init --launch`.

## Transport

- Newline-delimited JSON-RPC over a Unix socket.
- Each accepted connection becomes one MCP session inside the SDK.
- The bosun server is a single `*mcp.Server` instance; the SDK is
  concurrency-safe across connections (same pattern the SDK's HTTP
  example uses).

## Tools (round 0)

### `bosun_check`

**Description:** Check whether any of the given paths are claimed by
other bosun sessions before you start editing. Returns a list of
conflicts; empty list means safe to proceed.

**Input schema:**

```json
{
  "paths": ["internal/auth/handler.go", "internal/storage/"]
}
```

**Output schema:**

```json
{
  "conflicts": [
    {
      "path": "internal/auth/handler.go",
      "sessions": ["session-2", "session-3"]
    }
  ]
}
```

**Overlap semantics:** matches the filesystem-claims logic in
`internal/claims/`. Equality, directory containment, and (in a future
revision) glob matching all count as overlap. Round 0 ships the first
two; glob support folds in when claims itself unifies its matcher.

## Tools (planned for round 1)

Each is one parallel session's deliverable:

- **`bosun_claim`** (session-1) — declare paths the calling session is
  editing. Writes to `internal/claims/`. Replaces shelling out to
  `bosun claim`.
- **`bosun_release`** (session-1) — explicit release; today this only
  happens at merge time.
- **`bosun_done`** (session-2) — replaces `.bosun/state/<session>.done`
  write. Validates dirty / ahead per the existing `cmd_done.go` logic.
- **`bosun_stuck`** (session-2) — same for STUCK state.
- **`bosun_announce`** (session-3) — push a string into a per-server
  event channel so the operator's `bosun status` (or future TUI) can
  surface "I'm slow but not stuck" type signals.
- **Discovery wiring** (session-4) — `bosun init --launch` auto-starts
  (or attaches to) a `bosun mcp` daemon and exports `BOSUN_MCP_SOCK`
  into each launched session. See the **Discovery contract** section
  above for the resolution order and socket-path policy. ✅ shipped

## `bosun_usage` (Phase 4)

**Description:** Append a turn's token + cost usage to the calling
session's ledger. Agent runtimes are expected to call this after each
LLM round-trip with the cost the provider reported. Bosun aggregates
the ledger into `bosun status` (the COST column), `bosun show` (the
Usage section), and the merge-time round summary.

**Input schema:**

```json
{
  "session":     "session-1",   // or "1" — same canonical-label shortcut other tools accept
  "tokens_in":   1234,           // optional; omit when only total cost is known
  "tokens_out":  567,            // optional
  "cost_usd":    0.0421,         // required
  "model":       "claude-opus-4-7",
  "turn_label":  "auth handler refactor"
}
```

**Output schema:**

```json
{
  "totals": {
    "tokens_in":   8120,
    "tokens_out":  4011,
    "cost_usd":    0.4732,
    "turn_count":  9,
    "last_model":  "claude-opus-4-7"
  }
}
```

**On-disk:** newline-delimited JSON appended to
`.bosun/state/<session>.usage`. Append-only, atomic under PIPE_BUF, no
locking required. Malformed lines are skipped silently by readers — a
corrupt entry can't poison the rest of the ledger.

**Refusal cases:**

- Empty session → user error.
- Session is not bosun-managed (no matching worktree / state) →
  refused. Prevents an off-tree agent from polluting an unrelated
  session's ledger.
- Negative cost_usd or token counts → refused.

## Budget gate (Phase 4)

When `config.usage_budget_usd` is set to a positive dollar amount in
`.bosun/config.json`, `bosun_claim` enforces a per-session ceiling:

- **`cost_usd < 80%` of budget** → claim succeeds, no signal.
- **`cost_usd >= 80%`, `< 100%`** → claim succeeds and the result
  carries `budget_warning` — a short advisory string a wrapper can
  surface to the operator (also appended to the human-readable
  summary line).
- **`cost_usd >= 100%`** → claim **refused** with a user-error message
  pointing at `config.usage_budget_usd` and `bosun done`. The
  intent: stop the agent from opening new claims when it has already
  spent its budget. Already-open claims and in-flight work aren't
  affected; the gate is only on *new* claims.

`0` (the default) means "no limit" — the gate short-circuits before
reading the ledger, so an unconfigured repo pays nothing for the
feature.

The gate is best-effort. If `config.json` fails to parse, or the
usage ledger can't be read, the claim goes through and the gate
silently skips — refusing claims because of a malformed config would
be worse than letting them through.

## Operator-defined tools (Phase 5 #61)

Bosun's MCP server picks up custom tool definitions from
`config.mcp_tools` and registers each alongside the built-in
`bosun_*` tools at startup. Agents discover them via `tools/list`
and call them via `tools/call` exactly like any built-in. Useful
for repo-specific commands (`bosun_lint`, `bosun_test_one`,
`bosun_db_seed`) that don't belong in upstream bosun but should be
agent-callable.

Example config:

```json
{
  "mcp_tools": [
    {
      "name": "bosun_lint",
      "description": "Run the repo's lint script and report issues. Pass args=[\"--fix\"] to auto-fix.",
      "command": ["./scripts/lint.sh"],
      "timeout_seconds": 60
    },
    {
      "name": "bosun_test_one",
      "description": "Run a single test by name. args=[\"TestFoo\"]",
      "command": ["go", "test", "-run"]
    }
  ]
}
```

**Schema:** every operator-defined tool exposes the same input
shape:

```json
{
  "args": ["optional", "positional", "args"]
}
```

Agent-supplied `args` are **appended** to the configured `command`
before exec. No shell interpretation — argv is exec'd directly, so
`; rm -rf /` injected via a brief or web page can't escape into a
shell.

**Output:** stdout is returned as TextContent (the answer the
agent reads). The structured result carries stdout / stderr /
exit_code separately for callers that need them.

**Validation gates (refused at config-load time):**

- `name` must be non-empty and start with `bosun_` (so a custom
  tool can never shadow a tool from a different MCP server the
  agent is wired to).
- `name` must be unique within the list.
- `description` must be non-empty (agents pick tools by
  description; an empty one is unselectable in practice).
- `command` must have at least one entry, with a non-blank `[0]`.
- `timeout_seconds` must be between 0 (default = 30s) and 300s
  (the hard ceiling so a runaway tool can't pin a session).

**Security model:** any operator with write access to
`.bosun/config.json` already has equivalent shell access to the
repo, so a malicious def is no worse than the operator running
that command directly. The guardrails above stop malicious
**input from elsewhere** (a brief from a web page, an agent that
reads an untrusted comment) from escalating into RCE.

## In-container liveness (Phase 5 #63)

When a session's agent runs inside a Docker container — local or
remote, single-host or SSH-tunneled — the host's PID-namespace-bound
proc-scan can't see the in-container PIDs. The attached-PID path is
also broken across namespaces: an in-container PID `1` doesn't
correspond to anything meaningful on the host, and registering it
would either falsely match the host's init or silently never resolve.

The portable signal that crosses the boundary is **`bosun_heartbeat`**.
The MCP server on the host writes the timestamp to
`.bosun/state/<session>.heartbeat`; `session.Derive` treats a
heartbeat newer than `HeartbeatStaleAfter` (5 minutes) as evidence
the session is alive, even when no attached PID or proc-scan match
exists.

Resulting behavior in `bosun status`:

| Liveness evidence | RUNNING column |
|---|---|
| Attached PID is alive | `<pid>` |
| Attached PID is dead | `—` (and state flips to CRASHED) |
| Proc-scan match in worktree | `<pid>` |
| **Only a fresh heartbeat** | `heartbeat` (Phase 5 #63) |
| `liveness_gate=external` mode | `external` |
| Nothing | `—` |

Note the precedence: a dead attached PID still wins over a fresh
heartbeat. The explicit "I crashed" signal is treated as
authoritative — an old heartbeat sitting just inside its TTL
shouldn't mask a real crash.

A reference shim that calls `bosun_heartbeat` from inside a
container every 60s lives at
[`examples/agent-wrappers/in-container-heartbeat.sh`](../examples/agent-wrappers/in-container-heartbeat.sh).
Copy into your image and background from the entrypoint.

## Compatibility with filesystem coordination

The MCP server reads and writes the **same** `.bosun/claims/` and
`.bosun/state/` files that `bosun status`, `bosun cleanup`, and
`bosun merge` operate on. There is no separate MCP-only state.

This means:

- A session that calls `bosun_claim` via MCP is fully visible to
  `bosun status` and to other sessions that read claims directly.
- A session that doesn't connect to MCP and instead runs
  `bosun claim` on the CLI still works.
- Mixed-mode operation (some sessions on MCP, some on CLI) is the
  intended behavior, not a degraded fallback.

## Session identity

The MCP server does not currently tag connections to particular
`session-N` branches. Tool calls that need to record "which session is
doing this" (like `bosun_claim`) will accept the session name as a tool
argument in round 1. Auto-detection from the connecting process's
working directory is a v0.3 polish item.

## What's deliberately out of scope for round 0

- Authentication on the socket — filesystem permissions are the gate.
- Server-side conflict prevention (locking). `bosun_claim` is still
  advisory in round 1; locks come in round 2.
- Persistent connection state. Each connection is independent; if it
  drops, the agent reconnects fresh.
- Streaming responses. MCP supports them; bosun has no use case yet.

## Open questions

These need to be resolved before the round-1 brief is written:

1. Does `bosun_claim` take an explicit `session_name` argument, or do
   we add a `bosun_register(session_name)` handshake at connection
   start?
2. Should `bosun mcp` auto-start when `bosun init --launch` runs, or
   stay manual? (Probably auto, gated by a flag.)
3. Does the server reject calls that target a session that isn't
   currently bosun-managed (i.e. no matching worktree)?
