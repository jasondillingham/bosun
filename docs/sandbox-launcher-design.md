# Sandboxed launchers — design proposal

**Scope.** New launcher strategies that run each bosun session's agent
inside an isolated environment instead of as a host process. Two
candidates: Docker (local container) and SSH (remote host). The
session's git worktree is still on the host filesystem; only the
agent's execution context moves.

This is **forward-looking** — no implementation today. Recording
constraints and trade-offs before a future round commits to one shape.

## 1. Motivation

The host machine becomes a bottleneck once parallel session count
crosses a small threshold. Concrete pain points:

1. **CPU saturation under load.** Running 4 Claude Code sessions
   simultaneously on a 10-core M-series Mac drives load averages
   past 8.0 — the existing pre-flight warning fires for exactly
   this reason ([cmd_init.go:309](../cmd/bosun/cmd_init.go)). With
   sandbox launchers, the host stays usable while agents do their
   work in dedicated environments.
2. **No CPU distribution across machines.** A homelab with a beefy
   workstation and an underused server (Ryzen, Synology, etc.)
   can't spread a 4-session round across both. Today bosun is
   single-host; SSH launchers would let a single round span
   multiple machines.
3. **Blast radius of an agent gone wrong.** A confused agent can
   `rm -rf` the worktree, exhaust disk on `/tmp`, fork-bomb, or
   leave socket FDs open. A container makes the worst case
   recoverable (`docker kill <ctr>`, container goes away, host is
   fine). The host-process model relies on the existing liveness
   gates + SIGTERM, which work for well-behaved agents but not
   adversarial ones.
4. **Reproducible agent environment.** Today the agent inherits
   whatever's on `PATH`, whatever Node/Python version is active,
   etc. A container makes "the agent has exactly these tools" a
   guarantee rather than a hope.

Non-goals for this proposal:

- Hard security isolation against a malicious agent. Containers are
  defense-in-depth, not a sandbox-escape barrier. Bosun's threat
  model is "agent makes a mistake," not "agent is hostile."
- Per-session network policy. Out of scope; revisit when CNI plumbing
  becomes worth the complexity.

## 2. Candidate strategies

### A. `StrategyDocker` — local container per session

- **Shape.** `docker run --rm -d -v <worktree>:/work -w /work
  -e BOSUN_MCP_SOCK=/work/.bosun/mcp.sock <image> <agent-command>`
- **Image contract.** Operator brings an image with the agent
  installed (`claude`, or whatever `agent_command` resolves to —
  see [agent-command-design.md](agent-command-design.md)). Bosun
  documents a reference Dockerfile in `docs/agent-images/` but
  doesn't ship images.
- **Worktree access.** Bind-mount. The container sees `/work` as
  the worktree; the host sees the same files at the original path.
  Git operations from inside (commit, push, etc.) work because
  `.git` is bind-mounted along with the worktree.
- **MCP socket.** Bind-mount `.bosun/mcp.sock` so the agent inside
  the container can talk to the bosun MCP server running on the
  host. Unix sockets work across bind mounts; this is the cheap
  path. Alternative (TCP) adds auth complexity nothing else needs.
- **Auth passthrough.** Two paths:
  - **`~/.claude/` bind-mount** for Claude Code agents — most
    invasive (writeable host credentials inside container).
  - **`-e ANTHROPIC_API_KEY=...`** for agents that read API keys
    from env. Cleaner but requires the operator to have the key
    in their shell env.
- **Window/TTY.** No GUI window. The operator attaches via
  `docker attach <ctr>` or `docker exec -it <ctr> bash` if they
  want a shell. Launcher prints the attach command on launch:
  `bosun: attach to session-1 with: docker attach bosun-session-1`.
  This is a UX regression from the current "window pops open"
  flow but is the honest shape — containers don't have desktops.
- **Lifecycle.** `--rm` cleans up the container on agent exit.
  `bosun cleanup` extends to also `docker kill bosun-<label>`
  before removing the worktree.
- **Cost estimate.** ~300-500 lines: new `StrategyDocker`, image
  detection (`hasDocker()`), launch path, attach-hint printer,
  cleanup wiring, scenario test against a real `docker` (or
  mockable `Docker` interface for unit tests).

### B. `StrategySSH` — remote host per session

- **Shape.** `ssh user@host 'cd <remote-worktree> && <agent-command>'`
- **Worktree access.** Three sub-options:
  1. **rsync up + rsync down** at launch + cleanup. Stateful;
     conflicts at sync time are the operator's problem. Latency
     per round.
  2. **sshfs/nfs mount.** Continuous; the remote sees the local
     worktree as a network mount. Latency per file op (bad for
     `git status`).
  3. **Remote repo clone with origin = local repo.** Each remote
     session pushes back to the local repo on `bosun done`. The
     cleanest model — local repo is the source of truth, remote
     does the work, results flow back via git's own machinery. But
     requires the remote can SSH to the local repo too (or pull
     via HTTPS).
