// Package brief parses a plan markdown file into per-session briefs and
// writes them as BOSUN_BRIEF.md into each session's worktree.
package brief

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const archivedPlanRelative = ".bosun/briefs/plan.last.md"

// Brief is the parsed body for a single session.
type Brief struct {
	Session int    // 1-based; 0 for named-session briefs
	Label   string // canonical session label: "session-1" or "auth"
	Body    string // verbatim markdown between this heading and the next
	// Depends lists session labels this brief depends on, parsed from
	// the optional `(depends: session-1, auth)` clause on the heading
	// line. Empty when no clause was given.
	Depends []string
	// Command, when non-empty, overrides the default agent command for
	// this session. Parsed from the optional `(command: ./wrap.sh)`
	// clause on the heading. Operator-controlled; not validated here
	// beyond non-emptiness — the launcher resolves the path at spawn
	// time. Empty means "fall back to CLI flag, then config default."
	Command string
	// Host, when non-empty, names the remote Docker endpoint to target
	// for this session. Parsed from the optional `(host: ssh://thor)`
	// clause on the heading (Phase 3, lane 1 of the remote-docker
	// plan). The string is taken verbatim and exported as
	// DOCKER_HOST in the launcher's env; the docker CLI handles the
	// transport. Empty means "fall back to --docker-host CLI flag,
	// then config.docker.hosts[0], then local docker."
	Host string
}

// Parse reads a plan markdown file and returns a Brief for every `## session-N`
// section it contains. The optional `(depends: session-X, session-Y)` clause
// on a heading sets the session's Depends list — bosun merge honors the
// order so a dependent session waits until its dependencies are merged.
//
// Plan-level errors (duplicate headings, `session-0`, self-dependency) are
// returned here rather than silently producing a degenerate brief list —
// init writes one brief per worktree and would otherwise drop the second
// occurrence on the floor.
func Parse(path string) ([]Brief, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read brief plan %s: %w", path, err)
	}
	briefs := parseContent(string(data))
	if err := ValidateBriefs(briefs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return briefs, nil
}

// ParseString is the in-memory variant of Parse. Used by the v0.9
// bosun_spawn MCP tool, which receives the brief markdown as a tool
// argument rather than as a file path. Validation is identical to
// Parse; the error wrapping just doesn't include a filename.
func ParseString(s string) ([]Brief, error) {
	briefs := parseContent(s)
	if err := ValidateBriefs(briefs); err != nil {
		return nil, fmt.Errorf("inline brief: %w", err)
	}
	return briefs, nil
}

// headingRe captures the session label and the optional clause block —
// zero or more `(key: value)` parentheticals like `(depends: x, y)` or
// `(command: ./wrap.sh)`. Clauses can appear in any order; clauseRe
// parses them out individually so the grammar stays extensible.
//
// Match groups:
//
//	1: session label (e.g. "session-3" or "auth")
//	2: full clause block (one or more "(key: value)" parens, or empty)
var headingRe = regexp.MustCompile(`(?m)^##\s+([a-z][a-z0-9-]*)((?:\s*\([a-z]+:\s*[^)]+\))*)\s*$`)

// clauseRe extracts one `(key: value)` clause from the captured clause
// block. Group 1 is the key (e.g. "depends" or "command"), group 2 is
// the raw value text — interpretation is key-specific (comma-split for
// depends; whole string for command).
var clauseRe = regexp.MustCompile(`\(([a-z]+):\s*([^)]+)\)`)

// utf8BOM is the byte-order mark some editors (notably Windows Notepad,
// Excel) prepend to UTF-8 files. Stripping it lets `## session-1` on the
// first line match headingRe — without this, the BOM is the first byte
// of "##" and the regex doesn't match. 2026-05 bug hunt #2.
const utf8BOM = "\ufeff"

