#!/usr/bin/env bash
#
# bosun agent wrapper: hand off the session to `aider` using a remote
# (or local) Ollama server as the LLM backend.
#
# Why aider rather than `ollama run` directly? Aider is a real coding
# agent — it edits files, runs commands, and follows the same
# brief/claim/done workflow bosun expects. `ollama run` is just a chat
# box; the session would have no way to touch files in the worktree.
#
# Aider routes through LiteLLM under the hood, which speaks Ollama's
# native API. Tool use works for any Ollama model that supports it
# (Llama 3.1+, Qwen 2.5 Coder, etc.); models without tool support
# degrade gracefully to a chat-only experience.
#
# ---------------------------------------------------------------------
# Configuration (all env-var-driven so this script stays generic):
#
#   OLLAMA_HOST    URL of the Ollama server. Default: http://localhost:11434
#                  Example: http://thor.local:11434 or http://10.66.0.45:11434
#   OLLAMA_MODEL   Model tag served by that host. Default: llama3.1:8b
#                  Example: qwen2.5-coder:7b, codellama:13b, etc.
#
# Set these in your shell rc, in .bosun/config.json's env hooks, or via
# bosun's per-session command override (see README.md).
#
# ---------------------------------------------------------------------
# Bosun contract:
#
# Bosun invokes:  <this-script> [<initial-prompt>]
# CWD:            the session's worktree
# Env it sets:    BOSUN_MCP_SOCK (when the MCP daemon is up)
#
# We pass the initial prompt to aider via --message so the agent
# starts with the brief loaded.
#
# ---------------------------------------------------------------------
# Known limitations:
#
# 1. bosun_* MCP tools are not wired into aider. Use the CLI fallback
#    (`bosun claim`, `bosun done`, `bosun_heartbeat` via shell) from
#    inside the aider session.
# 2. The BOSUN_BRIEF.md auto-load pointer (`.claude/CLAUDE.md`) is
#    Claude Code-specific; aider ignores it. The wrapper passes the
#    brief path as the initial message so the agent reads it anyway.

set -euo pipefail

: "${OLLAMA_HOST:=http://localhost:11434}"
: "${OLLAMA_MODEL:=llama3.1:8b}"

# Aider reads OLLAMA_API_BASE — alias it to OLLAMA_HOST so users
# only have to set one var.
export OLLAMA_API_BASE="$OLLAMA_HOST"

if ! command -v aider >/dev/null 2>&1; then
    echo "bosun: aider not on PATH. Install with: pip install aider-chat" >&2
    echo "       See https://aider.chat for setup details." >&2
    exit 127
fi

# Pre-flight: verify the Ollama server is reachable AND has the
# requested model loaded. Fast-fails the wrapper before opening a
# launcher window with an agent that's about to error opaquely on
# its first turn.
if ! command -v curl >/dev/null 2>&1; then
    echo "bosun: curl not on PATH; skipping Ollama pre-flight checks." >&2
elif ! tags_json="$(curl -sS --connect-timeout 5 "$OLLAMA_HOST/api/tags" 2>&1)"; then
    echo "bosun: Ollama server unreachable at $OLLAMA_HOST" >&2
    echo "       Set OLLAMA_HOST to your Ollama server (e.g. http://thor.local:11434)." >&2
    exit 1
elif ! printf '%s' "$tags_json" | grep -qF "\"$OLLAMA_MODEL\""; then
    echo "bosun: model '$OLLAMA_MODEL' not found on $OLLAMA_HOST." >&2
    echo "       Pull it on the Ollama host:  ollama pull $OLLAMA_MODEL" >&2
    echo "       Or pick one already loaded:  curl $OLLAMA_HOST/api/tags" >&2
    exit 1
fi

prompt="${1:-}"
args=(--model "ollama_chat/$OLLAMA_MODEL" --yes-always)

# Auto-load BOSUN_BRIEF.md if present — aider's --read flag stages it
# as read-only context so the agent sees the assignment without the
# operator having to type "read BOSUN_BRIEF.md" first.
if [ -f BOSUN_BRIEF.md ]; then
    args+=(--read BOSUN_BRIEF.md)
fi

if [ -n "$prompt" ]; then
    args+=(--message "$prompt")
fi

exec aider "${args[@]}"
