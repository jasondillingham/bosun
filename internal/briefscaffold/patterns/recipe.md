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

1. **Write the shared interface file FIRST** at
   `{{shared-interface-path}}` with the type(s) defined above. Run
   `{{verify-cmd}}` to confirm it parses. Commit this on your branch
   BEFORE spawning so the subs branch from a base that already has
   the type available. This step is load-bearing: it's why the three
   subs can each implement against the same `HealthResult`-shaped
   contract instead of each inventing their own.
2. **Spawn sub-session `session-1.{{lane-1-label}}`** by calling the
   `bosun_spawn` MCP tool with these arguments:
   - `parent`: `"session-1"`
   - `label`: `"session-1.{{lane-1-label}}"`
   - `brief`: paste the body of `## session-1.{{lane-1-label}}` from
     this file (below) verbatim
3. **Spawn sub-session `session-1.{{lane-2-label}}`** the same way,
   using the `## session-1.{{lane-2-label}}` body.
4. **Spawn sub-session `session-1.{{lane-3-label}}`** the same way,
   using the `## session-1.{{lane-3-label}}` body.
5. **While subs work**, write `{{parent-integration-file}}` against
   the Shared interface defined above. The interface is final; the
   subs are implementing to it, not negotiating it.
6. **When all three subs are DONE**, run:

   ```sh
   bosun merge --tree session-1
   ```

   This walks the spawn tree post-order — each sub squash-merges into
   your branch first, then your branch is ready for the operator's
   final merge to main.
7. **Verify the spawn-tree state before declaring done.** Call the
   `bosun_check_tree` MCP tool with `parent: "session-1"` and confirm
   every direct child reports `state: "done"`. A child reporting
   `no-launch` or `dead` means a sub vanished or crashed and your
   branch is missing its work — stop and surface to the operator
   before continuing to step 8.
8. **Mark yourself DONE** with `bosun done session-1 -m "..."`. Your
   message should name the three lanes that landed and the
   integration file that ties them together.

### Constraints

- Do not write any per-lane code yourself. The three subs own that
  work; you own the integration.
- Do not change the Shared interface after spawning. If subs report
  a problem with the contract, ask the operator before redesigning.
- `{{verify-cmd}}` must pass on every branch in the tree, including
  yours.

### Done criteria

- `.bosun/spawn-tree.json` shows `session-1` with three children
- All three sub-branches landed on `bosun/session-1` via `merge --tree`
- `{{parent-integration-file}}` exists on `bosun/session-1` and uses
  the Shared interface
- `{{verify-cmd}}` passes

### Sub-session bodies (paste into `bosun_spawn` per the recipe)

The headings below are informational — they don't parse as separate
sessions (the parent's `## session-1` heading owns this whole body).
Paste the body under each heading into the `brief` argument of the
corresponding `bosun_spawn` call.

#### session-1.{{lane-1-label}}

**Goal.** {{one-paragraph statement of what this lane delivers.
Reference the Shared interface from session-1's brief.}}

**Target file(s).** `{{lane-1-target-files}}`

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

#### session-1.{{lane-2-label}}

**Goal.** {{one-paragraph statement of what this lane delivers.}}

**Target file(s).** `{{lane-2-target-files}}`

**Constraints.**
- Implement the Shared interface defined in your parent's brief.
- {{lane-specific constraints}}
- No edits outside the target files.
- Verify with `{{verify-cmd}}` before `bosun done`.

**Done criteria.**
- Target files implement the interface
- Tests cover success + most-likely failure
- `bosun done session-1.{{lane-2-label}} -m "..."`

#### session-1.{{lane-3-label}}

**Goal.** {{one-paragraph statement of what this lane delivers.}}

**Target file(s).** `{{lane-3-target-files}}`

**Constraints.**
- Implement the Shared interface defined in your parent's brief.
- {{lane-specific constraints}}
- No edits outside the target files.
- Verify with `{{verify-cmd}}` before `bosun done`.

**Done criteria.**
- Target files implement the interface
- Tests cover success + most-likely failure
- `bosun done session-1.{{lane-3-label}} -m "..."`