- **MCP socket.** Same constraint as Docker but harder — Unix
  sockets don't work across network. Options:
  - **SSH tunnel** the socket: `ssh -L /remote/.bosun/mcp.sock:/local/.bosun/mcp.sock`.
    Works but every SSH session needs the tunnel. Connection drops
    leave the agent unable to call MCP.
  - **TCP MCP server with auth.** Bigger architectural change —
    bosun would need to support TCP + auth on the server side.
- **Auth passthrough.** Worse than Docker: API key has to land on
  the remote host one way or another. `ssh -E` env passthrough is
  the cleanest (`AcceptEnv ANTHROPIC_API_KEY` in remote sshd_config
  + bosun sets the env at SSH-spawn time).
- **Window/TTY.** Same problem as Docker. Operator attaches via
  `ssh <host>` and `screen -x` / `tmux attach` if they want the
  session live.
- **Cost estimate.** ~500-1000 lines + design discussion on the
  worktree-sync strategy. Lots of moving parts.

### C. `StrategyPodman` / `StrategyNspawn` — Linux-native alternatives

- Same shape as Docker but with the daemonless / system-container
  alternatives. Mostly a launch-command swap on top of the Docker
  design. Worth supporting as a near-zero-cost addition IF Docker
  lands first.

## 3. Cross-cutting concerns (any sandbox launcher)

1. **MCP socket boundary.** The single biggest design constraint.
   Bind mount (Docker) is cheap; tunnel (SSH) requires new
   plumbing; TCP requires new server-side auth. The launcher's
   choice here determines how much MCP server code needs to
   change.

2. **Agent process detection.** Today
   [proc.Running](../internal/proc/detect.go) finds the agent by
   walking the host process table and matching CWD. That breaks
   for both sandboxes:
   - Docker: agent PID is inside a container's PID namespace, not
     visible to host `gopsutil`. Need to track container ID
     instead, query `docker inspect`.
   - SSH: agent is on a remote host entirely; host process table
     has nothing.

   `session.Session.Running` becomes "is the container/SSH
   session alive?" rather than "is a process here alive?". The
   `RunningPID` field has to generalize too — maybe a `RunningHandle`
   string ("docker:bosun-session-1" / "ssh:user@host:12345") with
   handle-specific liveness checks.

3. **Cleanup termination.** `proc.Terminate` doesn't apply. Need:
   - Docker: `docker kill <container>` then `docker rm`. Already
     handled by `--rm` on graceful exit.
   - SSH: `ssh <host> kill <pid>` or just close the SSH session
     (agent dies on SIGHUP if it's not nohup'd).

4. **Output capture for `bosun show`.** Today the agent's stdout
   goes to its terminal window. Containers/SSH change this:
   - Docker: `docker logs <container>` is queryable.
   - SSH: needs a `tee` to a file or remote `screen`/`tmux`
     buffer.

5. **Concurrent host MCP daemon.** One MCP server today; multiple
   sandboxed sessions all share it via the bind-mounted socket.
   Existing socket-locking + concurrency tests cover this case as
   long as the socket interface stays the same.

## 4. Recommended phasing

1. **Phase 1: agent_command config** (see [agent-command-design.md](agent-command-design.md))
   — operators can already get most of this via wrapper scripts.
   `agent_command = "./docker-claude.sh"` where the script wraps
   `docker run -v ...`. Zero bosun changes for Docker; SSH still
   needs an MCP-socket-tunnel decision.

2. **Phase 2: `StrategyDocker`** with bind-mounted MCP socket.
   Solves the local CPU isolation case. Defers SSH's harder
   MCP-socket-across-network problem.

3. **Phase 3: revisit SSH** once Phase 2 is in production and the
   `RunningHandle` generalization has landed. Pick a
   worktree-sync strategy informed by real usage of phase 2.

## 5. Decision points to settle before Phase 2

- **Which agent-detection abstraction wins:** `RunningPID int` →
  `RunningHandle interface{ IsAlive() bool }` change. Refactor cost
  vs payoff.
- **Default Dockerfile for the reference image** — bosun ships a
  starter, or operators always BYO?
- **Auth-passthrough default** — env var vs bind-mounted
  `~/.claude/`. Each has security implications worth a thread.
- **Cleanup ordering** — kill container before or after `git worktree
  remove`? (Container has a bind mount into the worktree; removing
  the worktree under a live container produces undefined behavior.)

## 6. Out of scope for the Phase 2 doc

These are real concerns but each is its own design discussion:

- Per-session resource limits (CPU/memory caps on the container).
- GPU passthrough (`--gpus all` for Ollama-style local models in
  the container — see [agent-command-design.md](agent-command-design.md)).
- Image scanning / supply-chain policy (operator-controlled).
- Network policy (which hosts the container can reach).
- Cross-VLAN deployment (e.g., spawn session-1 on Docker host A
  and session-2 on Docker host B). Today's bosun assumes a single
  Docker daemon; multi-daemon needs separate work.

---

When Phase 2 starts, this doc gets pruned to "what actually shipped"
+ a roadmap entry for Phase 3.
