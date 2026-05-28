# Lane L5 — `bosun serve` HTTP hardening — findings

**Lane:** L5-http (bughunt-1)
**Date:** 2026-05-28
**Baseline:** bosun at SHA `aabaf3d`, binary `/tmp/bosun_test`
**Target:** v0.12 Bundle C — `internal/web/server.go` security middleware
**Sandbox:** `/tmp/bosun-redteam-L5/test-repo` on `127.0.0.1:18080`
**Runlog:** `/tmp/bosun-redteam/runlog/run-2026-05-28-L5-http.md`

> Severity scale matches the project rollup: CRITICAL / HIGH / MEDIUM / LOW.

## Rollup

| ID | Severity | Title | Status |
|---|---|---|---|
| F044 | **HIGH** | No Host-header validation on `bosun serve` — DNS rebinding lets any website the operator visits read `/api/status`, `/api/show/<label>` (including `BOSUN_BRIEF.md` body), and `/api/events` SSE stream | confirmed |
| F045 | MEDIUM | `/api/events` SSE handler has no `ReadTimeout` / `WriteTimeout` / `IdleTimeout`; 64 idle SSE conns saturate `limitListener` and lock out all subsequent users (default `MaxConnections=64`) | confirmed |
| F046 | LOW | `MaxBytesReader` body cap is unreachable code in v0.12 — every handler rejects non-GET methods before the body is read, so the `1 MiB` cap never engages. Defense-in-depth for a future POST handler is fine; flagging so anyone adding one knows the cap is "free" | confirmed |
| F047 | LOW | XSS-via-`'unsafe-inline'`-CSP closure — verified by inspection: `static/index.html` is fully embedded, all session-derived data flows through `escapeHTML()` before `innerHTML`, no Go-side template rendering touches user input. The `'unsafe-inline'` CSP allowance is currently harmless, but the closure should be re-evaluated each time a new HTML sink lands | closure |
| F048 | LOW | No `Permissions-Policy` / `Cross-Origin-Opener-Policy` / `Cross-Origin-Resource-Policy` headers — Bundle C's 4-header set is the floor, not the ceiling; on `--bind` non-loopback these omissions matter more | confirmed |

---

## F044 — HIGH — DNS rebinding: `bosun serve` accepts any `Host:` header

**Files:**
- `internal/web/server.go:97-107` (`securityHeaders` middleware — no Host check)
- `internal/web/server.go:176-187` (`Start` — binds 127.0.0.1 by default; no `--allowed-hosts` flag)
- `internal/web/handlers.go:43,58,61,62` (every mux registration; no per-handler Host gate)

**Observed.** Against the sandbox at `http://127.0.0.1:18080`:

```
$ curl -sS -H "Host: evil.com" http://127.0.0.1:18080/api/status
HTTP/1.1 200 OK
...
{ "sessions": [ { "name": "session-1", ... "path": "/private/tmp/.../session-1", ... } ] }

$ curl -sS -H "Host: attacker.com:18080" http://127.0.0.1:18080/api/show/session-1
HTTP/1.1 200 OK
...
{ "name": "session-1", ..., "brief": "# Secret brief content\n\nAWS_SECRET=sk-test-AKIA1234567890BLEED\nworktree absolute path leak: /home/operator/projects/secret-client\n" }
```

Also reproduced with `Host: 127.0.0.1.nip.io`, `Host: evil.com` on `/` (returns the dashboard HTML), and `Host: evil.com` on `/api/events` (SSE stream including event-log records).

**Threat model.** Bundle C's commentary calls out "the malicious-browser-tab vector for an operator who hits `bosun serve` while another tab is open" as the primary attack the headers defend against. The 4 headers + `frame-ancestors 'none'` defeat *iframe-based* same-origin abuse, but they do **not** defeat **DNS rebinding**:

