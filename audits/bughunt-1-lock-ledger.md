# Lane L4 — lockfile, ledger, daemon lifecycle

**Lane:** L4-lock-ledger
**Operator:** L4
**Date:** 2026-05-28
**Baseline:** bosun `aabaf3d` (binary `/tmp/bosun_test` reports `v0.11.2-0.20260522171635-aabaf3d7bd25`)
**Sandbox:** `/tmp/bosun-redteam-L4/test-repo`
**Runlog:** `/tmp/bosun-redteam/runlog/run-2026-05-28-L4-lock-ledger.md`
**Lane script:** `/tmp/bosun-redteam/harness/lanes/L4-lock-ledger.sh`
**Helpers:** `/tmp/l4_no_shut_call.py` (no-SHUT_WR MCP client), `/tmp/l4_lock_holder` (Go flock holder), `/tmp/l4_utf8_adversarial.go` + `/tmp/l4_utf8_inspect.go` (offline UTF-8 fuzz)

---

## Severity scale

- **CRITICAL** — exploitable RCE / arbitrary file write / trust bypass
- **HIGH** — privilege boundary breach, DoS crashing the daemon, secret leakage, trust bypass
- **MEDIUM** — resource exhaustion within bounds, error-swallowing that masks problems, weak input validation
- **LOW** — quality, races without practical exploit paths, observation-only

## Rollup

| ID | Severity | Title | Status |
|---|---|---|---|
| F007 | — | (withdrawn) attach hang DoS suspected → **harness bug** | withdrawn |
| F040 | LOW | side-effect commits on connections that half-close pre-response | confirmed |
| F041 | MEDIUM | removing `mcp.sock` orphans daemon (no auto-detect / no auto-shutdown) | confirmed |
| F042 | MEDIUM | second `bosun mcp` silently steals socket — no inter-process guard | confirmed |
| F043 | LOW | Bundle B timeout overshoots wall clock by `pollInterval` (observation) | confirmed |

## Contracts re-verified (no findings)

- **Bundle B (30s bounded lock acquisition)** — externally-held `.bosun/state/.lock` produced a `LockTimeoutError` at 31s with `pid=NNNN ts=...` holder diagnostic surfacing the holder's PID. Daemon recovered cleanly afterward.
- **Bundle B (SIGKILL of holder)** — flock is kernel-tracked; killing the holder mid-wait released the lock within the poll interval (waiter completed in ~3s = 2s sleep + acquire).
- **Bundle D (PIPE_BUF=480 atomic-append ledger)** — 8 concurrent writers × 50 entries each: **400/400 entries written, 400/400 parse as JSON, max line = 246 bytes**. No torn writes.
- **Bundle D (UTF-8 multi-byte at cap boundary)** — adversarial sweep N=140..200 with 3-byte runes, and adversarial both-fields tests with 500×SOH (each expands to 6 ASCII bytes via ``). `encodeLineUnderCap` converges in every case: when a byte-slice cuts mid-rune, `json.Marshal` replaces the partial bytes with `�` (6 ASCII bytes), the result EXPANDS, the over-cap check fails, and the loop runs one more iteration. **Robust — no torn-write risk from this class.**
- **Socket permissions** — `srw-------` (0600, owner-only). Bundle E F003-shape regression intact.
- **EOF before initialize** — daemon handles cleanly, stays up, subsequent `tools/list` works.
- **SIGTERM cleanup** — both `.bosun/mcp.sock` and `.bosun/mcp.pid` removed on graceful shutdown.

---

## F007 — Withdrawn (harness bug, not a daemon hang)

**Original report (L2):** `bosun_attach` with `pid=2`, `pid=99999999`, `pid=2147483647` (INT32_MAX) returned NO RESPONSE within 10s on Darwin. Suspected daemon hang DoS.

**Root cause:** `harness/mcp_sock.py` half-closes the write side (`s.shutdown(SHUT_WR)`) after sending the call frame. This is fine for fast tools like `tools/list` because the response is generated before the server's read pump observes EOF. For tools that go through `findSessionByLabel → session.Derive → c.git.ListWorktrees` (attach, usage, done), the server's read pump sees EOF, the go-mcp-SDK runtime tears down the connection, and the response is never flushed to the client. The **operation itself completes server-side** — the `.attached-pid` file gets written — but the client sees "no response" and the SDK closes the connection.

