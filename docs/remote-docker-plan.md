# Remote / multi-host Docker — implementation plan

Phase 3 of [`docs/sandbox-launcher-design.md`](sandbox-launcher-design.md).
Design captures the architecture (sections 3.1–3.5); this doc captures
*execution* — lanes, sequencing, decisions to nail down before any code
lands, and the trial that validates the round end-to-end.

This is a real implementation round (estimated 2–3 sessions of focused
work) and should NOT be shoehorned into a single sitting. The lanes
are designed to be parallelizable via bosun itself once the
foundation (lanes 1 + 5) lands.

---

## 0. Goal in one sentence

Make `bosun init N` capable of distributing each session's container
across one or more remote Docker hosts (e.g. a homelab fleet) so the
operator's main machine stays usable while real CPU work happens
elsewhere.

## 1. Constraints (anchor the design when scope creep arrives)

1. **Local bosun stays the source of truth.** State files, MCP server,
   merge target, status renderer — all on the bosun host. Remote hosts
   are pure compute.
2. **Git is the contract across the network.** No NFS, no sshfs, no
   rsync-the-worktree. Sessions push branches back to bosun-as-origin.
3. **MCP reaches across hosts.** In-container agents call
   `bosun_claim` / `bosun_done` / `bosun_heartbeat` exactly as
   local agents do; the plumbing is invisible to the agent code.
4. **One global MCP daemon.** Per-host MCP daemons are Phase 4 if ever.
5. **No new auth.** Operators already have SSH keys for `DOCKER_HOST=
   ssh://`; that's the only credential surface we add.
6. **Cleanup is best-effort.** Unreachable hosts at cleanup time
   warn-and-continue; the operator can `--force` to reap local state
   while leaving the remote container running.

## 2. Decisions to resolve BEFORE code lands

Each of these is its own ~15-minute discussion. Locking them down
first prevents a "the design changed mid-implementation" cascade.

### 2.1 Pool config shape

Three plausible shapes:

| Shape | Example | Trade-off |
|---|---|---|
| Single static host | `docker.host: "ssh://thor"` | Simplest. Doesn't deliver multi-host. |
| Pool + operator placement | `docker.hosts: ["ssh://thor", "ssh://docker-server"]` + brief clause `(host: thor)` | Operator-decided. Flexible. Requires the brief format extension. |
| Pool + bosun placement | Same config; bosun round-robins | Most magic. Needs liveness probes + load awareness. |

**Recommendation for v1:** pool + operator placement. The brief
already supports `(depends: …)` and `(command: …)`; adding
`(host: …)` is a small parser extension. Bosun-picked placement
can layer on later.

### 2.2 Brief clause syntax for per-session host

Already settled by precedent: `(host: thor)` in the heading clause,
parsed by the same `clauseRe` that handles `command:` and `depends:`.
Empty → fall back to config.docker.hosts[0].

### 2.3 Worktree-sync mechanism

Design doc recommends **remote-clones-then-pushes**. Lock that in.
Alternatives (operator-pre-syncs, bosun-rsyncs) get explicitly
rejected to prevent the round from re-litigating mid-implementation.

Concrete shape:

1. Bosun host runs a bare git daemon (or uses the existing repo as
   an SSH-accessible origin via `git push --set-upstream` over SSH).
2. At init, bosun pushes the new bosun/session-N branch to
   itself-as-origin so it's fetchable from remote hosts.
3. The remote container's startup command clones from the bosun
   host, checks out the session branch, runs the agent.
4. On `bosun done`, the in-container agent pushes the session
   branch back to bosun-as-origin.
5. Bosun merge runs locally against the freshly-pushed branches.

### 2.4 MCP socket plumbing

Design doc recommends **SSH reverse-proxy** (`ssh -R`). Lock that in.

Lifecycle: one SSH reverse-proxy per session, started just before
`docker run`, dies with the docker run process. Socket path inside
the container: `/work/.bosun/mcp.sock` (same as local Phase 2 —
keeps the in-container agent code path uniform).

### 2.5 bosun_attach as an MCP tool

Phase 2 explicitly deferred this. Phase 3 NEEDS it: in-container
agents can't run `bosun attach` as a CLI because the bosun binary
isn't in the container. The MCP tool lets them call attach over
the reverse-proxied MCP socket.

**Lock this in as lane 0 (blocks everything else).** ~50 lines.

### 2.6 docker_host persistence on the session

Session state file grows a `docker_host` field. Cleanup reads it
to know which `docker stop` to issue. Status reads it to know
which host to query. This is a state-schema change; settle the
field name + null-handling (sessions launched before Phase 3
have no docker_host → cleanup uses local proc.Terminate as today).

### 2.7 bosun binary on remote hosts

Two options:

| Option | Cost | Benefit |
|---|---|---|
| Require bosun installed on every docker host | Operator burden | Lets remote hosts run helper commands without container roundtrips |
| Don't require — all in-container agent calls go via MCP | Cleaner | MCP must support every operation the container needs (already mostly true) |

