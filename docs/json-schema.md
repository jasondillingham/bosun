# Bosun JSON schema reference

Bosun emits machine-readable JSON from five places. This document is the
contract — every shape is locked in by tests in the producer's package
(see [Schema-lock tests](#schema-lock-tests) below) so any silent change
to a key or type fails CI.

The audit that produced this doc also surfaced eight live drift findings
across the five surfaces. They are catalogued in
[Findings — schema drift](#findings--schema-drift); each one is left in
place deliberately and tagged with the producer that owns the fix.

---

## Surfaces

| # | Producer | Source | Versioned? |
|---|---|---|---|
| 1 | `bosun status --json` | `internal/status/status_json.go::RenderJSON` | No (deliberate, v0.1 contract) |
| 2 | `bosun list --json` | `cmd/bosun/cmd_list.go::listJSON` | Yes (`version`) |
| 3 | `bosun show <session> --json` | `cmd/bosun/cmd_show.go::showJSON` | Yes (`version`) |
| 4 | `GET /api/status` (web) | `internal/web/handlers.go::handleStatus` → `status.RenderJSON` | No (shares producer #1) |
| 5 | `GET /api/show/<session>` (web) | `internal/web/handlers.go::showJSON` | No |
| 6 | `GET /api/events` (web, SSE) | `internal/web/events.go` → `internal/mcp.Event` | No |

Surface #4 reuses the producer for #1, so they share a single test.
Surface #6 emits one JSON object per SSE `data:` line; the event shape
is the same as the on-disk JSONL records under `.bosun/events.log`.

The schema version constant lives at
`internal/status.JSONSchemaVersion`. Producers 2 and 3 emit it under
the `version` key. Bumping it is the breaking-change knob; additive
fields keep the same value.

---

## 1. `bosun status --json` / `GET /api/status`

Two surfaces, one payload (the web handler calls `status.RenderJSON`
directly).

```json
{
  "sessions": [
    {
      "name": "session-1",
      "number": 1,
      "branch": "bosun/session-1",
      "path": "/abs/worktree/path",
      "state": "WORKING",
      "ahead": 0,
      "dirty": 0,
      "claimed": 0,
      "running": false,
      "running_pid": 12345,
      "last_sha": "abc1234",
      "last_subject": "implement auth",
      "last_relative": "3m ago",
      "last_unix": 1700000000,
      "state_message": "blocked on review",
      "parent": "session-0",
      "children": ["session-1.auth"],
      "depth": 1,
      "subtasks": 3
    }
  ],
  "overlaps": [
    { "path": "internal/auth.go", "sessions": ["session-1", "session-3"] }
  ]
}
```

- `sessions` is always present and is an array (never null), possibly empty.
- `overlaps` is `omitempty` from the CLI (only emitted when `--with-overlaps`)
  but always present on `/api/status` (the dashboard always wants them).
- `running_pid`, `last_*`, `state_message`, `parent`, `children`, `depth`,
  `subtasks` are `omitempty`. A typed-struct consumer gets the zero
  value when absent; a raw-map consumer must handle absence.
- `parent`/`children`/`depth` describe the spawn-tree (v0.9). Top-level
  sessions with no children render the same minimal payload as v0.8.
- `subtasks` (v0.11+) is the count of active (un-cancelled) sub-task
  records under `.bosun/subtasks/<name>/`. Sessions that never run a
  sub-task keep the v0.10 minimal payload (field absent).
- `number` is `0` for named sessions (e.g. `bosun init auth`). It is *not*
  `omitempty` — a value of 0 is emitted as `"number": 0`.

### Stability promise

Documented in the doc comment at `internal/status/status_json.go:1-46`.
**Keys and types above will not change within the v0.1 line.** Removing
or renaming is a breaking change reserved for a major bump. Additive
fields keep consumers working.

---

## 2. `bosun list --json`

```json
{
  "version": "v0.4.0",
  "sessions": [
    { "name": "session-1", "branch": "bosun/session-1", "state": "WORKING" }
  ]
}
```

- `version` mirrors `status.JSONSchemaVersion`.
- `sessions` always present, may be empty. With `--ready`, only DONE
  sessions are included; the version key is unchanged either way.
- Order matches `session.Derive`'s sort (numeric ascending, named
  label-alphabetical after).
- Per-session shape is intentionally a strict subset of the status
  surface — same key names (`name`, `branch`, `state`).

---

## 3. `bosun show <session> --json`

```json
{
  "version": "v0.4.0",
  "name": "session-1",
  "branch": "bosun/session-1",
  "worktree": "/abs/worktree/path",
  "state": "WORKING",
  "state_msg": "",
  "ahead": 0,
  "dirty": 0,
  "claimed_paths": [],
  "recent_commits": "abc1234 (HEAD -> bosun/session-1) initial\n",
  "brief": "# Bosun brief — session-1\n...",
  "agent_command": "",
  "usage_cost_usd": 0,
  "usage_tokens_in": 0,
  "usage_tokens_out": 0,
  "usage_turns": 0
}
```

- `version` mirrors `status.JSONSchemaVersion`.
- `claimed_paths` is **always a JSON array** (never null). Empty when
  no claim exists for the session.
- `recent_commits` is the raw output of `git log -10 --oneline --decorate`.
  May be empty and may contain newlines.
- `brief` is the full `BOSUN_BRIEF.md` of the worktree. May be empty
  (missing brief is not an error). May be large.
- `agent_command` is the per-session command override (from the brief
  or `init --command`). Empty string when the session uses the
  repo-wide `config.agent_command` default — combine with
  `bosun config get agent_command` to compute the effective command.
- `usage_*` fields are the Phase 4 cost tracking aggregates from the
  session's `bosun_usage` ledger. All four are always present; zero
  when the agent runtime hasn't reported any usage. Consumers
  distinguish "agent didn't report" from "agent reported $0" via
  `usage_turns` (turn count, never aggregated).
- None of the optional fields use `omitempty` here — they are always
  emitted, with empty strings or `[]` for the zero value. This is a
  deliberate divergence from the status surface so consumers don't have
  to probe for key presence.

### See also: drift with `/api/show/<session>` (#5) and `/api/status` (#1)

Two of this surface's keys disagree with surfaces #1 and #5 — `worktree`
instead of `path`, and `state_msg` instead of `state_message`. See
[F1](#f1-path-vs-worktree-key-name-collision) and
[F2](#f2-state_msg-vs-state_message-key-name-collision).

---

## 4. `GET /api/status`

Identical payload to `bosun status --json`; see #1 above. The handler
always passes `withOverlaps: true` so `overlaps` is always present here
(though may be empty).

---

## 5. `GET /api/show/<session>`

```json
{
  "name": "session-1",
  "number": 1,
  "branch": "bosun/session-1",
  "path": "/abs/worktree/path",
  "state": "WORKING",
  "state_message": "blocked",
  "ahead": 0,
  "dirty": 0,
  "claimed": 1,
  "running": false,
  "running_pid": 12345,
  "last_sha": "abc1234",
  "last_subject": "implement auth",
  "last_relative": "3m ago",
  "last_unix": 1700000000,
  "claimed_paths": ["internal/auth.go"],
  "brief": "# Bosun brief — session-1\n..."
}
```

- No `version` field on this surface (see [F3](#f3-version-field-is-inconsistent-across-surfaces)).
- Per-session fields (`name` through `last_unix`) mirror the per-session
  shape of `/api/status` (same key names, same `omitempty` rules). The
  additional fields are `claimed_paths` (array, never null) and `brief`
  (raw string, may be empty).
- 404 on unknown session; 400 on a malformed label; 405 on non-GET.

### See also: drift with `bosun show --json` (#3)

Different shape from the CLI `show --json` despite serving the same data:
this surface omits `recent_commits`, the CLI surface omits `number`/
`claimed`/`running`/`running_pid`/`last_*`. The two were specified
independently and never converged. See [F4](#f4-show-cli-and-show-web-have-disjoint-field-sets).

---

## 6. `GET /api/events` (Server-Sent Events)

Each `data:` line is a single JSON object:

```
data: {"session":"session-1","kind":"progress","message":"halfway","at":"2026-05-15T12:00:00Z"}

```

Event object:

| Key | Type | Notes |
|---|---|---|
| `session` | string | Session name (e.g. `session-1`). |
| `kind` | string | One of `info`, `progress`, `warn` (default `info`). Not enforced — consumers must tolerate unknown values. |
| `message` | string | Human-readable text. |
| `at` | string | RFC 3339 timestamp (Go `time.Time` default encoding). |

- The Go type is `mcp.Event` in `internal/mcp/events.go`. The same
  struct is what the JSONL events log on disk holds (`.bosun/events.log`),
  so the wire format and the on-disk format are guaranteed identical.
- **No version field.** This is the only persistent stream with no
  embedded version tag; see [F5](#f5-events-have-no-schema-version).
- SSE framing also emits `: keep-alive` comment lines every ~15s and may
  send blank lines; consumers must ignore non-`data:` lines.

---

## Versioning policy

- `internal/status.JSONSchemaVersion` is the single source of truth.
  Producers 2 and 3 emit it as `"version"`.
- **Additive changes** (new keys) keep the same version.
- **Breaking changes** (key rename, key removal, type change,
  `omitempty` flip) bump the version. Bump major when consumers cannot
  recover; bump minor when the change is opt-in.
- The `bosun status --json` surface is intentionally unversioned
  because it predates the constant and the doc comment provides the
  same promise (see `internal/status/status_json.go:38-46`).

---

## Findings — schema drift

The audit caught the following inconsistencies. Each is annotated with
the producer that should fix it; this doc deliberately documents the
*current* shape so consumers can rely on it until those producers ship
fixes.

### F1: `path` vs `worktree` key-name collision

The absolute worktree path is emitted under different keys depending on
the surface:

| Surface | Key |
|---|---|
| `bosun status --json` (per-session) | `path` |
| `/api/status` (per-session) | `path` |
| `/api/show/<session>` | `path` |
| `bosun show --json` | **`worktree`** |

Three of four use `path`; `bosun show --json` is the odd one out.
**Owner:** `cmd/bosun/cmd_show.go`. Likely best fixed by emitting both
keys for one release (additive), then dropping `worktree` in a versioned
breaking change.

### F2: `state_msg` vs `state_message` key-name collision

The body of the `.done` / `.stuck` marker file is emitted under
different keys:

| Surface | Key | Empty handling |
|---|---|---|
| `bosun status --json` (per-session) | `state_message` | `omitempty` |
| `/api/show/<session>` | `state_message` | `omitempty` |
| `bosun show --json` | **`state_msg`** | always present as `""` |

Different *both* in name and in empty-handling convention. **Owner:**
`cmd/bosun/cmd_show.go`. Same additive-then-rename strategy as F1.

### F3: `version` field is inconsistent across surfaces

| Surface | Has `version`? |
|---|---|
| `bosun status --json` | No (deliberate, doc-comment contract) |
| `/api/status` | No (shares producer with status) |
| `bosun list --json` | **Yes** |
| `bosun show --json` | **Yes** |
| `/api/show/<session>` | No |
| `/api/events` | No |

The two web surfaces that exist for parity with their CLI cousins
(`/api/show/<session>` ↔ `bosun show --json`) disagree on whether to
emit `version`. **Owner:** `internal/web/handlers.go`. Adding `version`
to `/api/show/<session>` and to `/api/status` is purely additive — no
existing consumer breaks.

### F4: `show` CLI and `show` web have disjoint field sets

The two surfaces returning per-session detail differ as follows:

| Field | `bosun show --json` | `/api/show/<session>` |
|---|---|---|
| `version` | ✅ | ❌ |
| `name` | ✅ | ✅ |
| `number` | ❌ | ✅ |
| `branch` | ✅ | ✅ |
| path key | `worktree` | `path` |
| `state` | ✅ | ✅ |
| state-msg key | `state_msg` | `state_message` (omitempty) |
| `ahead` | ✅ | ✅ |
| `dirty` | ✅ | ✅ |
| `claimed` (count) | ❌ | ✅ |
| `claimed_paths` (array) | ✅ | ✅ |
| `running` | ❌ | ✅ |
| `running_pid` | ❌ | ✅ (omitempty) |
| `last_sha` | ❌ | ✅ (omitempty) |
| `last_subject` | ❌ | ✅ (omitempty) |
| `last_relative` | ❌ | ✅ (omitempty) |
| `last_unix` | ❌ | ✅ (omitempty) |
| `recent_commits` | ✅ | ❌ |
| `brief` | ✅ | ✅ |

The web handler's own doc comment flags this:
"the full shape will converge with `bosun show --json` once session-3
merges; until then the field names are chosen to align". That
convergence never happened. **Owner:** split between
`cmd/bosun/cmd_show.go` and `internal/web/handlers.go`. The path
forward is to land the union of fields in both with `omitempty` and let
the schema version stay the same — purely additive.

### F5: Events have no schema version

`/api/events` and `.bosun/events.log` records carry no version tag.
The set of valid `kind` values is documented (`info|progress|warn`) but
not enforced; a future addition (e.g. `error`, `done`) would be
silently visible to old consumers as an unknown kind. **Owner:**
`internal/mcp/events.go`. Adding a top-level `v` integer to the event
struct is additive; older consumers' `encoding/json` decoders will
ignore it. The alternative is a documented promise that `kind` is
open-ended and consumers must tolerate unknowns — fine for now,
should be made explicit in the field doc.

### F6: `JSONSchemaVersion` value doesn't track release version

The constant is `v0.4.0`. The latest tagged release is `v0.2.0-alpha`
(see `RELEASES.md`). The schema version is independent of the release
version on purpose (different change cadence) but the `v0.4.0` value
risks reader confusion. **Owner:** documentation. Either name it
something that doesn't look like a release (e.g. `schema-2026-05`) or
add a note that this is a schema-only version distinct from the binary
release.

### F7: Empty-value conventions diverge

| Surface | Empty optional strings | Empty arrays |
|---|---|---|
| `bosun status --json` | `omitempty` (key absent) | array always present, possibly `[]` |
| `bosun list --json` | n/a | array always present |
| `bosun show --json` | always present as `""` | `claimed_paths` always `[]` |
| `/api/status` | `omitempty` | both arrays always present |
| `/api/show/<session>` | `omitempty` for status-derived fields | `claimed_paths` always `[]`, `brief` always present as string |
| `/api/events` | n/a (all required) | n/a |

The split is rough — `show --json` is the only producer that never
omits, the rest use `omitempty` for optional strings. **Owner:**
documentation policy. The doc above describes the current state;
unifying behind one rule is a v0.next concern.

### F8: `bosun status --json` overlaps inclusion differs from `/api/status`

`bosun status --json` omits the `overlaps` key entirely unless
`--with-overlaps` is passed (`omitempty`). `/api/status` always
includes it (handler passes `withOverlaps=true`). This is documented
behavior in the handler but worth flagging — a script that pipes
`status --json` to `/api/status`-style logic and assumes `.overlaps`
exists will trip on the missing key. **Owner:** call-site policy. Not
a bug — but the doc comment in `status_json.go` should state the web
handler's override.

---

## Schema-lock tests

If a future change breaks any of the shapes documented above, the
following tests fail loudly. They live next to each producer so a
session editing the producer can't miss them.

| Surface | Test |
|---|---|
| `bosun status --json` / `/api/status` | `internal/status/status_json_test.go::TestSchema_StatusJSON_LockedKeys` |
| `bosun list --json` | `cmd/bosun/cmd_list_test.go::TestSchema_ListJSON_LockedKeys` |
| `bosun show --json` | `cmd/bosun/cmd_show_test.go::TestSchema_ShowJSON_LockedKeys` |
| `/api/show/<session>` | `internal/web/server_test.go::TestSchema_ShowAPIJSON_LockedKeys` |
| `/api/events` | `internal/mcp/events_test.go::TestSchema_EventJSON_LockedKeys` |

Each test marshals a deterministic fixture and asserts the exact set
of top-level (and per-session) keys it sees. Adding a key fails the
test on purpose — the failure message points the implementer at this
doc so the schema, the doc, and the lock test all stay in sync.

The version constant is also locked: anyone bumping
`status.JSONSchemaVersion` must update the corresponding lock test in
the same change.