1. Operator runs `bosun serve` on `127.0.0.1:8765` (default).
2. Operator opens any browser tab, visits `https://innocent-looking.example/`.
3. That site's DNS record initially resolves to a benign IP (long TTL bypass via subdomain rotation), then re-resolves `attacker-rebind.example` → `127.0.0.1` (low TTL).
4. Top-level JS on `attacker-rebind.example` issues `fetch('http://attacker-rebind.example:8765/api/show/session-1')`. The browser treats the origin as `attacker-rebind.example` (so SOP allows the read), but the request lands on the operator's loopback bosun server because the IP is now `127.0.0.1`.
5. Bosun's mux ignores the `Host:` header entirely and returns the JSON, including:
   - Every worktree absolute path (filesystem layout leak)
   - Every session's claimed paths
   - Every session's `BOSUN_BRIEF.md` body — operators put plans, file paths, ad-hoc context, and (per F044's planted payload) potentially secrets/credentials in briefs
   - The `events.log` stream via `/api/events` — confirmed by SSE capture under `Host: evil.com`:
     ```
     $ curl -sS --max-time 3 -H "Host: evil.com" -H "Accept: text/event-stream" http://127.0.0.1:18080/api/events
     data: {"session":"session-1","kind":"claim","message":"session-1 claimed README.md ..."}

     data: {"session":"session-1","kind":"brief","message":"AWS_SECRET=sk-test-AKIA1234567890BLEED in brief"}
     ```
     The backfill replays the last 20 events with no Host check, and the live poll keeps streaming. A cross-origin reader (DNS rebinding) gets an open firehose into the operator's session activity.

`X-Frame-Options: DENY` and `frame-ancestors 'none'` do not help — this is a top-level navigation + `fetch()`, not an iframe. The browser's CSP applies to the *response* document, not to the cross-origin reader. The four Bundle C headers are about *bosun-served pages being safe to render*; they say nothing about *who is allowed to ask bosun for them*.

**Why HIGH.** The leaked surface is exactly the data bosun is supposed to keep on the operator's machine. `BOSUN_BRIEF.md` routinely carries pre-merge thoughts, file paths to private clients, sometimes credentials. The leak is one drive-by visit away — no operator action needed beyond keeping `bosun serve` running, which the dashboard is *designed* for. The default bind is loopback so the attacker can't reach the server at the network layer, but DNS rebinding is the textbook bypass for "loopback-only services."

**Fix shape.** Standard rebinding mitigation: enforce a Host-header allowlist in `securityHeaders` middleware (or a dedicated `hostGuard` middleware), keyed off `s.cfg.Bind`:

- When `Bind` is a loopback address (`127.0.0.1`, `::1`, `localhost`), accept Host headers only for `127.0.0.1[:port]`, `[::1][:port]`, `localhost[:port]`. Reject everything else with `421 Misdirected Request`.
- When `Bind` is non-loopback, require Host to match the configured bind or a `--allowed-host` repeated flag.

Sample code-skeleton:

```go
func hostGuard(allowed map[string]struct{}, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        h := r.Host
        if i := strings.IndexByte(h, ':'); i >= 0 {
            h = h[:i]
        }
        if _, ok := allowed[strings.ToLower(h)]; !ok {
            http.Error(w, "misdirected host", http.StatusMisdirectedRequest)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

Add a `server_test.go` regression that hits `/api/status` with `Host: evil.com` and expects `421`.

**Discovered.** 2026-05-28 — Lane L5 first probe.

---

## F045 — MEDIUM — Slowloris-style DoS via 64 idle SSE conns (no `ReadTimeout`/`IdleTimeout`)

**Files:**
- `internal/web/server.go:211-215` (`http.Server` only sets `ReadHeaderTimeout: 10s` and `MaxHeaderBytes`; no `ReadTimeout`, `WriteTimeout`, `IdleTimeout`)
- `internal/web/server.go:128-163` (`limitListener` — `MaxConnections` defaults to 64)
- `internal/web/events.go:30-67` (SSE handler holds the conn until `r.Context().Done()` — only cancelled by client disconnect or `ctx` shutdown)

**Observed.** Repro:

```
python3 <<'EOF'
import socket
conns = []
for i in range(64):
    s = socket.socket(); s.connect(("127.0.0.1", 18080))
    s.sendall(b"GET /api/events HTTP/1.1\r\nHost: localhost\r\nAccept: text/event-stream\r\n\r\n")
    conns.append(s)
