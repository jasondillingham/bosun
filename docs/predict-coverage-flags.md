# `bosun predict --coverage` flag reference

`bosun predict <plan> --coverage` extends the predictor with a repo-walk
that surfaces files containing "you'd be embarrassed if a stranger saw
this" content — but which no lane in the plan claims. The output sits
alongside the existing overlap report; exit code is `1` when either
overlaps OR coverage gaps are found.

## When to use it

The architect-mcp dogfood found that lane decompositions can be silently
incomplete: lane-1 claimed `cmd/bosun/**`, lane-2 claimed
`internal/storage/**` — and a homelab hostname slipped through in
`internal/screenshot/screenshot.go`, plus a personal path string in
`internal/mcp/blueprint_tool.go`. `predict` correctly said "no overlaps"
but missed that the lanes didn't cover those files at all.

`--coverage` is the safety net for that class of miss. Run it once
before `bosun init` to catch leaks. Run it as a CI gate to keep them
from coming back.

## Default flag set

Four built-in heuristic flags. Each finding includes the category name
in the report so an operator can tell at a glance which signal fired.

| Category          | What it looks for                                                                                            |
|-------------------|--------------------------------------------------------------------------------------------------------------|
| `personal-path`   | `/Users/<name>/`, `/home/<name>/`, `C:\Users\<name>\` style paths                                            |
| `internal-host`   | Common homelab service names (`thor`, `vault`, `valkey`, `prometheus`, `grafana`, `loki`, `synology`, …) and `<host>.local` mDNS hostnames |
| `possible-secret` | API-token shapes (`sk-…`, `ghp_…`, `glp_…`, `AKIA…`) and 32+ hex-char strings inside string literals          |
| `todo`            | `TODO`, `FIXME`, `XXX` markers                                                                               |

Global exclusions applied to every scan: `.git/**` and `vendor/**`.

The four built-in categories are stable identifiers — see
`internal/predict/coverage.go` for the `FlagPersonalPath` /
`FlagInternalHost` / `FlagPossibleSecret` / `FlagTodo` constants. They
appear in the report and as the section name an override file uses to
tune or disable them.

## Per-repo overrides

If `.bosun/predict-flags.toml` exists at the repo root, the scanner
merges its contents into the default set. The file is optional; absent,
the built-ins run unchanged.

```toml
# Disable a built-in entirely.
[flags.personal-path]
enabled = false

# Replace a built-in's regex. The category name stays the same so the
# report still says "internal-host".
[flags.internal-host]
regex = "\\b(mybox|otherbox)\\b"

# Add a custom flag. Regex is required for custom flags. exclude is a
# list of slash-separated glob patterns relative to the repo root; both
# `**` (any depth) and `*` (one segment) are supported.
[flags.custom-bad-word]
enabled = true
regex = "(?i)\\b(vault|styx[\\s-]?vanguard)\\b"
description = "project-specific terms that shouldn't ship publicly"
exclude = ["docs/**", "**/testdata/**"]
```

Schema rules:

- Only `[flags.<name>]` sections are recognised. Anything else is a
  parse error.
- Keys per section: `enabled` (bool, default `true`), `regex` (string),
  `description` (string), `exclude` (string array).
- Strings are double-quoted. The usual escapes work (`\\n`, `\\t`, `\\"`,
  `\\\\`); unrecognised escapes are passed through verbatim, which lets
  you write regex backslashes (`\\b`, `\\d`, `\\s`) without double-escaping.
- Custom (non-built-in) flags must specify a `regex`. A missing regex
  is a parse error so a typo in the section name isn't silently ignored.

Override precedence:

1. Defaults load first (four built-ins, two global excludes).
2. For each `[flags.<name>]` section:
   - If `enabled = false`, the flag is removed (if built-in) or skipped
     (if custom).
   - If the name matches a built-in, `regex` / `description` replace
     the built-in's values; `exclude` is appended.
   - Otherwise a new flag is appended to the end of the list.

## Output shape

Human-readable form (the default):

```
$ bosun predict plan.md --coverage
Predicted conflict report for plan.md
…
Overlaps: none predicted.

Coverage gaps (3 files have flagged content but no lane claims them):
  internal/screenshot/screenshot.go:13   (internal-host: 'vault')
  internal/mcp/blueprint_tool.go:22      (personal-path: '/Users/jason/')
  internal/architect/presets_test.go:75  (todo)

Suggestion: add these to a lane's scope, or assign an audit lane.
```

A zero-gap run prints `Coverage gaps: none — every flagged file is in
some lane's scope.` so the operator can confirm the flag actually ran.

JSON form (`--json --coverage`) adds a top-level `coverage` array of
`{file, line, category, match}` records to the existing
`{predictions, overlaps}` payload.

## Performance contract

The scanner must finish under 10s on repos of 100k+ LOC. Most real
repos finish in under 2s. To hit that target it:

- prunes excluded directories (`.git`, `vendor`, anything in
  `.bosun/predict-flags.toml`'s `exclude` lists) before stat'ing them
- skips files larger than 2 MiB (generated artifacts, fixtures)
- sniffs the first 512 bytes for a NUL byte and skips binaries
- bails on the first match per (file, flag) pair — a "this file is
  uncovered" signal doesn't need every hit

If a scan ever runs longer than this on your repo, open an issue with
the size, file count, and timing — that's a regression worth a fix.