**Recommendation:** don't require. Push every cross-host concern
through MCP. Keeps the install surface small.

## 3. Implementation lanes

Five lanes, two foundation + three parallel.

### Lane 0 (foundation, ~50 lines) — `bosun_attach` MCP tool

**Why first:** every other lane assumes the in-container agent can
register itself. Without this, status detection is broken across
hosts.

**Scope:**

- New MCP tool in `internal/mcp/tool_attach.go`.
- Accepts `{session, pid}`, calls into `state.Store.WriteAttachedPID`.
- Auth: session label must match a bosun-managed session
  (refuse arbitrary writes).
- Update `cmd/bosun/cmd_attach.go` to ALSO accept invocation
  via MCP — or simpler, just expose the new tool and leave the CLI
  path as a parallel entry point.

**Test:** unit test for the MCP handler + a scenario test that
calls the MCP tool and verifies the attached-pid file lands.

### Lane 1 (foundation, ~100 lines) — config + connection plumbing

**Why second:** lanes 2/3/4 all need to know which host to target.

**Scope:**

- `config.Docker.Hosts []string` (list of SSH endpoints).
- `Brief.Host string` parsed from `(host: …)` clause.
- `--docker-host` CLI flag on `bosun init`.
- Resolution precedence: brief clause > CLI flag > Hosts[0] >
  unset → fall back to local docker.
- Launcher path: when DockerHost is set, prepend
  `DOCKER_HOST=ssh://…` to the env before composing the docker
  run pipeline.
- Validation: if Launcher == "docker" and Hosts is non-empty,
  every entry must be parseable as a URL with scheme ssh or tcp.

**Test:** table-driven Brief parser tests for `(host: …)`,
config validation tests, scenario test that confirms DOCKER_HOST
makes it into the launcher env.

### Lane 2 (parallel) — worktree sync via clone-and-push

**Scope:**

- New `internal/remote/` package (or extend `internal/git/`) with
  `PreparePushable(repoRoot, branch)` — ensures the local repo
  is SSH-cloneable from the docker host. May require:
  - Setting `receive.denyCurrentBranch=updateInstead` so pushes
    to the local working tree's branch succeed, OR
  - Creating a sibling bare repo under `.bosun/remote/repo.git`
    that the local working tree pushes to, and that the remote
    container clones from.
- Bosun host's SSH endpoint discovery: read `BOSUN_REMOTE_ORIGIN`
  env var if set; otherwise compose from `whoami@hostname:repo-path`.
- Update `dockerInvocation` for remote hosts: the in-container
  startup runs `git clone <bosun-ssh-origin> /work` instead of
  bind-mounting from `/work`.
- `bosun done` agent: pushes back to bosun-as-origin before
  marking DONE via MCP.
- Conflict handling: documented operator policy — concurrent
  edits to the same branch are not supported in v1. The brief
  warns the operator.

**Test:** integration test against a local SSH daemon (sshd in
Docker or rootless ssh-agent) to verify clone + push round-trip.

### Lane 3 (parallel) — MCP socket reverse-proxy

**Scope:**

- New `internal/remote/sshtunnel.go` — wraps `ssh -R
  /work/.bosun/mcp.sock:<local-mcp-sock> <host>` lifecycle.
- Started just before `docker run`; killed when docker run exits
  (signal propagation via the same parent-PID mechanism Phase 2
  already uses).
- Failure handling: SSH connection error → session goes CRASHED
  on next status render (in-container agent can't reach MCP →
  attached-pid liveness fires). Operator runs `bosun cleanup`.
- In-container path: `BOSUN_MCP_SOCK=/work/.bosun/mcp.sock`
  (same as Phase 2 local — the path stays uniform, only the
  transport changes).

**Test:** integration test using a local SSH server, asserts a
`bosun_heartbeat` call from inside a simulated container reaches
the host's MCP daemon.

### Lane 4 (parallel-ish — depends on lane 1) — cleanup + status

**Scope:**

- Session state gains `DockerHost string`. Persisted at init time
  via `state.Store.WriteDockerHost(label, host)`.
- `cmd_cleanup.go` `executeCleanupOne`: when `s.DockerHost != ""`,
  run `DOCKER_HOST=$host docker stop bosun-<label>` instead of
  (or before) `proc.Terminate`.
- `bosun status` RUNNING column for remote sessions: relies on
  attached-pid being written by the in-container agent via
  `bosun_attach` MCP tool (lane 0). No process-table scan on
  remote hosts.
- Cleanup failure handling: unreachable remote host warns + skips
  the docker stop; local state still gets cleared. Operator can
  re-run cleanup when the host comes back.
- `--force` semantics: clears local state regardless of remote
  reachability. Documented in the cleanup --help.

**Test:** scenario test that mocks a remote DOCKER_HOST and
verifies the cleanup pipeline issues the right command.

## 4. Sequencing