# 65th connection — should fail or stall
s = socket.socket(); s.settimeout(3.0)
s.connect(("127.0.0.1", 18080))
s.sendall(b"GET /api/status HTTP/1.1\r\nHost: localhost\r\n\r\n")
s.recv(2048)  # times out
EOF
```

Result: the 65th connection sits in the kernel SYN queue indefinitely — `Accept()` never returns it because the `limitListener` semaphore is full of the 64 SSE conns. A legitimate user (or the operator's own browser tab refresh) gets no response. Recovery requires either the attacker disconnecting or `bosun serve` restart.

The advisor flagged this as the realistic post-Bundle-C DoS path. Confirmed in `~3s` against the running sandbox.

**Why MEDIUM.** Easy to exploit (one Python script, ~10 lines), no auth required, recovery requires operator intervention. Not HIGH because (a) loopback-only by default, so the attacker needs local code execution OR DNS rebinding (F044) to reach the port, and (b) the cap is configurable — operators who notice can `--max-connections 256` as a stopgap. The deeper issue is that **none of the server's read/write/idle timeouts will close a misbehaving client** — `ReadHeaderTimeout: 10s` only fires before headers complete; once the SSE handler is in the read-loop, it never times out a writer or reader on its own.

**Fix shape — load-bearing change first, then defense-in-depth:**

1. **Per-handler SSE cap, separate from total conn cap.** Track active SSE streams in a counter; reject new `/api/events` conns over a small cap (e.g. 8 concurrent). A normal operator opens 1–2 SSE conns per tab; 8 is generous; 64 SSE conns is always abuse. This is the actual fix — it stops the attacker from monopolizing all of `MaxConnections` with SSE.
   - Note: Go's `IdleTimeout` applies *between* requests on a keep-alive conn — it does **not** kill an in-flight SSE stream, because the stream is one long request. And `WriteTimeout` on the `http.Server` must be longer than `sseKeepalive=15s` or SSE breaks for legitimate users. So no `http.Server`-level timeout alone closes this DoS; the per-handler cap is the load-bearing fix.
2. **Defense-in-depth: set `IdleTimeout: 5 * time.Minute` on the `http.Server`.** This won't help the SSE-saturation case above, but it reaps truly idle TCP conns (slowloris-style "opened conn, sent partial bytes after headers, never finished") that aren't running a handler. Cheap; close the unrelated class.

**Discovered.** 2026-05-28 — Lane L5 conn-cap probe.

---

## F046 — LOW — `MaxBytesReader` body cap is dead code in v0.12

**Files:**
- `internal/web/server.go:66` (`maxRequestBytes = 1 << 20`)
- `internal/web/server.go:104` (`r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)`)
- `internal/web/handlers.go:48-51, 66-69, 121-124` (every handler returns `405 Method Not Allowed` immediately on non-GET, *before* touching `r.Body`)
- `internal/web/events.go:31-34` (same — GET-only)

**Observed.** A `POST /api/status` with a 1 MiB+100B body returns `405 Method Not Allowed` without the body being read. The `MaxBytesReader` wrapper is installed but never triggered because Go's `net/http` doesn't *force* body reads — handlers can return without consuming `r.Body`, and the server's `Connection: close` response signals the kernel/socket layer to discard the unread body.

**Why LOW (not a bug, just an observation).** The code is correct defense-in-depth for a future POST/PUT handler. But anyone reading `server.go` could plausibly believe Bundle C's `MaxBytesReader` is exercised by current handlers — it isn't. Worth a comment update.

**Fix shape.** Either:
- Add a comment to `maxRequestBytes` explaining it's "armed but inert until a non-GET handler lands" (low-effort, accurate).
- Add a regression test that mocks a body-reading handler and verifies the `413` / `MaxBytesError` return — locks the contract in place so a future POST handler doesn't accidentally strip the cap by routing past the middleware.

**Discovered.** 2026-05-28 — Lane L5 body-cap probe.

---

## F047 — LOW — XSS via `'unsafe-inline'` CSP — closure (no current sink)

**Files:**
- `internal/web/server.go:86-92` (CSP includes `'unsafe-inline'` for `script-src` and `style-src`)
- `internal/web/static/index.html` (the entire dashboard — fully embedded static file)
- `internal/web/handlers.go:41-55` (serves the embedded `index.html` byte-for-byte)
- `internal/web/static/index.html:137-141` (client-side `escapeHTML` helper)
- `internal/web/static/index.html:222-229, 262-275, 286-292` (every `innerHTML` write routes user-derived data through `escapeHTML` first)

**Observed by inspection** (no probe needed — the source is conclusive):

1. The server never templates user data into HTML. The `/` handler serves the embedded `index.html` byte stream verbatim.
2. The `/api/status` and `/api/show/<label>` handlers emit JSON (`application/json; charset=utf-8`) — even if a session name contained `<script>`, the JSON encoder escapes `<`, `>`, `&`. The browser parses it as JSON, not HTML.
3. The client-side JS reads the JSON and writes to `innerHTML` — but every dynamic field is wrapped: `escapeHTML(s.name)`, `escapeHTML(s.branch)`, `escapeHTML(d.brief)`, etc.
4. `escapeHTML` covers `& < > " '` — the full set browsers need to neutralize for an `innerHTML` text context. Attribute-context sinks would need additional handling, but the current code only writes text-context children.