**Evidence:**

1. `time MCP_TIMEOUT=30 mcp_sock.py call bosun_attach '{"pid":2,...}'` returned in **0.5s** (not 30s — no hang).
2. A custom client that does NOT half-close (`/tmp/l4_no_shut_call.py`, see appendix) gets the response cleanly:
   ```json
   {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"session-1 attached pid=2"}], ...}}
   ```
3. With the harness client (SHUT_WR), the side-effect still commits — `.bosun/state/session-1.attached-pid` contains `2`. The client never learns.
4. Repeated with `pid=99999999` and `pid=2147483647` — identical behavior. Daemon stays healthy throughout (`tools/list` works between probes).

**Resolution:** F007 OPEN → **WITHDRAWN**. Not a daemon hang DoS. The lane uses the no-SHUT_WR helper for all attach/usage/done probes; the SDK's `bosun_check` short-circuit (when called with only `paths`, no session) is unaffected.

The side-effect-without-response shape **is** worth a LOW finding (F040 below) so the next reviewer doesn't chase it again.

---

## F040 — LOW — bosun side-effects commit on connections that half-close pre-response

**Class:** protocol contract / observability

**Files (no fix proposed):**
- `internal/mcp/tool_attach.go:119` — `s.state.WriteAttachedPID(label, args.PID)` runs before the response is queued.
- `internal/mcp/tool_usage.go` — same shape (`usage.Append` then return).
- `internal/mcp/tool_done.go` — same.
- `internal/mcp/transport.go:50-58` — `connConn.Read` returns the read error; the go-mcp-SDK runtime treats this as fatal for the connection.

**Reproducer (this lane, runlog L4a):**
```bash
$ python3 harness/mcp_sock.py call bosun_attach '{"pid":2,"session":"session-1"}'
NO RESPONSE for id=2
[
  {"jsonrpc":"2.0","id":1,"result":{...initialize...}}
]
$ cat .bosun/state/session-1.attached-pid
2          # ← side-effect committed despite client seeing no response
```

**Why LOW:**
- The MCP / JSON-RPC spec does not require the server to keep a connection open past EOF on read. Half-closing pre-response is a client bug.
- An agent using a well-formed client (no half-close) gets the response. The default go-mcp-SDK client doesn't half-close.
- Repeated calls converge — the operator can recover by re-invoking with a correct client.

**Why worth recording:**
- The shape is invisible from the agent's side: the agent's tool call appears to time out (no response), but the state mutates on disk. Re-invocation with a different PID overwrites the prior value — no real corruption, but the agent's mental model is wrong (it thinks the attach failed).
- This is the same class of "successful but un-confirmable side effect" that gives operators heartburn during incidents (was the file written? do I retry?).

**Fix shapes (operator decision):**
1. **Document the contract:** "MCP clients MUST NOT close their write side until they've received a response for every outstanding call." Update `cmd_mcp.go` long-help.
2. **Defer side-effecting writes:** restructure attach/usage/done so the state mutation happens AFTER the response is enqueued. Requires careful ordering — most call sites already are atomic, but `WriteAttachedPID` followed by `errResult` ordering would need testing.
3. **Drain pending writes on EOF:** check whether go-mcp-SDK can be configured to flush the write queue before tearing down on read EOF. Likely upstream-only fix.

(1) is cheapest and matches the JSON-RPC norms.

---

## F041 — MEDIUM — removing `mcp.sock` leaves daemon running but unreachable; no automatic recovery

**Class:** daemon lifecycle / resource leak

**Files:**
- `internal/mcp/server.go:185-220` — `Listen` binds the socket; nothing watches the inode afterward.
- `cmd/bosun/cmd_mcp.go:106-109` — `defer os.Remove(socketPath)` removes the socket on graceful shutdown, but no equivalent runs if the file is removed out-of-band.

**Reproducer (this lane, runlog L4e probe 9):**
```bash
$ ls -la .bosun/mcp.sock
srw-------@ 1 jasondillingham  wheel  0 May 28 14:33 .bosun/mcp.sock
$ rm .bosun/mcp.sock
$ python3 harness/mcp_sock.py list
transport: connect: [Errno 2] No such file or directory
$ ps -p <daemon-pid>
PID   TTY  TIME     CMD
57098 ??   0:00.40  /tmp/bosun_test mcp     # still alive, but unreachable
```