```
       lane 0 (bosun_attach MCP)
            │
            ▼
       lane 1 (config + connection)
       │      │      │
       ▼      ▼      ▼
   lane 2  lane 3  lane 4
   (sync) (tunnel) (cleanup)
```

Lanes 2–4 are parallel once 0 + 1 land. A bosun-managed round of
3 sessions could run them concurrently against the cleared lanes 0+1.

## 5. Trial protocol (the "we're done" check)

Setup:

- Operator's Mac (bosun host)
- Docker-server (10.66.0.20) — Linux + docker-ce, has SSH from Mac
- Thor (10.66.0.45) — Windows + WSL2 docker, has SSH from Mac
- One small reference image with `claude` + `git` installed,
  pushed to a registry both hosts can reach
- Operator's SSH key works against both hosts for `DOCKER_HOST=
  ssh://…` (manually verifiable: `docker -H ssh://… ps`)

Round:

1. `bosun config set launcher docker`
2. `bosun config set docker.hosts '["ssh://jason@10.66.0.20", "ssh://jason@10.66.0.45"]'`
3. Brief with 3 sessions, each pinning a host:
   ```
   ## session-1 (host: 10.66.0.20)
   tiny task A

   ## session-2 (host: 10.66.0.45)
   tiny task B

   ## session-3 (host: 10.66.0.20)
   tiny task C
   ```
4. `bosun init --brief plan.md --launch`
5. Three containers start across two hosts. Bosun host's `docker
   ps` is empty.
6. `bosun status` shows all three as WORKING with attached PIDs.
7. Each agent does its task, calls `bosun_done` via MCP.
8. `bosun merge` squashes all three back to local main.
9. `bosun cleanup` issues `docker stop bosun-session-N` against
   the right host for each session. Local state clears.

Demonstrated capabilities (1:1 with constraints in section 1):

- Local bosun is source of truth (state, merge, status).
- Git carried the work in both directions across the network.
- MCP worked from inside the container, across hosts.
- Single global MCP daemon served all three remote sessions.
- No new credentials beyond SSH keys.
- Cleanup ran against the right remote host.

## 6. Risks

Ranked by likelihood × impact.

1. **SSH key auth failures.** Operator's key works for shell but
   not for `DOCKER_HOST=ssh://`? Docker uses the system ssh client
   with `~/.ssh/config` — failures here are operator config
   problems, not bosun bugs. Mitigation: `bosun doctor` adds a
   pre-flight that runs `docker -H <host> info` against each
   configured host and reports failures.
2. **Container can't reach the local SSH origin.** Bosun host
   might be on a different network than the docker hosts (NAT,
   firewalls). Mitigation: lane 2 must support an
   operator-configured `bosun.remote_origin` override.
3. **Worktree sync race.** Two sessions on the same branch (
   shouldn't happen by design but operator mistake possible).
   Mitigation: lane 2 explicitly refuses if the remote branch
   already exists on bosun-as-origin.
4. **Reverse-proxy tunnel leaks.** Failed SSH session leaves
   an orphan tunnel process. Mitigation: lane 3 uses
   `proc.Terminate`-style supervision on the ssh PID.
5. **Cross-host bosun binary version skew.** If lane 0's
   bosun_attach MCP tool has a version-dependent payload,
   in-container agents talking to a newer bosun host get
   confused. Mitigation: lock the MCP tool schema before lane 0
   ships; add a schema version negotiation if it becomes a real
   problem.

## 7. Out of scope (deliberately)

- Bosun-picked placement / load balancing. v2 if anyone asks.
- GPU passthrough (already out of scope per design doc §6).
- Kubernetes / Swarm orchestration. Not competing with those.
- Cross-host session migration. Sessions stay where they're
  launched.
- Per-host MCP daemons. One global on the bosun host.
- Detached `docker run -d` mode. Foreground-via-terminal stays
  the UX (matches Phase 2).

## 8. Estimated effort

- Lane 0: 1 session (50 lines + tests + docs).
- Lane 1: 1 session (100 lines + parser update + config + tests).
- Lane 2: 1.5 sessions (real engineering on the SSH origin + clone
  + push lifecycle).
- Lane 3: 1 session (SSH tunnel wrapper + integration test).
- Lane 4: 0.5 session (mostly wiring, no new abstractions).
- Trial: 0.5 session.
- **Total: 5–6 sessions of focused work.**

If run as a bosun-managed multi-session round, lanes 2–4 in parallel
collapse the wall-clock to ~3 sessions: foundation round (0 + 1),
parallel round (2 + 3 + 4), trial round.

## 9. What this doc gets pruned to after Phase 3 ships

- Sections 0–2: kept (the constraints + decisions are load-bearing
  knowledge for the next phase).
- Sections 3–4: collapsed to a single "what shipped" paragraph.
- Section 5: kept (the trial recipe is reusable).
- Sections 6–8: deleted (risks materialized or didn't; effort
  estimate is no longer interesting).
