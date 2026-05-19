#!/usr/bin/env bash
#
# bosun agent wrapper: run Claude Code inside a Docker container with
# the session's worktree bind-mounted. Per-session CPU isolation
# without bosun needing native Docker support — the wrapper handles
# the docker orchestration, bosun just runs whatever command it's
# pointed at.
#
# This is Phase 1 of docs/sandbox-launcher-design.md: operator-owned
# wrapper that delivers the local-isolation benefit without bosun
# committing to a StrategyDocker launcher. Phase 2 (native bosun-
# managed containers) is a future round.
#
# ---------------------------------------------------------------------
# Configuration (env-var-driven):
#
#   BOSUN_AGENT_IMAGE   Docker image with `claude` installed.
#                       No default — operator brings their own. Example:
#                       ghcr.io/your-org/bosun-agent:latest
#                       The image must have:
#                         * a working `claude` binary on PATH
#                         * git available
#                         * a non-root user with UID 1000 (avoids root-
#                           owned files in the bind-mounted worktree)
#   ANTHROPIC_API_KEY   Forwarded into the container so Claude Code can
#                       reach the API. Read from the host shell.
#   BOSUN_MCP_SOCK      Already set by bosun when the MCP daemon is up.
#                       This wrapper bind-mounts it into the container
#                       at the same path so bosun_* tools work.
#
# ---------------------------------------------------------------------
# Bosun contract:
#
# Bosun invokes:  <this-script> [<initial-prompt>]
# CWD:            the session's worktree (we bind-mount it as /work)
#
# We translate that into a `docker run` that:
#   1. Mounts the worktree at /work.
#   2. Mounts the MCP socket so bosun tools cross the container boundary.
#   3. Mounts ~/.claude for credential reuse (optional; comment out for
#      stricter isolation, but then operator has to log in inside the
#      container every time).
#   4. Forwards ANTHROPIC_API_KEY when set.
#   5. Runs `claude` with the initial prompt as its argv.
#
# Container removal: --rm cleans up on agent exit. `bosun cleanup`'s
# `proc.Terminate` won't reach the in-container PID, but the bind
# mount means killing `claude` from inside (via the host process tree)
# still works — Docker reports the exit and reaps the container.
#
# ---------------------------------------------------------------------
# Known limitations:
#
# 1. Window-close: bosun's launcher.CloseTmuxWindow runs after agent
#    termination; it has no equivalent for Docker. Use Docker Desktop's
#    UI or `docker ps -a | grep bosun-<label>` to inspect leftovers.
# 2. macOS Docker Desktop has VM I/O overhead — first `claude` startup
#    inside the container is noticeably slower than native. Linux
#    Docker runs at native speed.

set -euo pipefail

if [ -z "${BOSUN_AGENT_IMAGE:-}" ]; then
    echo "bosun: BOSUN_AGENT_IMAGE not set. Set it to a Docker image with claude installed." >&2
    echo "       Example: export BOSUN_AGENT_IMAGE=ghcr.io/your-org/bosun-agent:latest" >&2
    exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
    echo "bosun: docker not on PATH. Install Docker Desktop (mac/win) or docker-ce (linux)." >&2
    exit 127
fi

prompt="${1:-}"
worktree="$(pwd)"

# Container name lets `bosun cleanup` (or the operator) target this
# specific session's container if needed. The BOSUN_SESSION env is
# set by bosun's launcher.
container_name="bosun-${BOSUN_SESSION:-session}"

mounts=(-v "$worktree:/work")

# MCP socket bind: makes bosun_* tools work from inside the container.
# Unix sockets work across bind mounts on every supported host kernel.
mcp_sock="${BOSUN_MCP_SOCK:-}"
if [ -n "$mcp_sock" ] && [ -S "$mcp_sock" ]; then
    mounts+=(-v "$mcp_sock:/work/.bosun/mcp.sock")
fi

# Credential bind: reuses host's ~/.claude so the operator doesn't
# have to re-login inside every container. Comment out for stricter
# isolation (e.g. shared dev box where you don't want the container
# to have host creds).
if [ -d "$HOME/.claude" ]; then
    mounts+=(-v "$HOME/.claude:/root/.claude")
fi

env_args=()
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    env_args+=(-e "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY")
fi
env_args+=(-e "BOSUN_MCP_SOCK=/work/.bosun/mcp.sock")

cmd_args=()
[ -n "$prompt" ] && cmd_args=("$prompt")

exec docker run --rm -it \
    --name "$container_name" \
    -w /work \
    "${env_args[@]}" \
    "${mounts[@]}" \
    "$BOSUN_AGENT_IMAGE" \
    claude "${cmd_args[@]}"
