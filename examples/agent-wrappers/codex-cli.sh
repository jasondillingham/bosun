#!/usr/bin/env bash
#
# bosun agent wrapper: hand off the session to OpenAI's `codex` CLI.
#
# Codex CLI is a real coding agent — it reads the worktree, edits
# files, runs commands, and respects the same brief/claim/done
# workflow bosun expects. Same shape as ollama-aider.sh but routed
# at OpenAI's hosted models instead of a local Ollama server.
#
# Install: `npm install -g @openai/codex` (or `brew install codex`
# on macOS). Auth is via OPENAI_API_KEY in the host shell.
#
# ---------------------------------------------------------------------
# Configuration:
#
#   OPENAI_API_KEY   Required. Read from the host shell environment.
#                    The wrapper forwards it to codex without
#                    persisting it anywhere on disk — same model the
#                    other bosun wrappers use for ANTHROPIC_API_KEY.
#   CODEX_MODEL      Optional. Defaults to whatever codex's own
#                    default is (currently gpt-5-codex). Set this
#                    via --model env to pin a specific model:
#                    e.g. CODEX_MODEL=gpt-5
#   CODEX_EXTRA_ARGS Optional. Extra argv to pass to codex,
#                    space-separated. Useful for --approval-mode or
#                    --reasoning options that aren't worth their own
#                    env var.
#
# ---------------------------------------------------------------------
# Bosun contract:
#
# Bosun invokes:  <this-script> [<initial-prompt>]
# CWD:            the session's worktree
# Env it sets:    BOSUN_MCP_SOCK (when the MCP daemon is up)
#                 BOSUN_SESSION, BOSUN_BIN
#
# The initial prompt is forwarded to codex as the first user
# message so the agent reads the brief and starts the assignment
# without the operator having to type anything.
#
# ---------------------------------------------------------------------
# Known limitations:
#
# 1. bosun_* MCP tools are not wired into codex. Use the CLI
#    fallback (`bosun claim`, `bosun done`, etc.) from inside the
#    codex session — codex's "run a shell command" capability
#    handles this just fine.
# 2. No `.claude/CLAUDE.md` auto-load (Claude Code-specific). The
#    wrapper passes the initial prompt asking codex to read
#    BOSUN_BRIEF.md so the brief gets into context.

set -euo pipefail

if [ -z "${OPENAI_API_KEY:-}" ]; then
    echo "bosun: OPENAI_API_KEY not set. codex requires an OpenAI API key." >&2
    echo "       Get one at https://platform.openai.com/api-keys and export it" >&2
    echo "       in your shell (e.g. via ~/.zshrc or a per-repo .envrc)." >&2
    exit 1
fi

if ! command -v codex >/dev/null 2>&1; then
    echo "bosun: codex not on PATH. Install with:" >&2
    echo "       npm install -g @openai/codex" >&2
    echo "       or (macOS):  brew install codex" >&2
    exit 127
fi

prompt="${1:-}"
# Default initial prompt nudges the agent toward the brief if no
# explicit prompt was supplied. Mirrors the pattern bosun init uses
# for Claude Code (read BOSUN_BRIEF.md first).
if [ -z "$prompt" ] && [ -f BOSUN_BRIEF.md ]; then
    prompt="Read BOSUN_BRIEF.md to understand the assignment, then proceed."
fi

args=()
if [ -n "${CODEX_MODEL:-}" ]; then
    args+=(--model "$CODEX_MODEL")
fi
if [ -n "${CODEX_EXTRA_ARGS:-}" ]; then
    # shellcheck disable=SC2206
    extra=(${CODEX_EXTRA_ARGS})
    args+=("${extra[@]}")
fi
if [ -n "$prompt" ]; then
    args+=("$prompt")
fi

# Self-register with bosun before exec'ing into codex. The PID
# stays the same across exec, so $$ is the PID codex will run as.
# Without this, `bosun status` can't see the wrapper-launched agent
# (codex's process basename is "node" or "codex" depending on the
# install path — neither is in the default allowlist) and
# `bosun cleanup` won't terminate it on reap.
#
# Best-effort: failure to register only loses RUNNING-column
# accuracy + agent-kill on cleanup. The session still works.
bosun_bin="${BOSUN_BIN:-$(command -v bosun 2>/dev/null || true)}"
if [ -n "${BOSUN_SESSION:-}" ] && [ -x "$bosun_bin" ]; then
    "$bosun_bin" attach "$BOSUN_SESSION" --pid $$ >/dev/null 2>&1 || true
fi

exec codex "${args[@]}"
