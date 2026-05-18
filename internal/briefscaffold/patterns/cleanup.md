# {{round-name}} — refactor (multi-lane cleanup)

Each lane owns one cross-cutting concern in the codebase. The lanes
are partitioned so they don't touch the same files. Use this pattern
when you have a mechanical refactor (adopt-new-API, rename-symbol,
migrate-to-new-pattern) that affects several disjoint subsystems.

The cardinal rule: **lanes must not touch shared types or
interfaces.** If a refactor needs a shared-type change, that change
goes in a prerequisite session that the lanes depend on — see the
`recipe` pattern for that shape.

## session-1

**Goal.** Migrate `{{subsystem-1}}` to `{{new-pattern}}` (e.g.,
"migrate internal/logging to slog; replace every `log.Printf` with
`slog.InfoContext(ctx, ...)` and propagate ctx through call sites").

**Target scope.** Only files under `{{subsystem-1-path}}`. If a call
site outside that path passes through `{{subsystem-1}}`, leave it
alone and add a follow-up note in your commit message.

**The mechanical change.**
- {{describe the before/after — be concrete. Two-three lines max.}}
- Before:
  ```{{language}}
  {{representative before-code snippet}}
  ```
- After:
  ```{{language}}
  {{representative after-code snippet}}
  ```

**Constraints (read carefully).**
- **Do not touch shared types or interfaces.** If the migration
  requires changing a type used by other packages, STOP and surface
  to the operator — that's a prerequisite session, not your work.
- Do not change behavior. This is a mechanical refactor — same
  inputs produce same outputs, same observable side effects.
- Do not "improve" code while you're in there. Rename a poorly-named
  variable only if the migration touches it for other reasons.
- `{{verify-cmd}}` must pass before and after every commit. The
  refactor should be commit-by-commit reviewable.
- Stay inside `{{subsystem-1-path}}`. If you need to touch a file
  outside it, ask the operator first.

**Done criteria.**
- Every call site under `{{subsystem-1-path}}` uses `{{new-pattern}}`.
- A grep for the old pattern in `{{subsystem-1-path}}` returns zero
  results (verify this in your commit message).
- `{{verify-cmd}}` passes.
- `bosun claim session-1 {{subsystem-1-path}}`
- `bosun done session-1 -m "migrated {{subsystem-1}} to {{new-pattern}}"`

## session-2

**Goal.** Migrate `{{subsystem-2}}` to `{{new-pattern}}` per the same
mechanical change as session-1.

**Target scope.** Only files under `{{subsystem-2-path}}`. No overlap
with `{{subsystem-1-path}}` or `{{subsystem-3-path}}`.

**Constraints.** Identical to session-1, substituting the path.
Don't touch shared types.

**Done criteria.**
- Every call site under `{{subsystem-2-path}}` migrated.
- `bosun claim session-2 {{subsystem-2-path}}`
- `bosun done session-2 -m "migrated {{subsystem-2}} to {{new-pattern}}"`

## session-3

**Goal.** Migrate `{{subsystem-3}}` to `{{new-pattern}}` per the same
mechanical change as session-1.

**Target scope.** Only files under `{{subsystem-3-path}}`. No overlap
with the other two subsystems.

**Constraints.** Identical to session-1, substituting the path.
Don't touch shared types.

**Done criteria.**
- Every call site under `{{subsystem-3-path}}` migrated.
- `bosun claim session-3 {{subsystem-3-path}}`
- `bosun done session-3 -m "migrated {{subsystem-3}} to {{new-pattern}}"`