**Why LOW / closure.** The `'unsafe-inline'` allowance is a real CSP-strength regression (per the source comment on lines 80-85), but bosun's dashboard has no XSS sink to exploit it against. Document, re-check on every PR that adds an HTML sink.

**Fix shape.** Two paths if hardening is desired later:
- Move inline `<style>` and `<script>` blocks into separate `static/dashboard.css` and `static/dashboard.js` files, drop `'unsafe-inline'` from the CSP. Simple, breaks nothing.
- Nonce-based CSP: server generates a per-response nonce, injects `<script nonce="...">` into `index.html` at serve time, replies with `Content-Security-Policy: script-src 'self' 'nonce-...'`. Stronger but adds templating risk that doesn't currently exist.

**Discovered.** 2026-05-28 — Lane L5 source inspection.

---

## F048 — LOW — Missing modern hardening headers (`Permissions-Policy`, `COOP`, `CORP`)

**Files:** `internal/web/server.go:97-107` (`securityHeaders` middleware sets only 4 headers + body cap).

**Observed.** Every response shape (200/400/404/405/500) carries exactly:
- `X-Frame-Options: DENY`
- `X-Content-Type-Options: nosniff`
- `Content-Security-Policy: default-src 'self'; ...`
- `Referrer-Policy: no-referrer`

Notably absent:
- `Permissions-Policy: ` — nothing restricting browser features (camera, geolocation, etc.). Mostly cosmetic for a dashboard with no such APIs, but a `Permissions-Policy: interest-cohort=()` style header is cheap.
- `Cross-Origin-Opener-Policy: same-origin` — protects against cross-origin window references (Spectre-class). Cheap, no cost.
- `Cross-Origin-Resource-Policy: same-origin` — defense-in-depth against cross-site `<img>` / `<script>` embeds reading the dashboard. Combined with F044's Host gate, this is the second wall.
- `Strict-Transport-Security` — N/A because bosun serves plaintext HTTP.

**Why LOW.** Bundle C's stated goal was the malicious-browser-tab vector; the 4 headers cover the historical XS-leaks list. The omissions above are 2024+ era hardening that mostly matter when (a) the dashboard is reachable cross-origin (which F044 makes worse than it should be) or (b) a future feature adds a sensitive browser API.

**Fix shape.** Add to `securityHeaders`:

```go
h.Set("Cross-Origin-Opener-Policy", "same-origin")
h.Set("Cross-Origin-Resource-Policy", "same-origin")
h.Set("Permissions-Policy", "interest-cohort=()")
```

**Discovered.** 2026-05-28 — Lane L5 header survey.

---

## Probes that did NOT produce findings (recorded for completeness)

- **Header injection (CRLF in value):** Go's `net/http` parses `\r\n` as the header terminator and rejects values containing literal CR/LF. A `Set-Cookie: hijack=1` smuggled as a second header line shows up as a request header from the client perspective; the server doesn't reflect it. No injection.
- **Path traversal in `/api/show/`:** `/api/show/../../../etc/passwd` returns 404 — handler rejects when the trimmed path contains `/`. `handleShow` then runs the input through `session.ParseLabel` which rejects non-label characters. Defense is tight.
- **Big single header (1 MiB-ish):** Go's `MaxHeaderBytes: 1 MiB` (explicitly pinned in `server.go:71,214`) caps it. Headers right under the cap process normally; over-cap, Go returns `431 Request Header Fields Too Large` before the handler runs.
- **Method-not-allowed (405) header coverage:** confirmed on HEAD, POST, OPTIONS, TRACE, PUT against `/` and `/api/status`. All carry the 4 headers.
- **Static asset / sub-path on `/api/show/`:** `/api/show/` with no segment, and `/api/show/foo/bar`, both correctly return 404. No tree-walk surface.
