#!/usr/bin/env sh
#
# In-container heartbeat shim for bosun. Run this as a background
# process inside a Docker container so the host's `bosun status` knows
# the agent is alive — the host's PID-namespace-bound proc-scan can't
# see container PIDs, and the attached-pid path doesn't help across
# namespaces either. Periodic `bosun_heartbeat` calls are the portable
# liveness signal that crosses the container boundary.
#
# Bosun's session.Derive treats a fresh heartbeat (within ~5 min) as
# evidence of liveness when nothing else proved RUNNING. The RUNNING
# column renders "heartbeat" — distinct from a real PID — so the
# operator can tell at a glance that the session is in container mode.
#
# This script is a reference. It's POSIX `/bin/sh` so it runs in
# Alpine, busybox, and slim Debian images alike. The MCP call goes
# over the bind-mounted Unix socket — no network access needed.
#
# Requirements in the container:
#   * Either python3 OR socat. python3 is in most agent images already;
#     socat is a one-line apk add / apt install if not. The script
#     auto-detects and uses whichever is present.
#   * The MCP socket bind-mounted from the host. The native Docker
#     launcher binds it at /run/bosun-mcp.sock by default.
#
# Environment:
#   BOSUN_MCP_SOCK   Path to the bind-mounted MCP socket.
#                    (default: /run/bosun-mcp.sock)
#   BOSUN_SESSION    Session label (e.g. "session-1"). The native
#                    Docker launcher sets this for you.
#   BOSUN_HEARTBEAT_INTERVAL  Seconds between heartbeats. (default: 60)
#
# Dockerfile integration:
#   COPY in-container-heartbeat.sh /usr/local/bin/bosun-heartbeat
#   RUN chmod +x /usr/local/bin/bosun-heartbeat
#
# Then in your entrypoint script (or the agent wrapper):
#   /usr/local/bin/bosun-heartbeat &
#   exec your-agent ...
#
# The `&` backgrounds the heartbeat loop. The trailing `exec` replaces
# the shell with the agent so the container's PID 1 is the agent —
# important so that `docker stop` SIGTERMs the agent directly. The
# heartbeat loop exits when its parent shell exits (no explicit trap
# needed — the kernel reaps PID 1 children automatically).

set -eu

sock="${BOSUN_MCP_SOCK:-/run/bosun-mcp.sock}"
sess="${BOSUN_SESSION:-}"
interval="${BOSUN_HEARTBEAT_INTERVAL:-60}"

if [ -z "$sess" ]; then
    echo "bosun-heartbeat: BOSUN_SESSION not set; exiting" >&2
    exit 1
fi
if [ ! -S "$sock" ]; then
    echo "bosun-heartbeat: $sock is not a Unix socket; exiting" >&2
    exit 1
fi

# MCP framing is line-delimited JSON-RPC. The handshake + tools/call
# pair below is the minimum needed to invoke bosun_heartbeat once.
# Each invocation opens a fresh connection — connections are cheap
# and stateless, and re-establishing on every tick is more robust
# against transient socket issues than holding one open for hours.
mcp_call() {
    payload="$(printf '%s\n%s\n' \
        '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"bosun-heartbeat","version":"1.0.0"}}}' \
        '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bosun_heartbeat","arguments":{"session":"'"$sess"'"}}}')"

    if command -v python3 >/dev/null 2>&1; then
        printf '%s' "$payload" | python3 -c '
import os, socket, sys
sock_path = os.environ["BOSUN_HB_SOCK"]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(5)
s.connect(sock_path)
data = sys.stdin.read().encode()
s.sendall(data)
# Read a couple of replies, then close. We do not parse — failure
# surfaces via the next heartbeat erroring or the operator noticing
# the STALE flag.
s.settimeout(2)
try:
    s.recv(65536)
except Exception:
    pass
s.close()
' || return 1
        return 0
    fi

    if command -v socat >/dev/null 2>&1; then
        # socat keeps the connection alive long enough for the server
        # to write the responses; -t1 caps it at one second after EOF.
        printf '%s' "$payload" | socat -t1 - "UNIX-CONNECT:$sock" >/dev/null 2>&1 || return 1
        return 0
    fi

    echo "bosun-heartbeat: neither python3 nor socat available; cannot reach MCP" >&2
    return 1
}

export BOSUN_HB_SOCK="$sock"

while true; do
    mcp_call || echo "bosun-heartbeat: $sess heartbeat failed (will retry in ${interval}s)" >&2
    sleep "$interval"
done
