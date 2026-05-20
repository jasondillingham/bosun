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
- **`$BOSUN_BIN`** is the absolute path to the bosun binary that
  launched the session. Use this in your wrapper instead of
  relying on `bosun` being on `$PATH` — `go install` puts the
  binary in `$GOPATH/bin/` which isn't on a default macOS login
  shell's PATH.
- The initial prompt is passed as **`$1`**. It's empty when bosun
  has nothing to seed.

Any executable that respects that contract works. The examples
here are starting points — copy, edit, point bosun at your fork.

### Self-registration (important for non-claude agents)

If your wrapper `exec`s into a binary whose basename isn't
`claude` / `claude-code` / `code-cli`, bosun's proc-scan won't
find it — `bosun status` will show RUNNING as `—` and
`bosun cleanup` won't terminate the agent on reap.

Both example wrappers self-register via:

```sh
bosun_bin="${BOSUN_BIN:-$(command -v bosun 2>/dev/null || true)}"
if [ -n "${BOSUN_SESSION:-}" ] && [ -x "$bosun_bin" ]; then
    "$bosun_bin" attach "$BOSUN_SESSION" --pid $$ >/dev/null 2>&1 || true
fi
exec your-agent ...
```

The `$$` before `exec` is the PID that the exec'd binary will run
as (POSIX `exec` preserves the PID). Bosun records that PID in
`.bosun/state/<session>.attached-pid`, and the liveness gate
trusts it ahead of the proc-scan. Subsequent `bosun status`
correctly shows RUNNING; `bosun cleanup` SIGTERMs the right
process before removing the worktree.

### In-container agents: heartbeat instead of attached-PID

Self-registration with `bosun attach --pid` works on the **host**
because the wrapper and the host's proc-scan share the same PID
namespace. **Inside a Docker container, that PID is meaningless to
the host** — the container's claude is `1` (or some low number)
which collides with PID 1 / random host PIDs. Registering it
silently breaks the liveness gate.

The portable pattern for in-container agents is to call
`bosun_heartbeat` over the bind-mounted MCP socket every minute or
two. Bosun's liveness gate treats a fresh heartbeat as evidence of
liveness when nothing else proved RUNNING, and the status table
renders `heartbeat` in the RUNNING column (distinct from a real
PID) so the operator can see at a glance that the session is in
container mode.

[`in-container-heartbeat.sh`](in-container-heartbeat.sh) is a
drop-in reference shim. Copy it into your image and background it
from the entrypoint:

```dockerfile
COPY in-container-heartbeat.sh /usr/local/bin/bosun-heartbeat
RUN chmod +x /usr/local/bin/bosun-heartbeat
```

```sh
# in your entrypoint
/usr/local/bin/bosun-heartbeat &
exec claude
```

Either `python3` or `socat` must be present in the image — both
common, the script auto-detects which one is available.

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
| `OLLAMA_HOST` | `http://localhost:11434` | Set this to your Ollama server's URL. Example: `http://your-ollama-host.local:11434`. |
| `OLLAMA_MODEL` | `llama3.1:8b` | Any tag your server serves. **Strongly recommend `qwen2.5-coder:14b`** for actual code work — it's tool-use-capable, code-trained, and punches above its weight at 14B. Llama 3.1 8B works as a generic fallback but produces more "rewrite the file" patterns than targeted edits. |

Requirements:

- `aider-chat` Python package (`pip install aider-chat` or
  `pipx install aider-chat` for an isolated install).
- An Ollama server reachable at `$OLLAMA_HOST` with `$OLLAMA_MODEL`
  pulled. The wrapper pre-flights both before launching aider —
  unreachable host or missing model fails fast with a clear error
  instead of leaving you staring at an opaque aider crash.

Setup check (no aider needed):

```sh
# Confirm thor (or your Ollama host) is up:
curl http://YOUR_OLLAMA_HOST:11434/api/tags

# Pull the recommended model if not already there:
ollama pull qwen2.5-coder:14b   # on the Ollama host
```

Known limitations:

1. **No MCP server compatibility.** `bosun_*` tools are wired into
   Claude Code, not aider. Use the CLI fallback inside the
   session: `bosun claim … `, `bosun done … `.
2. **No `.claude/CLAUDE.md` auto-load.** That pointer file is
   Claude Code-specific; aider ignores it. The wrapper passes
   `BOSUN_BRIEF.md` directly via aider's `--read` flag instead.

### `docker-claude.sh`

Runs Claude Code inside a Docker container with the worktree
bind-mounted. CPU/memory isolation via wrapper-script orchestration.

> **Bosun also ships a native Docker launcher** (`launcher=docker`
> + `docker.image` in config). For most operators that's the
> simpler path — `bosun init` composes the `docker run` directly,
> no wrapper script in the loop. Use this wrapper instead when you
> want custom command-line behavior bosun's native launcher
> doesn't expose, or as a starting point for non-Claude
> in-container agents. See the project README for the native
> launcher syntax.

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
