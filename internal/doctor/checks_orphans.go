package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CheckOrphanWorktrees scans the repo's parent directory for sibling
// `<reporoot>-bosun-<label>` directories that aren't registered with
// `git worktree list`. These are the leftover artifacts from prior
// cleanups that didn't fully run — the v0.7 kickoff found four of
// them in this exact repo and they blocked `bosun init` until we
// manually renamed them out of the way.
func CheckOrphanWorktrees(ctx context.Context, repoRoot string) Result {
	parent := filepath.Dir(filepath.Clean(repoRoot))
	prefix := repoRootName(repoRoot) + "-bosun-"

	entries, err := os.ReadDir(parent)
	if err != nil {
		// Parent unreadable is mildly surprising but not a doctor failure
		// — we can't enumerate orphans, so we punt with a warn.
		return Result{
			Name:    "orphan-worktrees",
			Status:  Warn,
			Message: fmt.Sprintf("could not scan parent dir %s: %v", parent, err),
		}
	}

	// Snapshot the set of registered worktree paths so we can ignore
	// them. List with a short timeout — under fsync pressure this op
	// is one of the slow ones (we centralized timeout in v0.6.2 but
	// the doctor probe is supposed to be fast regardless).
	registered, listErr := listWorktrees(ctx, repoRoot)
	if listErr != nil {
		return Result{
			Name:    "orphan-worktrees",
			Status:  Warn,
			Message: fmt.Sprintf("could not list registered worktrees: %v", listErr),
			Fix:     "fix the underlying git error then re-run doctor",
		}
	}
	regSet := make(map[string]struct{}, len(registered))
	for _, p := range registered {
		// git emits absolute paths; canonicalize on both sides.
		regSet[filepath.Clean(p)] = struct{}{}
	}

	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		path := filepath.Clean(filepath.Join(parent, name))
		if _, ok := regSet[path]; ok {
			continue
		}
		orphans = append(orphans, path)
	}

	if len(orphans) == 0 {
		return Result{
			Name:    "orphan-worktrees",
			Status:  Pass,
			Message: "no orphan worktree directories",
		}
	}

	// Show the first 3 paths so the operator knows what to act on; tail
	// off if more.
	shown := orphans
	suffix := ""
	if len(shown) > 3 {
		shown = shown[:3]
		suffix = fmt.Sprintf(" (and %d more)", len(orphans)-3)
	}
	return Result{
		Name:    "orphan-worktrees",
		Status:  Warn,
		Message: fmt.Sprintf("%d orphan worktree dir(s): %s%s", len(orphans), strings.Join(shown, ", "), suffix),
		Fix:     "rename or remove the listed directories, or re-run with --fix once available",
	}
}

// listWorktrees runs `git worktree list --porcelain` and parses out
// each absolute worktree path. Kept in-package rather than calling
// internal/git so doctor stays a free-standing health check without
// dragging in the full git.Client surface.
func listWorktrees(ctx context.Context, repoRoot string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimSpace(strings.TrimPrefix(line, "worktree ")))
		}
	}
	return paths, nil
}
