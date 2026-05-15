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
}

// Parse reads a plan markdown file and returns a Brief for every `## session-N`
// section it contains.
func Parse(path string) ([]Brief, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read brief plan %s: %w", path, err)
	}
	return parseContent(string(data)), nil
}

var headingRe = regexp.MustCompile(`(?mi)^##\s+session-(\d+)\s*$`)

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
		briefs = append(briefs, Brief{Session: n, Body: body})
	}
	return briefs
}

// WorkflowPreamble is the standard "how to work this session" block prepended
// to every BOSUN_BRIEF.md. Without it, agents tend to implement the work
// but skip the bosun lifecycle (commit + claim + done) — observed in the
// v0.1 dogfood session where 3 of 4 sessions "finished" but never committed.
//
// The {N} placeholder is substituted with the session number at write time.
const WorkflowPreamble = "## How to work this session\n\n" +
	"1. Read this brief in full — your assignment is in **Your assignment** below.\n" +
	"2. Implement the work. Keep changes minimal; don't refactor adjacent code.\n" +
	"3. Run `make check` from the worktree root to validate (vet + race tests + demo).\n" +
	"4. Stage and commit: `git add . && git commit -m \"...\"` — descriptive message.\n" +
	"5. Declare what you touched: `bosun claim session-{N} <paths...>` (run from this worktree).\n" +
	"6. Mark ready to merge: `bosun done session-{N} -m \"summary\"`.\n\n" +
	"Steps 3–6 are not optional — bosun won't squash-merge your work until you've\n" +
	"committed AND marked the session done. The operator monitors progress via\n" +
	"`bosun status` so the **DONE** signal is how they know you're finished.\n\n" +
	"---\n\n"

// WriteToWorktree writes the brief body as BOSUN_BRIEF.md into worktreePath,
// prefixed by the standard workflow preamble.
func WriteToWorktree(worktreePath string, b Brief) error {
	target := filepath.Join(worktreePath, "BOSUN_BRIEF.md")
	header := fmt.Sprintf("# Bosun brief — session-%d\n\n", b.Session)
	preamble := strings.ReplaceAll(WorkflowPreamble, "{N}", fmt.Sprintf("%d", b.Session))
	content := header + preamble + "## Your assignment\n\n" + b.Body + "\n"
	return os.WriteFile(target, []byte(content), 0o644)
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