**Observed:** Daemon stays alive forever. `tools/list` returns ENOENT. The kernel-side listening socket inode still exists (held open by the daemon's fd) but no path resolves to it. Operator must `kill` + restart.

**Impact (MEDIUM):**
- A maintenance script that `rm -rf .bosun/` to reset state silently strands the daemon. Subsequent `bosun_check` / `bosun_attach` calls all fail until the operator notices and SIGTERMs.
- Pairs badly with F042 (two-daemon race) — if the operator restarts via `bosun mcp` without first killing the orphan, the orphan stays around consuming memory.
- Not a CRITICAL because there's no privilege boundary breach and no state corruption — just lost reachability.

**Fix shapes:**
1. Add a `Stat` watchdog inside `Serve()`: every ~5s, `os.Lstat` the socket path; if missing or no longer a socket, exit cleanly with `defer os.Remove` running normally.
2. Use `kqueue` (BSD/macOS) or `inotify` (Linux) to watch the socket-file parent dir for unlinks of the socket node. More efficient than polling but adds platform code.
3. Document and accept: "If you `rm` the socket file by hand, restart the daemon." Note in `cmd_mcp.go` long-help.

(1) is the least surprising — it matches the cost-of-recovery the user already expects (a `bosun mcp restart`).

---

## F042 — MEDIUM — no inter-process guard on `bosun mcp`: second daemon silently steals the socket, first becomes a zombie

**Class:** daemon lifecycle / race

**Files:**
- `internal/mcp/server.go:196` — `_ = removeIfSocket(socketPath)` is called UNCONDITIONALLY before `net.Listen` — including when another daemon currently holds the socket inode.
- `cmd/bosun/mcp_autostart.go:53` — the autostart path DOES take `.bosun/mcp.lock` around the spawn dance. But `cmd/bosun/cmd_mcp.go:85-98` (raw `bosun mcp`) does NOT take that lock.
- `cmd/bosun/cmd_mcp.go:106-109` — the doomed `defer os.Remove(socketPath)` of the eventually-killed daemon clobbers whichever daemon currently holds the path.
- **`cmd/bosun/mcp_autostart_test.go:65`** — in-tree corroboration. The comment reads: _"second daemon's Listen() would unlink the first's socket and clobber"_. The codebase already documents the exact failure mode in the test that protects the **autostart** path. This lane confirms the guard isn't enforced on the **raw `bosun mcp`** path — same failure shape, different entry point.

**Reproducer (this lane, runlog L4e probe 10):**
```bash
$ bosun mcp &                       # daemon 1 (PID A)
bosun mcp: listening on .bosun/mcp.sock
$ bosun mcp &                       # daemon 2 (PID B)
bosun mcp: listening on .bosun/mcp.sock     # ← claims to be listening too!
$ ps -p A; ps -p B                  # both alive
$ lsof .bosun/mcp.sock              # daemon-2 holds the current inode
$ kill A                            # daemon 1 dies; its `defer os.Remove` runs
$ ls .bosun/mcp.sock                # → no such file
$ python3 mcp_sock.py list
transport: connect: [Errno 2] No such file or directory
$ ps -p B                           # daemon 2 still alive, but stranded
```

**Mechanism:**
1. Daemon 1 binds `.bosun/mcp.sock` to a listening socket with inode I1. Process holds fd → I1.
2. Daemon 2 starts. `removeIfSocket(socketPath)` unlinks the path (the inode I1 stays — daemon 1 still has it open via fd). `net.Listen("unix", path)` creates a new socket with inode I2 at the same path.
3. New clients now resolve the path → connect to I2 → reach daemon 2. Daemon 1 is orphaned (its fd points to an inode no longer reachable by path).
4. When daemon 1 is killed (SIGTERM during cleanup), its `defer os.Remove(socketPath)` removes the path that now refers to I2. Daemon 2 is now also stranded.

**Impact (MEDIUM):**
- An operator who runs `bosun mcp` twice (in two terminals, or by forgetting to check whether one is already running) ends up with two zombie processes consuming memory and one of them silently losing all subsequent connections.
- The `.bosun/mcp.pid` pidfile exists for exactly this reason — `mcp_autostart.go` checks it before spawning. But raw `bosun mcp` ignores it.
- Worse: the cleanup order is unpredictable. Killing the "good" daemon disables the orphan; killing the "orphan" daemon disables the path that points to the good one. Operators reaping pidfile entries can clobber the wrong daemon.

**Fix shapes (in order of preference):**
1. **Take `.bosun/mcp.lock` around the Listen+Serve block of `cmd_mcp.go`.** Re-uses the existing flock mechanism. Bundle B's 30s bounded acquisition ensures the second invocation waits up to 30s then errors cleanly with the holder diagnostic.
2. **Check `.bosun/mcp.pid` for a live owner before `removeIfSocket`.** If the pidfile names a live process, refuse to start. Cheap defense without changing locking semantics.
3. **Both** — operator-facing message becomes "bosun mcp is already running (PID NNNN); see `bosun mcp status`."

`cmd/bosun/mcp_lifecycle_test.go` already covers SIGTERM cleanup; an equivalent two-daemon test would catch this regression on future Bundle work.

---

## F043 — LOW — observed wall-clock overshoot on lock timeout is dominated by harness overhead, not server-side poll granularity

**Class:** observation / documentation

**Files:**
- `internal/lockfile/lockfile_unix.go:33-51` — the deadline check `if time.Now().After(deadline)` runs AFTER `time.Sleep(pollInterval)`, so the worst-case server-side overshoot is bounded at one `pollInterval` (**50ms** with the current default).
- `internal/lockfile/lockfile.go:33-39` — `DefaultTimeout = 30 * time.Second`, `pollInterval = 50 * time.Millisecond`.

**Reproducer (this lane, runlog L4c):**
```
$ time bosun_attach with state lock held externally for 35s
elapsed=31s
{"text":"write attached-pid: bosun: lock /private/tmp/.../.bosun/state/.lock held by PID 54749 for 31s; timed out after 30s"}
```

The observed wall-clock 31s **= 30000ms timeout + ≤50ms server poll-overshoot + ~1s of test-harness overhead** (python interpreter init, MCP handshake's two `time.sleep` calls totalling 250ms, socket round-trip, JSON-RPC framing, and bash `time` boundaries). The server-side overshoot is bounded at 50ms by construction.

**Why LOW:**
- Already-correct semantics. The poll loop guarantees an upper bound of `DefaultTimeout + pollInterval` on server time-to-error.
- The error-message diagnostic already surfaces both numbers (`held by PID N for 31s; timed out after 30s`), so an operator reading it sees the wall-clock vs server-timeout distinction.

**No fix recommended.** Observation only. Recorded so future reviewers don't read `elapsed=31s` and chase a non-existent 1s drift inside the lockfile package.

---

## Appendix — Helpers built for this lane

### `/tmp/l4_no_shut_call.py`

Python MCP unix-socket client that drives `initialize` + `notifications/initialized` + `tools/call` WITHOUT half-closing the write side. Required because `harness/mcp_sock.py` does `s.shutdown(SHUT_WR)` which causes the go-mcp-SDK server to tear down before slow (Derive-gated) tools finish writing. Reads until an `id=2` response or `MCP_TIMEOUT` (default 15s). Same interface as `mcp_sock.py call`.

### `/tmp/l4_lock_holder` (built from `/tmp/l4_lock_holder.go`)

Go program: `holder <lock-path> [hold-seconds]`. Opens the lock path, takes an exclusive flock, writes a `pid=N ts=...` line into the body (matching the shape `readLockHolder` expects), then sleeps. Used by L4c to exercise Bundle B's externally-held-lock timeout path.

### `/tmp/l4_utf8_adversarial.go`, `/tmp/l4_utf8_inspect.go`

Offline Go reproducers of `encodeLineUnderCap` from `internal/usage/usage.go`. Used to confirm that mid-rune byte slicing is safely handled (json.Marshal expands `�`; the over-cap check runs another iteration). Not part of the bosun build — pure traces.

---

## Cleanup

All daemons started by this lane were killed by trap handlers at script exit. The `/tmp/bosun-redteam-L4/` sandbox can be removed (`rm -rf /tmp/bosun-redteam-L4/`) without affecting bosun itself.

No `.bosun/*.lock` orphans remain. (Verified at lane end via `ls /tmp/bosun-redteam-L4/test-repo/.bosun/state/.lock` — file exists but is releasable.)
