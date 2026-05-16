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
	"strconv"
	"strings"
)

const archivedPlanRelative = ".bosun/briefs/plan.last.md"

// Brief is the parsed body for a single session.
type Brief struct {
	Session int    // 1-based
	Body    string // verbatim markdown between this heading and the next
	// Depends lists session numbers this brief depends on, parsed from
	// the optional `(depends: session-1, session-3)` clause on the
	// heading line. Empty when no clause was given.
	Depends []int
}

// Parse reads a plan markdown file and returns a Brief for every `## session-N`
// section it contains. The optional `(depends: session-X, session-Y)` clause
// on a heading sets the session's Depends list — bosun merge honors the
// order so a dependent session waits until its dependencies are merged.
func Parse(path string) ([]Brief, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read brief plan %s: %w", path, err)
	}
	return parseContent(string(data)), nil
}

// headingRe captures the session number and an optional depends clause.
// Match groups:
//
//	1: session number
//	2: full "(depends: …)" clause including the parens
//	3: just the comma-separated body inside the parens
var headingRe = regexp.MustCompile(`(?mi)^##\s+session-(\d+)(\s*\(depends:\s*([^)]+)\))?\s*$`)

func parseContent(s string) []Brief {
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
		n, _ := strconv.Atoi(s[m[2]:m[3]])
		// Body starts after the heading line.
		bodyStart := m[1]
		for bodyStart < end && s[bodyStart] == '\n' {
			bodyStart++
		}
		body := strings.TrimRight(s[bodyStart:end], "\n ")

		// m[6]/m[7] frame the comma-separated dependency list (the third
		// capture group). -1 when the optional clause is absent.
		var depends []int
		if m[6] >= 0 && m[7] > m[6] {
			depends = parseDepList(s[m[6]:m[7]])
		}

		briefs = append(briefs, Brief{Session: n, Body: body, Depends: depends})
	}
	return briefs
}

// parseDepList accepts a comma-separated list of session references
// ("session-1, session-3" or "1, 3") and returns the integer session
// numbers in order. Unparseable entries are silently skipped — the
// parser is lenient because the brief plan is human-authored.
func parseDepList(s string) []int {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, "session-")
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// WorkflowPreamble is the standard "how to work this session" block prepended
// to every BOSUN_BRIEF.md. Without it, agents tend to implement the work
// but skip the bosun lifecycle (commit + claim + done) — observed in the
// v0.1 dogfood session where 3 of 4 sessions "finished" but never committed.
//
// Placeholders substituted at write time:
//   {N}          → the session number
//   {verifyCmd}  → the verification command (default `make check`; override
//                  via .bosun/config.json `verify_cmd` so non-bosun projects
//                  can use their own target like `make test` or `go test ./...`)
const WorkflowPreamble = "## How to work this session\n\n" +
	"1. Read this brief in full — your assignment is in **Your assignment** below.\n" +
	"2. Implement the work. Keep changes minimal; don't refactor adjacent code.\n" +
	"3. Run `{verifyCmd}` from the worktree root to validate.\n" +
	"4. Stage and commit: `git add . && git commit -m \"...\"` — descriptive message.\n" +
	"5. Declare what you touched: `bosun claim session-{N} <paths...>` (run from this worktree).\n" +
	"6. Mark ready to merge: `bosun done session-{N} -m \"summary\"`.\n\n" +
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
	target := filepath.Join(worktreePath, "BOSUN_BRIEF.md")
	header := fmt.Sprintf("# Bosun brief — session-%d\n\n", b.Session)
	preamble := strings.ReplaceAll(WorkflowPreamble, "{N}", fmt.Sprintf("%d", b.Session))
	preamble = strings.ReplaceAll(preamble, "{verifyCmd}", verifyCmd)

	var depsBlock string
	if len(b.Depends) > 0 {
		names := make([]string, len(b.Depends))
		for i, d := range b.Depends {
			names[i] = fmt.Sprintf("session-%d", d)
		}
		depsBlock = "## Depends on\n\n" +
			strings.Join(names, ", ") + "\n\n" +
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
// wasn't used and no --initial-prompt was passed.
func WriteSessionPointer(worktreePath string, session int) error {
	dir := filepath.Join(worktreePath, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	content := fmt.Sprintf("**You're in a bosun-managed worktree (session-%d).**\n\n"+
		"Your assignment is in `BOSUN_BRIEF.md` at the worktree root. Read it\n"+
		"first. The brief explains the bosun lifecycle (commit → claim → done)\n"+
		"you should follow when ready to hand off.\n", session)
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
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
	}
	return os.WriteFile(dest, data, 0o644)
}

// LookupBrief returns the brief for session n, or nil if not present.
func LookupBrief(briefs []Brief, n int) *Brief {
	for i := range briefs {
		if briefs[i].Session == n {
			return &briefs[i]
		}
	}
	return nil
}

// LoadArchivedDeps returns a session-number → dependency-numbers map by
// re-parsing the archived plan at .bosun/briefs/plan.last.md. A missing
// or unparseable plan returns an empty map with no error — bosun merge's
// caller treats "no deps known" the same as "no deps declared."
func LoadArchivedDeps(repoRoot string) (map[int][]int, error) {
	plan := filepath.Join(repoRoot, archivedPlanRelative)
	data, err := os.ReadFile(plan)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[int][]int{}, nil
		}
		return nil, fmt.Errorf("read archived plan %s: %w", plan, err)
	}
	briefs := parseContent(string(data))
	out := make(map[int][]int, len(briefs))
	for _, b := range briefs {
		if len(b.Depends) > 0 {
			out[b.Session] = append([]int(nil), b.Depends...)
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
