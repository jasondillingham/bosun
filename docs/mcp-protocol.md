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