func parseContent(s string) []Brief {
	// Strip UTF-8 BOM before any other processing — see utf8BOM comment.
	s = strings.TrimPrefix(s, utf8BOM)
	// Normalize line endings.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	matches := headingRe.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return nil
	}
	var briefs []Brief
	for i, m := range matches {
		end := len(s)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		label := s[m[2]:m[3]]
		number := 0
		if rest, ok := strings.CutPrefix(label, "session-"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n >= 1 {
				number = n
			}
		}
		// Body starts after the heading line.
		bodyStart := m[1]
		for bodyStart < end && s[bodyStart] == '\n' {
			bodyStart++
		}
		body := strings.TrimRight(s[bodyStart:end], "\n ")

		// m[4]/m[5] frame the clause block (group 2). Empty when no
		// clauses were given. Iterate the individual clauses with
		// clauseRe so a `(command: ...)` alongside a `(depends: ...)`
		// in either order is handled identically.
		var depends []string
		var command string
		var host string
		if m[4] >= 0 && m[5] > m[4] {
			for _, c := range clauseRe.FindAllStringSubmatch(s[m[4]:m[5]], -1) {
				key := c[1]
				val := strings.TrimSpace(c[2])
				switch key {
				case "depends":
					depends = parseDepList(val)
				case "command":
					command = val
				case "host":
					host = val
				}
				// Unknown keys are silently ignored — the parser stays
				// lenient so future clause additions don't break older
				// briefs.
			}
		}

		briefs = append(briefs, Brief{Session: number, Label: label, Body: body, Depends: depends, Command: command, Host: host})
	}
	return briefs
}

