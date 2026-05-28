# Lane L6 — MCP protocol fuzz (bosun-specific surface)

**Lane:** L6-protocol (bughunt-1)
**Operator:** L6
**Date:** 2026-05-28
**Baseline:** bosun `aabaf3d` — binary `/tmp/bosun_test` (`v0.11.2-0.20260522171635-aabaf3d7bd25`)
**Target:** `internal/mcp/transport.go` + the schema-validation paths reached via `internal/mcp/tool_*.go`
**Sandbox:** `/tmp/bosun-redteam-L6/test-repo`
**Runlog:** `/tmp/bosun-redteam/runlog/run-2026-05-28-L6-protocol.md`
**Lane script:** `/tmp/bosun-redteam/harness/lanes/L6-protocol.sh`
**Helpers:** `/tmp/bosun-redteam-L6/probe_unbounded_buffer.py`, `/tmp/bosun-redteam-L6/probe_unbounded_concurrent.py`

Severity scale (matches repo rollup):
- **CRITICAL** — exploitable RCE / arbitrary file write / trust bypass
- **HIGH** — daemon crash DoS, privilege boundary breach, session-impersonation
- **MEDIUM** — schema-validation bypass with on-disk side-effect, silent-drop of valid frames, deeply-nested / unbounded DoS
- **LOW** — error message shape, additionalProperties on edge values

## Rollup

