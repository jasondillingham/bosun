# Agent wrappers

Drop-in scripts that let bosun launch alternative agents — local
models via Ollama, sandboxed Claude via Docker, anything else you
write a wrapper for. None of these require bosun changes; they
plug into the `agent_command` config knob shipped in Phase 1 of
[`docs/agent-command-design.md`](../../docs/agent-command-design.md).

## The contract

When bosun launches a session, it invokes:

```
<agent_command> [<initial-prompt>]
```

- **CWD** is the session's worktree.
- **`$BOSUN_MCP_SOCK`** is set when the MCP daemon is up (path to
  the Unix socket).
- **`$BOSUN_SESSION`** is set to the session label (e.g.
  `session-1`).
- The initial prompt is passed as **`$1`**. It's empty when bosun
  has nothing to seed.

Any executable that respects that contract works. The examples
here are starting points — copy, edit, point bosun at your fork.

## Examples in this directory

### `ollama-aider.sh`

Routes the session through [aider](https://aider.chat) using a
local or remote Ollama server as the LLM backend. Aider is the
right wrapper because it has actual file-editing capabilities — a
bare `ollama run` would just open a chat box in the worktree with
no way to touch code.

Env vars (defaults in parens):

| Var | Default | Notes |
|---|---|---|
| `OLLAMA_HOST` | `http://localhost:11434` | Set this to your Ollama server's URL. Example: `http://thor.local:11434` or `http://10.66.0.45:11434`. |
| `OLLAMA_MODEL` | `llama3.1:8b` | Any tag your server serves. Tool-use-capable models (Llama 3.1+, Qwen 2.5 Coder, etc.) get the best results. |

Requirements:

- `aider-chat` Python package (`pip install aider-chat`).
- An Ollama server reachable at `$OLLAMA_HOST` with `$OLLAMA_MODEL` pulled.

Known limitations:

1. **No MCP server compatibility.** `bosun_*` tools are wired into
   Claude Code, not aider. Use the CLI fallback inside the
   session: `bosun claim … `, `bosun done … `.
2. **No `.claude/CLAUDE.md` auto-load.** That pointer file is
   Claude Code-specific; aider ignores it. The wrapper passes
   `BOSUN_BRIEF.md` directly via aider's `--read` flag instead.

### `docker-claude.sh`

Runs Claude Code inside a Docker container with the worktree
bind-mounted. CPU/memory isolation without bosun owning a native
Docker launcher — the wrapper does the orchestration.

Env vars:

| Var | Default | Notes |
|---|---|---|
| `BOSUN_AGENT_IMAGE` | *(required)* | Docker image with `claude` installed. Operator brings their own. See [`docs/sandbox-launcher-design.md`](../../docs/sandbox-launcher-design.md) for image-contract notes. |
| `ANTHROPIC_API_KEY` | *(forwarded)* | Forwarded into the container when set in the host shell. |

Requirements:

- Docker Desktop (mac/win) or `docker-ce` (linux) on PATH.
- A container image with `claude` installed.

Limitations: see comments in the script itself.

## Wiring a wrapper into bosun

There are three places `agent_command` resolves from, in
precedence order:

1. **Per-session brief clause** — wins over everything else:
   ```markdown
   ## session-1 (command: ./examples/agent-wrappers/ollama-aider.sh)
   Routine refactor; cheap local model is fine.

   ## session-2
   Architecture work; falls back to the config default (claude).
   ```

2. **`bosun init --command <cmd>`** — applies to every session in
   the round, unless a brief clause overrides.

3. **`config.agent_command` in `.bosun/config.json`** — the per-
   repo default. Set via:
   ```sh
   bosun config set agent_command examples/agent-wrappers/ollama-aider.sh
   ```
   or hand-edit `.bosun/config.json`:
   ```json
   {
     "agent_command": "examples/agent-wrappers/ollama-aider.sh"
   }
   ```

## Writing your own wrapper

Minimum viable wrapper:

```sh
#!/usr/bin/env bash
# my-agent.sh — runs $YOUR_AGENT in the worktree with the bosun-
# provided prompt.
set -euo pipefail
prompt="${1:-}"
exec your-agent ${prompt:+--message "$prompt"}
```

Three things to keep in mind:

1. **Exec, don't fork.** `exec` replaces the wrapper process so
   `proc.Running` / `RunningPID` detection points at the real
   agent, not your bash PID.
2. **Respect `$BOSUN_MCP_SOCK`** if your agent supports MCP. Pass
   it through unchanged so the agent can reach `bosun_claim`,
   `bosun_done`, etc.
3. **Keep the prompt opt-in.** Most agents take an initial
   message via a `--message` / `-m` flag; pass it through only
   when `$1` is non-empty so users can also use the wrapper in
   chat mode (no seed).

The wrapper's **basename** is what `proc.IsAgentForCommand`
extends the detection allowlist with. So `my-agent.sh` makes
bosun look for a process named `my-agent` in the worktree's CWD.
For `exec`'d wrappers this is the agent binary's name, not the
script — your wrapper's basename is irrelevant to detection in
that case.