// parseDepList accepts a comma-separated list of session references
// ("session-1, auth" or "1, 3") and returns the canonical session labels
// in order. Unparseable entries are silently skipped — the parser is
// lenient because the brief plan is human-authored. A bare integer "1"
// canonicalizes to "session-1".
func parseDepList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Bare integer → session-N.
		if n, err := strconv.Atoi(p); err == nil && n >= 1 {
			out = append(out, fmt.Sprintf("session-%d", n))
			continue
		}
		// Otherwise must match the label charset (which is also what
		// `session-N` matches).
		if !labelDepRe.MatchString(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// labelDepRe matches a bare label or a `session-N` literal as it appears
// inside a brief's depends clause. Looser than the heading-label form so a
// human-typed `depends: foo` still parses; the dependency target is
// re-validated when its own heading is checked.
var labelDepRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// strictLabelRe mirrors session.ValidateLabel's regex — kept inline to
// avoid a brief → session import. Tightens the brief heading's loose
// `[a-z][a-z0-9-]*` capture by forbidding trailing dashes and consecutive
// dashes that result in awkward branch names (`bosun/auth-` etc.).
var strictLabelRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidateBriefs reports plan-level errors that parseContent's regex can't
// catch on its own: duplicate session headings (init writes one brief per
// worktree — the second silently shadowed the first), `## session-0` (the
// numeric-session form requires N >= 1; "session-0" would parse as a named
// session and collide with any real "session-0" label), self-dependencies,
// and multi-step dependency cycles (`a → b → a`) that would otherwise stall
// `bosun merge` forever.
// ErrEmptyBriefs is the sentinel returned by Parse / ParseString when
// the input has zero recognizable `## <label>` headings. Callers like
// cmd_init wrap it with a richer "Expected shape: …" message tailored
// to the operator UX; callers that just want to surface the failure
// can use the default Error() string. errors.Is(err, ErrEmptyBriefs)
// is the recommended check. 2026-05 bug-hunt #6 — used to silently
// produce zero briefs instead of failing.
var ErrEmptyBriefs = errors.New("brief contains no `## <session>` headings — check the file isn't empty, that headings use lowercase labels (e.g. `## session-1`, not `## Session-1`), and that there's no UTF-8 BOM corrupting the first line")

// ValidateBriefs runs the lane-level invariants over a parsed brief
// slice: empty-not-allowed (returns ErrEmptyBriefs — see comment on
// that sentinel for the rationale), no duplicate labels, no
// self-deps, no dep cycles.
func ValidateBriefs(briefs []Brief) error {
	if len(briefs) == 0 {
		return ErrEmptyBriefs
	}
	seen := make(map[string]struct{}, len(briefs))
	depMap := make(map[string][]string, len(briefs))
	for _, b := range briefs {
		label := briefLabel(b)
		if label == "session-0" {
			return fmt.Errorf("invalid heading `## session-0`: numeric sessions start at 1")
		}
		if !strictLabelRe.MatchString(label) {
			return fmt.Errorf("invalid session heading %q (want lowercase letters/digits separated by single dashes, starting with a letter and not ending with a dash)", label)
		}
		if _, dup := seen[label]; dup {
			return fmt.Errorf("duplicate session heading %q: each label may appear once", label)
		}
		seen[label] = struct{}{}
		for _, dep := range b.Depends {
			if dep == label {
				return fmt.Errorf("session %q depends on itself", label)
			}
		}
		if len(b.Depends) > 0 {
			depMap[label] = append([]string(nil), b.Depends...)
		}
	}
	if cycle := FindDependencyCycle(depMap); cycle != nil {
		return fmt.Errorf("dependency cycle detected: %s", strings.Join(cycle, " → "))
	}
	return nil
}

// FindDependencyCycle returns the labels in the first dependency cycle it
// finds (starting and ending at the same label so the path reads as
// `a → b → a`), or nil when the graph is acyclic. Inputs are the
// label → dependency-labels map produced by Parse / LoadArchivedDeps.
//
// Detection is iterative DFS with an on-stack marker — O(V+E) and safe
// against nodes whose deps point at labels not present in the map.
func FindDependencyCycle(depMap map[string][]string) []string {
	if len(depMap) == 0 {
		return nil
	}
	visited := make(map[string]bool)
	onStack := make(map[string]bool)
	stack := make([]string, 0, len(depMap))

	// Deterministic iteration order keeps the reported cycle stable across
	// runs (map iteration would otherwise pick a different starting node
	// each time and surface a different rotation of the same cycle).
	roots := make([]string, 0, len(depMap))
	for label := range depMap {
		roots = append(roots, label)
	}
	sort.Strings(roots)

	var dfs func(label string) []string
	dfs = func(label string) []string {
		visited[label] = true
		onStack[label] = true
		stack = append(stack, label)
		for _, dep := range depMap[label] {
			if onStack[dep] {
				for i, l := range stack {
					if l == dep {
						cycle := append([]string{}, stack[i:]...)
						return append(cycle, dep)
					}
				}
			}
			if !visited[dep] {
				if c := dfs(dep); c != nil {
					return c
				}
			}
		}
		onStack[label] = false
		stack = stack[:len(stack)-1]
		return nil
	}
	for _, label := range roots {
		if visited[label] {
			continue
		}
		if c := dfs(label); c != nil {
			return c
		}
	}
	return nil
}

// WorkflowPreamble is the standard "how to work this session" block prepended
// to every BOSUN_BRIEF.md. Without it, agents tend to implement the work
// but skip the bosun lifecycle (commit + claim + done) — observed in the
// v0.1 dogfood session where 3 of 4 sessions "finished" but never committed.
//
// Placeholders substituted at write time:
//
//	{label}      → the session label (e.g. "session-1" or "auth")
//	{verifyCmd}  → the verification command (default `make check`; override
//	               via .bosun/config.json `verify_cmd` so non-bosun projects
//	               can use their own target like `make test` or `go test ./...`)
const WorkflowPreamble = "## How to work this session\n\n" +
	"1. Read this brief in full — your assignment is in **Your assignment** below.\n" +
	"2. Implement the work. Keep changes minimal; don't refactor adjacent code.\n" +
	"3. Run `{verifyCmd}` from the worktree root to validate.\n" +
	"4. Stage and commit: `git add . && git commit -m \"...\"` — descriptive message.\n" +
	"5. Declare what you touched: `bosun claim {label} <paths...>` (run from this worktree).\n" +
	"6. Mark ready to merge: `bosun done {label} -m \"summary\"`.\n\n" +
	"Steps 3–6 are not optional — bosun won't squash-merge your work until you've\n" +
	"committed AND marked the session done. The operator monitors progress via\n" +
	"`bosun status` so the **DONE** signal is how they know you're finished.\n\n" +
	"### MCP coordination (when available)\n\n" +
	"When `BOSUN_MCP_SOCK` is set in your environment — `bosun init --launch`\n" +
	"exports it automatically — prefer the MCP tools (`bosun_claim`, `bosun_done`,\n" +
	"`bosun_check`, …) over the shell commands above. The MCP path is the\n" +
	"primary contract; the CLI commands remain a working fallback if the\n" +
	"server is unreachable.\n\n" +
	"---\n\n"

// briefLabel returns the canonical label for a Brief, falling back to the
// numbered form when only Session is set (older callers that don't yet
// populate Label).
func briefLabel(b Brief) string {
	if b.Label != "" {
		return b.Label
	}
	return fmt.Sprintf("session-%d", b.Session)
}

// WriteToWorktree writes the brief body as BOSUN_BRIEF.md into worktreePath,
// prefixed by the standard workflow preamble. verifyCmd is substituted into
// the preamble's "run this to validate" step; pass an empty string to use
// the package default ("make check"). Dependency declarations surface as a
// "Depends on" block between the preamble and the assignment so the agent
// notices them at the top of the brief.
func WriteToWorktree(worktreePath string, b Brief, verifyCmd string) error {
	if verifyCmd == "" {
		verifyCmd = "make check"
	}
	label := briefLabel(b)
	target := filepath.Join(worktreePath, "BOSUN_BRIEF.md")
	header := fmt.Sprintf("# Bosun brief — %s\n\n", label)
	preamble := strings.ReplaceAll(WorkflowPreamble, "{label}", label)
	preamble = strings.ReplaceAll(preamble, "{verifyCmd}", verifyCmd)

	var depsBlock string
	if len(b.Depends) > 0 {
		depsBlock = "## Depends on\n\n" +
			strings.Join(b.Depends, ", ") + "\n\n" +
			"`bosun merge` will hold this session until its dependencies " +
			"are merged. Don't start touching paths another session owns; " +
			"use `bosun_check` to verify before editing.\n\n"
	}

	content := header + preamble + depsBlock + "## Your assignment\n\n" + b.Body + "\n"
	return os.WriteFile(target, []byte(content), 0o644)
}

// WriteSessionPointer writes .claude/CLAUDE.md inside worktreePath. Claude
// Code reads that file automatically at session start, so it acts as an
// auto-loader that points the agent at BOSUN_BRIEF.md even when --launch
// wasn't used and no --initial-prompt was passed. label is the session
// label ("session-1" or "auth") rendered into the pointer text.
func WriteSessionPointer(worktreePath string, label string) error {
	dir := filepath.Join(worktreePath, ".claude")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	content := fmt.Sprintf("**You're in a bosun-managed worktree (%s).**\n\n"+
		"Your assignment is in `BOSUN_BRIEF.md` at the worktree root. Read it\n"+
		"first. The brief explains the bosun lifecycle (commit → claim → done)\n"+
		"you should follow when ready to hand off.\n", label)
	return os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(content), 0o644)
}

