# {{round-name}} — code review (multi-lane)

This is a non-mutating round: each lane reads a different slice of
the codebase and writes a markdown notes file. No production code
changes. Useful when you want a second pair of eyes on a sprawling
feature without three reviewers stepping on each other.

## session-1

**Goal.** Review `{{path-1}}` for correctness, clarity, and
adherence to project conventions. Write findings to
`docs/review/{{round-slug}}-{{path-1-slug}}.md`. No edits to
production code in this lane — review notes only.

**Scope.**
- Files: every file under `{{path-1}}` (recursive).
- Out of scope: anything outside that path. If you see a smell in a
  sibling package, write a one-liner pointer in your notes but don't
  open files there.

**What to look for.**
- Logic bugs and edge cases the existing tests don't cover.
- Error handling: are errors wrapped with context? Are any
  swallowed? Any places that should return rather than continue?
- Concurrency: shared state without synchronization, goroutine
  leaks, channel-direction misuse.
- Public API shape: are exported symbols documented? Any that
  should be unexported?
- Test quality: do the tests actually verify behavior, or just
  that the code runs?

**Output format.** Markdown file with sections:
- `# Review notes: {{path-1}}`
- `## Findings` — numbered list, each entry: file:line, severity
  (blocker/major/minor/nit), short description, suggested fix.
- `## Open questions` — anything you can't answer from the code alone.
- `## What looked good` — at least 2 things. Reviews that are all
  negative are less useful than reviews that name what's working.

**Constraints.**
- Read-only on production code. The ONLY file you create or modify
  is the notes file at `docs/review/{{round-slug}}-{{path-1-slug}}.md`.
- Do not refactor "while you're in there" — that's a separate round.
- `{{verify-cmd}}` should still pass when you're done (it will,
  since you haven't touched production code).

**Done criteria.**
- `docs/review/{{round-slug}}-{{path-1-slug}}.md` exists with all
  four sections populated.
- At least one Finding (even if minor) — if you genuinely found
  nothing, say so in `## What looked good` and explain why your
  scan was thorough.
- `bosun claim session-1 docs/review/{{round-slug}}-{{path-1-slug}}.md`
- `bosun done session-1 -m "reviewed {{path-1}}: <N> findings"`

## session-2

**Goal.** Review `{{path-2}}` per the same shape as session-1.
Write findings to `docs/review/{{round-slug}}-{{path-2-slug}}.md`.

**Scope, what to look for, output format, constraints.** Identical
to session-1, substituting `{{path-2}}` for `{{path-1}}`.

**Done criteria.**
- `docs/review/{{round-slug}}-{{path-2-slug}}.md` populated.
- `bosun claim session-2 docs/review/{{round-slug}}-{{path-2-slug}}.md`
- `bosun done session-2 -m "reviewed {{path-2}}: <N> findings"`

## session-3

**Goal.** Review `{{path-3}}` per the same shape as session-1.
Write findings to `docs/review/{{round-slug}}-{{path-3-slug}}.md`.

**Scope, what to look for, output format, constraints.** Identical
to session-1, substituting `{{path-3}}` for `{{path-1}}`.

**Done criteria.**
- `docs/review/{{round-slug}}-{{path-3-slug}}.md` populated.
- `bosun claim session-3 docs/review/{{round-slug}}-{{path-3-slug}}.md`
- `bosun done session-3 -m "reviewed {{path-3}}: <N> findings"`
