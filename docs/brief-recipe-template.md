# Recipe-style brief template

The default brief format treats `bosun_spawn` as a tool the agent
*may* call when it judges the work warrants fan-out. Trial #3b
(`docs/v0.9-trial-3b-findings.md`) showed that agents will route
around `bosun_spawn` for any work that feels solo-tractable, even
when the brief directs otherwise. The recipe template inverts the
default: the brief is a script, the agent's job is to execute it.

**Use the recipe form when:**

- You (the operator) want `bosun_spawn` called regardless of whether
  the agent thinks it's the fastest path.
- The work has obvious lane boundaries you've already identified.
- The parent-side integration depends on the subs landing first
  (so `merge --tree`'s post-order cascade does real work).
- You want clean per-lane commits for review / attribution
  / failure-isolation — operator values, not agent values.

**Use the discretionary form when:**

- You genuinely don't know yet whether the work warrants spawn.
- You want the agent's judgment to inform the workflow.
- A solo-tractable solution is acceptable.

The two forms can coexist: the recipe template below uses the same
`## session-N` heading shape, parses through `brief.Parse` the same
way, and the launcher / MCP / hook surfaces don't change at all.

## The template

Drop into `<repo>/<plan>.md`, fill the placeholders, run
`bosun init 1 --brief <plan>.md`. Placeholders are `{{like-this}}`.

```markdown
# {{round-name}}

## session-1

**Goal.** {{one-paragraph statement of the cross-cutting feature.
Name the integration the parent will write AND the per-lane pieces
the subs will land. Be specific about the shared interface — types,
signatures, contracts — that the subs must conform to.}}

### Shared interface (the contract subs must implement)

```{{language}}
{{exact type / interface / signature subs must agree on, in code
form. This is non-negotiable; subs don't get to redesign it.}}
```

### Recipe — execute these steps in order, do not skip

1. **Spawn sub-session `session-1.{{lane-1-label}}`** by calling the
   `bosun_spawn` MCP tool with these arguments:
   - `parent`: `"session-1"`
   - `label`: `"session-1.{{lane-1-label}}"`
   - `brief`: paste the body of `## session-1.{{lane-1-label}}` from
     this file (below) verbatim
2. **Spawn sub-session `session-1.{{lane-2-label}}`** the same way,
   using the `## session-1.{{lane-2-label}}` body.
3. **Spawn sub-session `session-1.{{lane-3-label}}`** the same way,
   using the `## session-1.{{lane-3-label}}` body.
4. **While subs work**, write `{{parent-integration-file}}` against
   the Shared interface defined above. The interface is final; the
   subs are implementing to it, not negotiating it.
5. **When all three subs are DONE**, run:

   ```sh
   bosun merge --tree session-1
   ```

   This walks the spawn tree post-order — each sub squash-merges into
   your branch first, then your branch is ready for the operator's
   final merge to main.
6. **Mark yourself DONE** with `bosun done session-1 -m "..."`. Your
   message should name the three lanes that landed and the
   integration file that ties them together.

### Constraints

- Do not write any per-lane code yourself. The three subs own that
  work; you own the integration.
- Do not change the Shared interface after spawning. If subs report
  a problem with the contract, ask the operator before redesigning.
- `go vet ./...` (or {{verify-cmd}}) must pass on every branch in the
  tree, including yours.

### Done criteria

- spawn-tree.json shows `session-1` with three children
- All three sub-branches landed on `bosun/session-1` via `merge --tree`
- `{{parent-integration-file}}` exists on `bosun/session-1` and uses
  the Shared interface
- {{verify-cmd}} passes

---

## session-1.{{lane-1-label}}

**Goal.** {{one-paragraph statement of what this lane delivers.
Reference the Shared interface from session-1's brief.}}

**Target file(s).** {{path/to/the.go and path/to/the_test.go}}

**Constraints.**
- Implement the Shared interface (see your parent's brief). The
  exact signature is non-negotiable.
- {{one or two lane-specific constraints — e.g., specific endpoint,
  test shape, env var the package reads}}
- No edits outside the target files. If you need to touch a sibling
  package, ask the parent.
- Verify with `{{verify-cmd}}` before `bosun done`.

**Done criteria.**
- Target files implement the interface
- Tests cover the success path AND the most-likely failure shape
- `bosun done session-1.{{lane-1-label}} -m "..."` with a one-line
  summary

---

## session-1.{{lane-2-label}}

{{same shape as session-1.{{lane-1-label}} above}}

---

## session-1.{{lane-3-label}}

{{same shape}}
```

## Worked example — trial #3b recast as recipe

What trial #3b's brief should have looked like if we'd known then
what we know now:

```markdown
# Trial #3b — Health() probes across three service clients (recipe)

## session-1

**Goal.** Add a uniform `Health()` capability to three service-client
packages (cloudflare / synology / tailscale) and expose them through
one new aggregating MCP tool, `homelab_health`. The per-package
implementations are independent of each other but all conform to the
same Shared interface defined below. The MCP tool fans out via
errgroup and aggregates results.

### Shared interface (the contract subs must implement)

```go
// HealthResult is the per-service probe outcome. Lives in a new
// shared file in internal/healthproto/healthproto.go which is the
// parent's responsibility to write before spawning.
type HealthResult struct {
    OK         bool
    Latency    time.Duration
    Detail     string  // free-form text, e.g. "token valid for 14 zones"
    Err        error
    Configured bool    // false if required env vars are unset
}

// Each of cloudflare.Client, synology.Client, tailscale.Client
// must add:
//
//   func (c *Client) Health(ctx context.Context) HealthResult
//
// Implementations probe a cheap read-only endpoint that exercises
// the credentials. On any failure, return HealthResult with OK=false
// and Err set; do NOT panic, do NOT return a Go error.
```

### Recipe — execute these steps in order, do not skip

1. **Write `internal/healthproto/healthproto.go`** with the
   `HealthResult` type above. Run `go vet ./...` to confirm it
   parses. Commit this on `bosun/session-1` BEFORE spawning so the
   subs branch from a base that has the type available.
2. **Spawn sub-session `session-1.cloudflare`** via `bosun_spawn`:
   - `parent`: `"session-1"`
   - `label`: `"session-1.cloudflare"`
   - `brief`: paste the body of `## session-1.cloudflare` from this
     file verbatim
3. **Spawn sub-session `session-1.synology`** the same way.
4. **Spawn sub-session `session-1.tailscale`** the same way.
5. **While subs work**, write `internal/mcp/tools_health.go` (the
   `homelab_health` MCP tool that fans out to the three `Health()`
   methods via `errgroup`) and `tools_health_test.go`. Register the
   tool in `internal/mcp/server.go` and add a capability entry in
   `internal/mcp/tools_capabilities.go`.
6. **When all three subs are DONE**, run `bosun merge --tree session-1`.
   The three sub-branches squash-merge into yours; your branch then
   has the per-package `Health()` methods alongside the MCP tool.
7. **Mark yourself DONE** with a message naming the three endpoints
   the subs chose and the file paths of the integration.

### Constraints

- Do not write any per-package `Health()` code yourself. The three
  subs own that.
- Do not change `HealthResult` after step 1. If subs report a problem
  with the type, stop and ask the operator.
- `go vet ./...` must pass on every branch.

### Done criteria

- `.bosun/spawn-tree.json` shows session-1 with three children
- All three sub-branches landed via `merge --tree`
- `internal/healthproto/healthproto.go` defines `HealthResult`
- `internal/mcp/tools_health.go` exposes `homelab_health`
- `go vet ./...` clean on `bosun/session-1`

---

## session-1.cloudflare

**Goal.** Add `Health(ctx) HealthResult` to `cloudflare.Client` per
the Shared interface in your parent's brief.

**Target files.**
- `internal/cloudflare/client.go`
- `internal/cloudflare/client_test.go`

**Constraints.**
- Probe endpoint: `GET /user/tokens/verify`. Cheapest creds-verifying
  call in the Cloudflare API.
- If `CLOUDFLARE_API_TOKEN` is unset, return `HealthResult{Configured:
  false}` immediately — don't make the network call.
- Tests cover: success, 401-unauthorized, network-error,
  no-creds-configured.
- No edits outside the two target files. The `HealthResult` type
  lives in `internal/healthproto` — import it.
- Verify with `go test ./internal/cloudflare/... && go vet ./...`.

**Done criteria.**
- `Health` method added to `Client`
- All four test cases pass
- `bosun done session-1.cloudflare -m "cloudflare.Client.Health
  probes /user/tokens/verify; tests cover ..."`

---

## session-1.synology

**Goal.** Add `Health(ctx) HealthResult` to `synology.Client` per
the Shared interface.

**Target files.**
- `internal/synology/client.go`
- `internal/synology/client_test.go`

**Constraints.**
- Probe: login + logout round-trip. Proves reachability AND
  credential validity in one call.
- If any of `SYNOLOGY_BASE_URL` / `SYNOLOGY_USERNAME` /
  `SYNOLOGY_PASSWORD` is unset, `HealthResult{Configured: false}`,
  no network call.
- Tests cover: success, bad-creds (login fails), unreachable-host,
  no-creds-configured.
- No edits outside the two target files. Import `internal/healthproto`.
- Verify with `go test ./internal/synology/... && go vet ./...`.

**Done criteria.**
- `Health` method on `Client`
- All four test cases pass
- `bosun done session-1.synology -m "..."`

---

## session-1.tailscale

**Goal.** Add `Health(ctx) HealthResult` to `tailscale.Client`.

**Target files.**
- `internal/tailscale/client.go`
- `internal/tailscale/client_test.go`

**Constraints.**
- Probe: `GET /tailnet/{tailnet}/devices`. Returns a non-trivial
  payload, exercises auth.
- If `TAILSCALE_API_KEY` or `TAILSCALE_TAILNET` is unset,
  `HealthResult{Configured: false}`.
- Tests: success, unauthorized, network-error, no-creds.
- No edits outside the two target files.
- Verify with `go test ./internal/tailscale/... && go vet ./...`.

**Done criteria.**
- `Health` method on `Client`
- All four test cases pass
- `bosun done session-1.tailscale -m "..."`
```

That brief takes the agent out of the "should I spawn?" decision
entirely. The shared-interface-up-front pattern is also what makes
the parent's integration work in step 5 possible — by step 5 the
type exists on `bosun/session-1` and the subs are building against
the same base.

## Notes on use

**The shared-interface-up-front step matters.** Trial #3b's agent
made the right call inventing `HealthResult` early. The recipe
formalizes this: the parent writes the shared type first, commits
it on its branch, and only THEN spawns. Subs branch from the parent's
HEAD per the v0.9 spawn spec, so they inherit the type. Without this
step, the three subs would each define their own `HealthResult` and
the parent's integration tool would have to reconcile three slightly
different shapes.

**The "do not skip" framing is load-bearing.** Trial #3b's brief
said "Use it" but also said "only if warranted." The recipe form
removes the hedge entirely. Steps are numbered, the agent executes
top-to-bottom.

**Don't recipe-brief small work.** If the integration plus the three
lanes would total <300 LOC, the spawn overhead is real and solo
might be the right answer. Recipe-style is for work where the
operator-side values (clean per-lane commits, isolated review,
attribution) outweigh the spawn ceremony cost. Below some size
threshold, the discretionary form is fine.

**The template is not yet a `bosun init` flag.** It's a doc + worked
example. A future `bosun init --spawn-recipe` could generate the
parameterized form into the worktree's BOSUN_BRIEF.md, but the
template stands on its own first — let it get used a few times
before automating it.

## What this validates / what it doesn't

If an operator drives a round with this template, we should see:

- spawn-tree.json populated with the parent + N children at the
  expected depth
- TUI + web tree rendering for the hierarchical labels
- `bosun merge --tree` doing real post-order work
- `bosun cleanup --tree` cascading

That covers the v0.9 surface trial #3b failed to exercise. It does
NOT validate the discretionary path — the agent's autonomous decision
that "this work warrants spawn." For that, we still need either a
v0.9.x decision-UX fix (issue #13) or a real external user who
spawns of their own accord.