| ID | Severity | Title | Status |
|---|---|---|---|
| F018 | **HIGH** | Unbounded `bufio.Reader.ReadBytes('\n')` in `transport.go:51` — single attacker connection can pin arbitrary RSS in the bosun MCP daemon by streaming bytes without a trailing newline. Linear, no cap, no timeout. 8 conns × 16 MiB = ~330 MB resident; ~1 GiB / conn is feasible before OOM-kill. | confirmed |
| F019 | **MEDIUM** | Malformed-JSON frames are **silently dropped AND tear down the connection** with no JSON-RPC error response. Per JSON-RPC 2.0 §5 the server MUST emit `code:-32700 Parse error`. Bosun instead severs the conn; the client sees `BrokenPipe` on the next send with no diagnostic. Loses all subsequent in-flight requests on that connection. | confirmed |
| F020 | **MEDIUM** | F007 follow-up — invalid-PID attach (`pid=99999999`, `pid=2`, `pid=INT32_MAX`) writes `.bosun/state/<session>.attached-pid` on disk, which then makes `bosun cleanup` refuse the session with `"skipped — in-progress"` and `bosun status` mark it `CRASHED`. This is the exact "orphan worktrees that bosun cleanup refuses to reap without --force" symptom Bundle E was built to close. Reproducible across multiple sessions in a single sandbox. | confirmed |
| F021 | MEDIUM | UTF-8 BOM-prefix (`\xef\xbb\xbf{...}\n`) on a frame causes the whole frame to be silently dropped — no response, no error, daemon stays up. Sub-case of F019 (any parse error → silent drop + conn tear-down) but worth a separate row because the BOM is a real interop hazard from any client that flushes text-mode output. | confirmed |
| F022 | LOW | Duplicate JSON keys in `tools/call` arguments resolve **last-wins** (Go `encoding/json` default). Bosun's validation gates fire on the last value, so `pid=2,pid=1` correctly hits the pid=1 hard-refuse — no bypass surfaces in current code. Documenting because any future logger / WAF that reads the first value sees a different PID than what bosun acts on; flagged for the design-of-defense-in-depth reviewer. | confirmed |
| F023 | LOW | Deeply nested JSON (100,000+ levels, ~600 KB - 6 MB frame) is silently dropped (sub-case of F019). Daemon stays up; no stack overflow at 1,000,000 levels (Go's growable goroutine stack absorbs it). Worth noting that the *only* symptom is "no response" — operators cannot tell whether the payload was too deep, too long, or malformed. | confirmed |
| F024 | LOW | `mcp__bosun__attach` (Claude Code's namespaced tool form) returns `{"code":-32602,"message":"unknown tool \"mcp__bosun__attach\""}` from bare bosun. Reference: agents wiring against the bare daemon must drop the `mcp__bosun__` prefix; the error message echoes the user-supplied name verbatim, so debugging is straightforward. (Reference record — not a defect.) | reference |
| F025 | **MEDIUM** | JSON-RPC 2.0 §6 **batch frames** (`[{req1},{req2}]` array in one frame) are silently dropped AND tear down the connection. The spec requires servers either to process the batch or return a meaningful per-item error array. Bosun does neither. Same shape as F019 but specifically for a JSON-RPC feature that's part of the protocol contract. | confirmed |

## Contracts re-verified (no findings)

- **Two frames in one `send()`** — handler correctly framed both (`id=2` and `id=3` both answered). The prior FAIL in the runlog was a harness `recv()`-timeout, not a daemon bug. Re-verified inline.
- **Split frame across two `send()` calls** — bufio.Reader correctly reassembled. id=2 answered.
- **1 MiB single newline-terminated frame** — answered (the harness-side recv just needed longer).
- **20 concurrent `tools/call` on one connection** — all 20 answered, ids preserved (out-of-order is fine per spec — clients correlate by id).
- **Mid-session reconnect** — clean: 3 sequential reconnections each got tools/list back. Per-connection state isolated correctly.
- **Wrong-type fields** (`pid:"two"`, `session:1`) — schema validator emits well-formed error: `validating /properties/pid: type: two has type "string", want "integer"`. Sane error shape.
- **Embedded NUL in `session`** — caught by `ParseLabel`, error message visible (with `\\x00` notation). No process-side leak.
- **JSON-RPC id as float (`2.5`) / id as null** — answered; the SDK reflects the id back unchanged. No coercion bugs.
- **Tool-name wrong case / empty / substring** — uniformly `code:-32602 unknown tool "X"`. Correct JSON-RPC error code.

---

## F018 — HIGH — `transport.go:51` `ReadBytes('\n')` grows unbounded, single-client OOM lever

**Files:**
- `internal/mcp/transport.go:27,45` — `bufio.NewReader(t.conn)` and `bufio.NewReader(r)` constructors with default buffer (4 KiB)
- `internal/mcp/transport.go:50-58` — `Read()` calls `c.r.ReadBytes('\n')` with **no maximum-line guard** and **no read deadline**
- `internal/mcp/server.go:295-304` — `handleConn()` runs the per-connection loop without imposing any inactivity / size limit
- `internal/mcp/server.go:185-220` — `Listen()` and the accept loop have no connection-level resource caps (no `MaxConnections`, no per-conn memory ceiling)

`bufio.Reader.ReadBytes(delim)` is documented in `pkg.go.dev/bufio#Reader.ReadBytes` as:
> ReadBytes reads until the first occurrence of delim in the input, returning a slice containing the data up to and including the delimiter. … If ReadBytes encounters an error before finding a delimiter, it returns the data read before the error and the error itself (often io.EOF).

There is no upper bound on the accumulated slice. The buffer grows as the reader keeps demanding more bytes from the underlying `net.Conn`. An attacker that opens a Unix socket connection and `send(b"A" * N)` without a trailing newline forces the daemon to allocate N bytes of heap inside the bufio reader, with no timeout because the daemon never sets a `SetReadDeadline` on the connection.

**Reproducer.**

Single connection — `/tmp/bosun-redteam-L6/probe_unbounded_buffer.py` (no SHUT_WR; sends N bytes of `A` after a clean initialize handshake):

```
$ python3 probe_unbounded_buffer.py <DAEMON_PID> 67108864
== probe: send 67,108,864 bytes (no newline) to .../mcp.sock ==
  baseline RSS = 11,216 KB
  sent=      6,750,208 RSS=    22,688 KB
  sent=     13,500,416 RSS=    29,472 KB
  sent=     20,250,624 RSS=    36,384 KB
  sent=     27,000,832 RSS=    43,264 KB
  sent=     33,751,040 RSS=    49,808 KB
  sent=     40,501,248 RSS=    56,784 KB
  sent=     47,251,456 RSS=    63,872 KB
  sent=     54,001,664 RSS=    70,672 KB
  sent=     60,751,872 RSS=    77,568 KB
  DONE sent=67,108,864 bytes in 0.1s — RSS=83,952 KB
  post-close RSS=149,680 KB         ← spikes higher AFTER conn close (json decode buffers)
  liveness: 8192 bytes back from tools/list — daemon ALIVE
  final RSS = 150,256 KB
```

Linear 1 KB ≈ 1 KB of RSS, no cap. RSS continues to climb after the connection closes because the SDK's `jsonrpc.DecodeMessage` allocates more buffers during the final (failed) parse pass.

8 parallel attackers × 16 MiB each — `/tmp/bosun-redteam-L6/probe_unbounded_concurrent.py`:

```
$ python3 probe_unbounded_concurrent.py <DAEMON_PID> 8 16777216
== 8 parallel attackers, each sending 16,777,216 bytes (no newline) ==
  baseline RSS = 150,256 KB
  all attackers sent — RSS = 281,120 KB
  liveness: 8192 bytes from tools/list                ← daemon ALIVE
  after-close RSS = 346,832 KB                         ← +200 MB on a 128 MiB input
```

And one 64 MiB *newline-terminated* frame:

```
frame size: 67,108,930 bytes
send took 0.08s
daemon RSS before: 18,464 KB
daemon RSS after send: 112,944 KB
recv: 16343 bytes, ids: [2]                          ← daemon parsed & replied
daemon RSS post-close: 314,848 KB                    ← steady-state ≈ 5× input
```

**Threat model.**

The MCP daemon binds to a Unix socket inside the repo at `0600` perms (L4 confirmed), so only the same UID can connect. The threat surface is:

1. A *local* malicious / runaway process under the operator's UID (the same audience as F002/F003/F007 — anything that can speak to `.bosun/mcp.sock` already has UID-level trust).
2. An MCP-aware agent (Claude Code, Cursor, etc.) on a stuck or corrupted code path that holds the socket open and dumps bytes without a newline. Real-world: an SDK that buffers `print()` output into the socket pipe when the wrong file descriptor was selected. Not adversarial, just buggy.
3. (Future-proofing.) Any future transport that bridges the Unix socket to a remote endpoint — the same shape becomes remotely exploitable on byte 1.

Case (2) is the practical scenario: an operator running 4 bosun sessions across 4 worktrees and one of them goes runaway. The daemon's RSS climbs until `kern.maxfilesperproc` / OOM-killer kicks in. The operator's only feedback is "all my other bosun sessions stopped responding."

Daemon stays *up* in our tests — Go's allocator handled the 314 MB. But:
- 8 conns × 1 GiB each = 8 GiB. On any operator's laptop (16 GiB RAM, typical), that's a swap-thrash / OOM-kill condition.
- macOS `kern.maxproc` won't intervene; this is just RSS growth, not file-descriptor exhaustion.
- The daemon has **no mechanism** to defend against this: no read deadline, no max-line, no max-conn, no per-conn RSS watchdog.

**Why HIGH not MEDIUM.** Compare to L4's F010 (socket lifecycle DoS) and L5's F009 (HTTP DNS rebinding): the F018 attack is a single `send()` away from making the operator's other in-flight sessions stall while the daemon swaps. The mcp-go SDK's official `custom-transport` example has the same shape (no bound on `ReadBytes`), so this is class-of-bug across the ecosystem — but bosun is the long-running daemon and pays the cost. Per F018b proposal below, the fix is two lines.

**Fix shape.**

```go
// transport.go — replace bufio.Reader with a bounded one.
const maxFrameBytes = 16 * 1024 * 1024  // 16 MiB; tools/list is ~30 KB, brief cap = 256 KiB

type connConn struct {
    conn io.Closer
    r    *bufio.Reader
    w    io.Writer
}

func (c *connConn) Read(_ context.Context) (jsonrpc.Message, error) {
    // SetReadDeadline so the bufio.Reader can't pin a goroutine forever.
    if dl, ok := c.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
        _ = dl.SetReadDeadline(time.Now().Add(30 * time.Second))
    }
    line, err := readLineCapped(c.r, maxFrameBytes)
    if err != nil {
        return nil, err
    }
    return jsonrpc.DecodeMessage(line)
}

// readLineCapped is bufio.Reader.ReadBytes('\n') with a hard byte cap.
func readLineCapped(r *bufio.Reader, max int) ([]byte, error) {
    var buf []byte
    for {
        b, err := r.ReadByte()
        if err != nil { return buf, err }
        if b == '\n' { return buf, nil }
        buf = append(buf, b)
        if len(buf) > max {
            return nil, fmt.Errorf("mcp: frame exceeded %d bytes without newline", max)
        }
    }
}
```

Two-line equivalent: cap the bufio buffer size + add a read deadline. The fix is well-trodden territory (every production Go server with framed input does this) — bosun is missing it because the example code it adapted was for the trusted-stdin case.

---

## F019 — MEDIUM — malformed JSON frames silently tear down the connection (no `-32700 Parse error` response)

**Files:**
- `internal/mcp/transport.go:50-58` — `connConn.Read()` returns the decoder error to the SDK runtime
- `github.com/modelcontextprotocol/go-sdk/jsonrpc` `DecodeMessage()` — returns an error for any non-JSON-RPC-2.0 input
- `internal/mcp/server.go:295-304` — `handleConn()` runs `s.mcp.Run(ctx, transport)`; on Read error the SDK closes the connection

JSON-RPC 2.0 §5 *Response object* (and the canonical example in the spec) says:

> When a rpc call encounters an error, the Response Object MUST contain the error member with a value that is a Object … `Parse error -32700: Invalid JSON was received by the server.`

Bosun does the opposite: on a parse error, the daemon **silently severs the connection** — no error response is queued, the conn is just closed, and any in-flight subsequent send on the same socket gets `BrokenPipe`.

**Reproducer.** Bare-`{` frame after a clean handshake:

```python
$ python3 -c "
import socket, json, os, time
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(5); s.connect(os.environ['BOSUN_MCP_SOCK'])
# ... initialize handshake ...
s.sendall(b'{\\n')
time.sleep(0.3)
# Drain — got 0 bytes.
print('after bad frame, recv: 0 bytes')
# Try a good follow-up:
s.sendall(b'{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\\n')
"
after bad frame, recv: 0 bytes
BROKEN PIPE — server killed the conn after bad JSON: [Errno 32] Broken pipe
```

Same shape for: trailing-comma JSON (`{...,}`), garbage text (`this is not json`), BOM-prefix valid JSON (`\xef\xbb\xbf{...}`), missing `jsonrpc` field, wrong `jsonrpc` version, deeply nested JSON (see F023). Daemon stays up across all of them; just the connection dies.

**Impact.**

Per-connection: any client that pipelines several requests on one socket will lose every request after a single malformed frame. That includes the `tools/list` reflection most agents do at startup — a transient encoder bug becomes a "bosun went silent" report.

Observability: the *only* signal to the operator is `BrokenPipe`. The runlog `tail -f /tmp/l6_mcp_clean.log` showed only the boot banner — no log line was emitted for the parse error. From the operator's side: a session just stops talking to bosun, no error, no diagnostic.

**Why MEDIUM not HIGH.** The connection is recoverable (the L4 finding-F011 socket-lifecycle stays intact; opening a new conn works fine). Daemon stays alive. But the silent-drop pattern is the kind of bug that turns into a multi-hour debug session because the operator can't tell which side broke the protocol.

**Fix shape.**

In `connConn.Read()`, swallow `jsonrpc.DecodeMessage` errors and instead emit a synthetic error response on the write side:

```go
func (c *connConn) Read(ctx context.Context) (jsonrpc.Message, error) {
    line, err := c.r.ReadBytes('\n')
    if err != nil { return nil, err }                // real I/O error: fatal, close conn
    msg, derr := jsonrpc.DecodeMessage(line[:len(line)-1])
    if derr != nil {
        // RFC: send -32700 Parse error with id:null, keep connection open.
        _ = c.Write(ctx, makeParseErrorResponse(derr))
        return c.Read(ctx)                          // recurse to next frame
    }
    return msg, nil
}
```

(Recursion is fine in practice — bufio.Reader retains state, the next ReadBytes pulls the next frame.)

---

## F020 — MEDIUM — F007 follow-up: invalid-PID attach commits to disk and locks `bosun cleanup` out of the session

**Files (no fix proposed — F007 is the canonical fix site):**
- `internal/mcp/tool_attach.go:59-72` — `if args.PID <= 0 { ... }` + `if args.PID == 1 { ... }` are the only PID gates
- `internal/mcp/tool_attach.go:119` — `s.state.WriteAttachedPID(label, args.PID)` writes the file
- `internal/session/session.go` / `internal/state/store.go` — `WriteAttachedPID` is byte-identical to `bosun attach --pid N`
- `cmd/bosun/cmd_cleanup.go` — the `skipped — in-progress` path triggers when `attached-pid` exists

L2 promoted F007 from LOW to MEDIUM with the framing *"Bundle E PID-validation gap (pid=2/99M/INT32_MAX accepted)"*. L4 misdiagnosed F007 as a harness bug; it's not. **L6 closes the loop:** the invalid-PID attach has a concrete, operator-visible downstream effect: `bosun cleanup` refuses to reap the session, and `bosun status` shows it as `CRASHED`.

**Reproducer.** From the L6 runlog F7 section, with a fresh `bosun init` 4-session sandbox:

```
$ python3 -c '<send bosun_attach pid=99999999 session=session-1 over MCP>'
response: {"id":2,"result":{"content":[{"type":"text","text":"session-1 attached pid=99999999"}],...}}

$ cat .bosun/state/session-1.attached-pid
99999999

$ bosun status
4 sessions — 3 WORKING, 1 CRASHED · 0 commits ahead total
SESSION    BRANCH           STATE    AHEAD  DIRTY  CLAIMED  RUNNING  LAST_COMMIT
session-1  bosun/session-1  CRASHED  0      0      0        —        —       — (no commits)
session-2  bosun/session-2  WORKING  0      0      0        —        —       — (no commits)
...

$ bosun cleanup --dry-run
  ⏭ session-1: skipped — in-progress
  ▸ session-2: would remove (empty)
  ▸ session-3: would remove (empty)
  ▸ session-4: would remove (empty)
bosun: dry-run — would remove 3, skip 1 (no changes made)
```

Reproduces across **multiple sessions** (verified with session-2 + pid=99999990 in addition to session-1): both get quarantined, both refuse cleanup. With N malicious calls an attacker (or a buggy agent) can lock every session in the worktree out of cleanup without operator intervention.

**Quote from the Bundle E commit message** (per the brief): *"orphan worktrees that bosun cleanup refuses to reap without --force"* — F020 is that exact symptom, triggered by a missing PID-IsAlive check.

**Why MEDIUM.** Operator-visible side-effect (CRASHED state in `bosun status`, cleanup-refused on subsequent runs), no privilege escalation. The fix lives in F007 — add a `proc.IsAlive(pid)` gate in `tool_attach.go` between the `pid == 1` refuse and the `WriteAttachedPID` call. Roughly:

```go
if !proc.IsAlive(args.PID) {
    return errResult(fmt.Errorf("pid %d is not a running process — refuse", args.PID)), AttachResult{}, nil
}
```

Note bosun *already* has `proc.IsAlive` (used by `bosun status` to detect crashed sessions). The MCP path is just missing the gate before-the-fact rather than after-the-fact.

---

## F021 — MEDIUM — UTF-8 BOM-prefix frames silently dropped (sub-case of F019)

**File:** `internal/mcp/transport.go:50-58`

`\xef\xbb\xbf` is the UTF-8 byte-order mark. Many text-mode I/O libraries (Windows PowerShell, some Python stdout encoders, Java `OutputStreamWriter` with default charset) emit it at stream start. JSON-RPC requires bare UTF-8 with no BOM; Go's `encoding/json` rejects BOM-prefixed JSON.

Bosun's `jsonrpc.DecodeMessage` therefore rejects the frame, which routes through F019 → silent drop + connection tear-down.

**Reproducer:**

```python
s.sendall(b'\xef\xbb\xbf' + b'{"jsonrpc":"2.0","id":2,"method":"tools/list"}\n')
# recv: 0 bytes back, daemon stays up, next send BrokenPipe
```

**Why a separate finding (not just a sub-case).** Filing this separately because the affected population is real-world. Any agent built in: PowerShell (`Out-File` default), .NET (Encoding.UTF8 has `useBOM=true`), Java (System.out default in some locales), or a wrapper script that uses `iconv -f UTF-8 -t UTF-8 -c` accidentally prepends a BOM. The class is well-known interop-with-Windows; bosun ignoring it is fine, silently dropping the connection is not.

**Fix.** Either the F019 generic fix above (emit `-32700 Parse error` and keep conn alive) covers it, OR strip a leading BOM in `Read()`:

```go
if bytes.HasPrefix(line, []byte{0xEF, 0xBB, 0xBF}) {
    line = line[3:]
}
```

---

## F022 — LOW — duplicate JSON keys resolve last-wins (`encoding/json` default)

**File:** `internal/mcp/transport.go:57` — `jsonrpc.DecodeMessage` ultimately uses `encoding/json`, which silently overwrites on duplicate keys (last-wins).

**Reproducer:**

```python
$ raw_frame = b'{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bosun_attach","arguments":{"session":"session-1","pid":1,"pid":2}}}\n'
$ # Response: bosun_attach reports "session-1 attached pid=2"
$ # Disk: .bosun/state/session-1.attached-pid → "2"
```

Reverse order (`{"pid":2,"pid":1}`):

```python
$ raw_frame = b'...{"session":"session-3","pid":2,"pid":1}}}\n'
$ # Response: {"isError":true,"text":"pid 1 is init/launchd, not a bosun worker — refuse"}
$ # Disk: .bosun/state/session-3.attached-pid does NOT exist
```

So **bosun's validation gates fire against the last value**. The pid=1 hard-refuse from Bundle E correctly catches the reverse-order case. No bypass surfaces in the current bosun code.

**Why file LOW.** Documenting the behavior so any future defense-in-depth review (e.g. a proxy logger that records the first occurrence of each key, while the daemon acts on the last) can see that the bosun side picks last. JSON-RPC has no normative position on duplicate keys; RFC 8259 §4 lets implementations choose. Filing as a reference record for the defense-in-depth reviewer, not a defect.

---

## F023 — LOW — deeply-nested JSON silently dropped (no stack overflow, sub-case of F019)

**File:** `internal/mcp/transport.go:50-58` — `jsonrpc.DecodeMessage` uses `encoding/json` which has *no fixed depth limit* (recurses on goroutine stack, which grows to ~1 GB on Linux/macOS amd64).

**Reproducer:**

```bash
# 100,000 nesting levels — ~600 KB frame
$ python3 -c "
n = 100000
s = ('{\"x\":' * n) + '1' + ('}' * n)
print('{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\",\"_noise\":' + s + '}')
" | mcp_sock.py raw -
# → only initialize response, no id=2 response. Daemon stays alive.

# 1,000,000 nesting levels — ~6 MB frame
$ ... same shape ...
# → same: silent drop, daemon stays alive, no stack overflow
```

`encoding/json` doesn't blow up; it just rejects the input because `jsonrpc.DecodeMessage` enforces a JSON-RPC 2.0 envelope that this frame doesn't fit (probably the inner `_noise` map can't unmarshal into the expected `params` shape, or schema validation rejects `_noise` as an additionalProperty further down).

In either case, the symptom flows through F019 → silent drop + connection tear-down. No crash.

**Why LOW.** Daemon survives. Documenting because the only signal to the operator is "no response" — the same as F021 (BOM) and the generic F019 — and the same fix closes it.

---

## F024 — Reference — Claude Code namespaced tool form (`mcp__bosun__attach`) returns a clean `code:-32602`

**File:** `internal/mcp/tools.go` / `tool_*.go` registration — tool names are the bare `bosun_*` forms.

When Claude Code (or other MCP-aware harnesses) calls a tool, it namespaces the name as `mcp__<server-name>__<tool-name>` for display, **but the wire-level call to the MCP server should use the bare tool name**. If an agent gets confused and calls the namespaced form against bare bosun, the response is:

```json
{"jsonrpc": "2.0","id": 2,"error": {"code": -32602,"message": "unknown tool \"mcp__bosun__attach\""}}
```

`code:-32602` is JSON-RPC's "Invalid params" — the spec-correct code for "the method is recognized but the params (tool name in `tools/call`) are invalid." Good error shape; the user-supplied name is echoed back which makes debugging easy.

(Same shape for: `bosun_Attach` wrong-case, empty `""`, substring `bosun_a` — all return the same error code with the user-supplied string echoed. No leak, no daemon harm.)

**Why a reference record.** Not a defect. Filing because the brief asked for the bosun-side behavior for this exact form so agents wiring through Claude Code's namespaced-MCP layer have an answer.

---

## F025 — MEDIUM — JSON-RPC 2.0 §6 batch frames silently dropped + connection severed

**Files:** same as F019 — `internal/mcp/transport.go:50-58` + `jsonrpc.DecodeMessage` + SDK runtime.

JSON-RPC 2.0 §6 *Batch* says:

> To send several Request objects at the same time, the Client MAY send an Array filled with Request objects.

And:

> The Server should respond with an Array containing the corresponding Response objects, after all of the batch Request objects have been processed.

Bosun's `Read()` returns each `bufio.Reader.ReadBytes('\n')` to `jsonrpc.DecodeMessage`, which expects a single JSON-RPC message **object**, not an array. The decoder errors out, the SDK treats the error as fatal, and per F019 the connection is silently severed.

**Reproducer.**

```python
batch = b'[{"jsonrpc":"2.0","id":2,"method":"tools/list"},{"jsonrpc":"2.0","id":3,"method":"tools/list"}]\n'
s.sendall(batch)
# → 0 bytes back, daemon stays up, next send BrokenPipe
```

Observed:

```
BATCH: recv 0 bytes, id markers: []
AFTER BATCH: connection killed by server — [Errno 32] Broken pipe
```

**Why a separate finding (vs. just sub-case of F019).** JSON-RPC §6 is a *spec-mandated feature*; the server is supposed to either:
- Process the batch and return an array of responses, OR
- Return a single `code:-32600 Invalid Request` response (id:null) explaining "Batches not supported."

Bosun does neither. Real clients that pipeline (Cursor, some Claude Code paths, internal Anthropic tooling) may emit batch frames. They'll see the same opaque BrokenPipe as F019/F021.

**Why MEDIUM not LOW.** Higher impact than the BOM case because batching is part of the wire-level contract every JSON-RPC client may reasonably assume. The fix is the same as F019 (emit `-32600` for unsupported batch, keep conn open), but worth a separate row because the diagnosis path is different ("my client batches" vs "my client emits a BOM").

---

## Verification record — bufio framer correctness

The brief asked specifically:

- **"Frame split across two `send()` calls"** — bufio.Reader correctly reassembles (id=2 answered). No drops.
- **"Two frames in one `send()`"** — both processed (id=2 AND id=3 answered). The initial lane runlog FAILed this because the harness `recv()` adaptive-timeout exited too early; re-verified inline.
- **"Very large single frame"** — 1 MiB and 64 MiB newline-terminated frames both processed (id=2 answered each). The 64 MiB case observably grows daemon RSS to 300+ MB (relates to F018 but is *not* the unbounded attack — this is a legitimately-framed large frame).
- **"10 concurrent tools/call in one session"** — re-tested with 20 frames in one send: all 20 answered, response order is shuffled (out-of-order is fine per JSON-RPC).
- **"Mid-session reconnect"** — 3 sequential reconnect cycles each got `tools/list` back cleanly. Per-conn state isolation works.

---

## Sandbox + leftover-state notes

- L6 lane left two sessions (session-1 + session-2) in a quarantined state due to the F020 reproducer. `.bosun/state/session-1.attached-pid` = `99999999` and `.bosun/state/session-2.attached-pid` = `99999990`. Cleanup via `bosun cleanup --force` or `rm .bosun/state/session-*.attached-pid` before the next lane reuses the sandbox.
- Daemon binaries: multiple `bosun_test mcp` PIDs lingered after the lane's `restart_daemon` helper due to L4's F011 ("second bosun mcp steals socket"). Cleaned at session end with `pkill -f "bosun_test mcp"`.
- All Python helpers live under `/tmp/bosun-redteam-L6/`; the lane script lives under the campaign harness at `/tmp/bosun-redteam/harness/lanes/L6-protocol.sh`.