// ArchivePlan copies the original plan file into .bosun/briefs/plan.last.md
// for later reference.
func ArchivePlan(repoRoot, planPath string) error {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("read plan %s: %w", planPath, err)
	}
	dest := filepath.Join(repoRoot, archivedPlanRelative)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
	}
	return os.WriteFile(dest, data, 0o644)
}

// LookupBrief returns the brief for numeric session n, or nil if not present.
// Kept for backwards compat with numeric-only callers; named-session callers
// should use LookupBriefByLabel.
func LookupBrief(briefs []Brief, n int) *Brief {
	for i := range briefs {
		if briefs[i].Session == n {
			return &briefs[i]
		}
	}
	return nil
}

// LookupBriefByLabel returns the brief whose canonical label matches, or nil
// if not present.
func LookupBriefByLabel(briefs []Brief, label string) *Brief {
	for i := range briefs {
		if briefLabel(briefs[i]) == label {
			return &briefs[i]
		}
	}
	return nil
}

// LoadArchivedDeps returns a session-label → dependency-labels map by
// re-parsing the archived plan at .bosun/briefs/plan.last.md. A missing
// or unparseable plan returns an empty map with no error — bosun merge's
// caller treats "no deps known" the same as "no deps declared."
func LoadArchivedDeps(repoRoot string) (map[string][]string, error) {
	plan := filepath.Join(repoRoot, archivedPlanRelative)
	data, err := os.ReadFile(plan)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string][]string{}, nil
		}
		return nil, fmt.Errorf("read archived plan %s: %w", plan, err)
	}
	briefs := parseContent(string(data))
	out := make(map[string][]string, len(briefs))
	for _, b := range briefs {
		if len(b.Depends) > 0 {
			out[briefLabel(b)] = append([]string(nil), b.Depends...)
		}
	}
	return out, nil
}

// ReadFromWorktree returns the contents of BOSUN_BRIEF.md in worktreePath,
// or "" if the file does not exist.
func ReadFromWorktree(worktreePath string) (string, error) {
	target := filepath.Join(worktreePath, "BOSUN_BRIEF.md")
	data, err := os.ReadFile(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", target, err)
	}
	return string(data), nil
}
