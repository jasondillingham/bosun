# {{round-name}} — bug hunt (multi-lane audit)

Each lane focuses on a different class of issue across the whole
codebase. Output is a findings doc per lane — no production code
changes in this round. The fixes follow as a separate round once
the operator triages.

## session-1

**Goal.** Audit the codebase for {{concern-1}} issues (e.g.,
"concurrency safety: races, missing locks, channel-direction
misuse, goroutine leaks"). Write findings to
`docs/audit/{{round-slug}}-{{concern-1-slug}}.md`.

**Scope.** Whole repo, but only {{concern-1}}-class issues. If you
spot a {{concern-2}} or {{concern-3}} issue while scanning, leave a
one-line pointer in your notes for the other lanes — don't chase it.

**Methodology.**
- Start from the highest-traffic packages (read `go.mod`, look at
  `main.go`'s import tree) — bugs there matter most.
- For {{concern-1}} specifically: {{concern-1-search-strategy}}.
  E.g., for concurrency: grep for `go func`, `sync.`, channel
  operations; look for shared state passed to goroutines.
- Run `go vet ./...` and `go test -race ./...` and flag anything
  that surfaces, but don't stop there — the static + race checkers
  miss plenty.
- Read tests too. A bug that's "tested" by a test that doesn't
  actually exercise the failure path is still a bug.

**Output format.** Markdown file with sections:
- `# Audit findings: {{concern-1}}`
- `## Methodology` — what you grepped for, which packages you
  scanned, which you didn't and why.
- `## Findings` — numbered list, each entry: file:line, severity
  (blocker/major/minor/nit), short description, root cause, suggested
  fix shape (not the actual fix code — that's a separate round).
- `## Cross-lane pointers` — issues you spotted that belong to another
  lane's concern. One line each.
- `## Open questions` — anything you can't resolve from the code alone.

**Constraints.**
- Read-only on production code. The ONLY file you create or modify
  is the findings doc.
- Do not fix bugs you find. Document them and stop. Triaging is the
  operator's job, fixing is a later round.
- `{{verify-cmd}}` must still pass when you're done (it will).

**Done criteria.**
- `docs/audit/{{round-slug}}-{{concern-1-slug}}.md` populated with
  all sections.
- At least one Finding OR an explicit "scanned X files, found
  nothing, here's why my methodology was thorough."
- `bosun claim session-1 docs/audit/{{round-slug}}-{{concern-1-slug}}.md`
- `bosun done session-1 -m "audit {{concern-1}}: <N> findings"`

## session-2

**Goal.** Audit for {{concern-2}} issues (e.g., "error handling:
swallowed errors, missing context wrapping, panics that should be
errors"). Write findings to
`docs/audit/{{round-slug}}-{{concern-2-slug}}.md`.

**Methodology, output, constraints.** Same shape as session-1,
substituting {{concern-2}} and adapting the search strategy. For
error handling: grep for `_ =`, `err != nil` blocks that don't
return or wrap, `panic(`, `recover()`.

**Done criteria.**
- `docs/audit/{{round-slug}}-{{concern-2-slug}}.md` populated.
- `bosun claim session-2 docs/audit/{{round-slug}}-{{concern-2-slug}}.md`
- `bosun done session-2 -m "audit {{concern-2}}: <N> findings"`

## session-3

**Goal.** Audit for {{concern-3}} issues (e.g., "resource leaks:
unclosed files, goroutines without lifecycle, contexts without
cancel"). Write findings to
`docs/audit/{{round-slug}}-{{concern-3-slug}}.md`.

**Methodology, output, constraints.** Same shape as session-1,
substituting {{concern-3}} and adapting the search strategy. For
resource leaks: grep for `os.Open`, `http.Get`, `sql.Open`,
`context.WithCancel`; check whether each has a corresponding
close/cancel.

**Done criteria.**
- `docs/audit/{{round-slug}}-{{concern-3-slug}}.md` populated.
- `bosun claim session-3 docs/audit/{{round-slug}}-{{concern-3-slug}}.md`
- `bosun done session-3 -m "audit {{concern-3}}: <N> findings"`
