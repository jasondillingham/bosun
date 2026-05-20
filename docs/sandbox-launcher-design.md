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

1. **Phase 1: agent_command config — SHIPPED 2026-05-19.**
   Per-session `agent_command` + the `examples/agent-wrappers/`
   directory's `docker-claude.sh` and `ollama-aider.sh` cover the
   wrapper-script path. See
   [agent-command-design.md](agent-command-design.md).

2. **Phase 2: `StrategyDocker` — SHIPPED 2026-05-19.** Native
   command-rewrite layer in `internal/launcher/docker.go`:
   - `StrategyDocker` constant in `launcher.Options.Strategy`.
   - `dockerInvocation(opts)` composes `docker run --rm -it` with
     worktree + MCP socket bind mounts, plus operator-configured
     extra mounts and env passthrough.
   - The OS terminal launcher (Ghostty / Terminal.app / iTerm2 /
     tmux / Linux / Windows) still opens the window; Docker is
     the command running inside.
   - `config.Docker` struct adds `image`, `extra_mounts`,
     `env_passthrough`. `bosun config validate` refuses
     `launcher=docker` with an empty image.
   - Bosun's existing `proc.Terminate` cleanup hits the host
     docker CLI, which propagates SIGTERM to the container — no
     `RunningHandle` generalization required.
   - In-container env: `BOSUN_MCP_SOCK` is rewritten to the
     bind-mount path; `BOSUN_SESSION` is forwarded; `BOSUN_BIN`
     is stripped (host path that doesn't exist in the container).
   - Container naming: `bosun-<session-label>` so operators can
     `docker stop` / `docker ps` against a known name.

   Pinned by:
   - `TestDockerInvocation_Minimal` / `_RequiresImage` /
     `_RequiresWorktree` / `_BindsMCPSocket` /
     `_ForwardsBosunSession` / `_SkipsBosunBin` /
     `_ExtraMountsForwarded` / `_EnvPassthroughByName`.

   Deferred from Phase 2:
   - `bosun_attach` MCP tool so in-container agents can self-
     register (today they need the `bosun` binary mounted into
     the container; the wrapper README documents the workaround).
   - Detached mode (`docker run -d`) and corresponding
     `docker stop` cleanup path. Foreground-via-terminal is the
     v1 UX; detached can land in a follow-up.

3. **Phase 3: remote / multi-host docker (DEFERRED).** Phase 2's
   local-only scope surfaced the real operator need within hours:
   *"docker running on one or many other systems to offload the
   workload."* The Phase 2 architecture (bind-mount the local
   worktree, talk to a local MCP socket, kill the host docker CLI
   PID) doesn't scale across hosts. Phase 3 has to solve four
   distinct problems together:

   ### 3.1 Connection to a remote daemon

   Docker CLI already supports `DOCKER_HOST=ssh://user@host` and
   `DOCKER_HOST=tcp://host:port`. The trivial plumbing is: add a
   `docker.host` config field (or per-session brief clause), set
   it as an env var in the launcher's Options.Env so the spawned
   `docker run` resolves against the remote daemon.

   But "trivial plumbing" hides the real architectural choice:
   **does bosun own the host selection, or does the operator?**
   Three plausible shapes:

   - **Single static remote.** `docker.host: ssh://thor.local`.
     Every session goes to thor. Cheapest. Doesn't deliver the
     "spread across many systems" benefit.
   - **Pool with operator placement.** `docker.hosts:
     [ssh://thor, ssh://docker-server]` + brief clause
     `(host: thor)` per session. Operator-decided.
   - **Pool with bosun-picked placement.** Same config; bosun
     round-robins or picks the least-loaded host. Requires
     liveness + load probes; meaningful new surface area.

   ### 3.2 Worktree availability on the remote host

   Bind-mount `/local/path:/work` only works when `/local/path`
   exists on the remote docker host. Three sub-options:

   - **Operator pre-syncs.** Each remote host has the repo at a
     known path; bosun assumes it's there. Brittle, manual.
   - **Bosun rsyncs at launch + cleanup.** Up-front bandwidth on
     init; results sync back on `bosun done`. Conflicts (remote
     edits between syncs) are the operator's problem.
   - **Remote clones from local, pushes back.** Each remote host
     clones the repo over SSH at session-start, runs the agent,
     pushes the session branch back to local on done. Cleanest
     model — git is the source of truth. Cost: a real
     `git daemon` or SSH-accessible bare repo on the bosun host.

   Recommend the **remote-clones-then-pushes** approach. It maps
   cleanly to bosun's existing model (each session is a branch),
   it handles concurrent writes correctly via git, and the
   operator already has SSH keys in place if `DOCKER_HOST=ssh://`
   works.

   ### 3.3 MCP socket across the network

   Local Unix socket → remote container can't reach it. Three
   sub-options:

   - **SSH-tunnel the socket** at launch. `ssh -L
     /remote/sock:/local/sock` per session. Auth via the same
     SSH key DOCKER_HOST uses. Breaks if the SSH connection
     drops mid-session.
   - **Bosun listens on TCP** (`bosun mcp --tcp :PORT`) with a
     shared secret token. Containers connect via
     `BOSUN_MCP_TCP=host:port` + `BOSUN_MCP_TOKEN=...`. New
     server-side auth code; net-new attack surface.
   - **Reverse-proxy through the SSH connection.** `ssh -R
     :unix:/local/sock:/remote/sock`. Forwards the listening
     socket via the tunnel docker already uses. Cleanest for
     ops; depends on OpenSSH version + per-host config.

   Recommend the **reverse-proxy via SSH** approach. It reuses the
   transport docker already needs, no new auth code on bosun's
   side, and a dropped SSH session correctly invalidates the
   socket (session goes CRASHED rather than silently disconnected).

   ### 3.4 Cleanup + agent detection across hosts

   `proc.Terminate` doesn't apply — the agent's PID is in the
   remote container's PID namespace. Options:

   - **`docker stop <name>` via the remote daemon.** Bosun already
     names containers `bosun-<label>`. Add a `DockerCleanup`
     helper that runs `docker stop bosun-<label>` against the
     session's recorded `docker.host`. Reuses the existing
     `bosun cleanup` orchestration; just swaps `proc.Terminate`
     for the docker command when the session was launched on
     a remote host.

   For agent detection (`bosun status` RUNNING column): the
   in-container agent self-registers via `bosun_attach` (MCP
   tool — currently CLI-only, see Phase 2 deferred items). With
   reverse-proxied MCP, the in-container `bosun_attach` call lands
   on the host's MCP server and the attached-pid file is written
   correctly. No process-table scan needed for remote agents.

   ### 3.5 Decision points to settle before Phase 3 starts

   - Single static host vs pool vs bosun-placement (see 3.1).
   - Worktree-sync strategy: operator-pre-sync vs rsync vs
     remote-clones-then-pushes (see 3.2; recommend
     remote-clones).
   - MCP plumbing: SSH-tunnel vs TCP+auth vs reverse-proxy (see
     3.3; recommend reverse-proxy).
   - Schema change: per-session "where did this run?" state. The
     session state file gains a `docker_host` field; cleanup
     reads it to know where to send `docker stop`.
   - Whether `bosun_attach` graduates to a real MCP tool (see
     Phase 2 deferred items) — Phase 3 depends on it for
     in-container self-registration to work cleanly.

   ### 3.6 Out of scope for Phase 3 (or any other phase)

   - Sub-host placement (Kubernetes namespaces, Docker
     Swarm services, etc.) — bosun isn't competing with
     orchestrators.
   - Live session migration between hosts. Sessions stay where
     they were launched.
   - Cross-host MCP coordination (one MCP daemon per host, vs
     one global daemon). Default is one global on the bosun
     host; remote daemons can be a Phase 4 if anyone asks.

   Phase 3 is intentionally a real round of its own — easily
   2-3 sessions of work + a separate design + planning doc
   refining 3.1–3.5 before any code lands.

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
