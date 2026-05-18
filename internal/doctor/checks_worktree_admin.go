package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/phantom"
)

// CheckWorktreeAdminCorruption detects the issue #15 corruption shape
// in `.git/worktrees/`: iCloud File Provider conflict-resolution
// phantom dirs (` N` / ` (N)` suffix) and admin dirs that have been
// stripped of their top-level HEAD / commondir / gitdir files.
//
// Background: trial #3c and the v0.9 spawn bug hunt both produced this
// corruption on iCloud-managed paths. Without these files git refuses
// to recognize the worktree, bosun loses its view, and any agent
// inside the worktree gets "fatal: not a git repository" on every
// command. The corruption is invisible to the existing CheckFileSync
// (which only checks the repo path, not the admin dirs) and to
// `git worktree list` (which silently skips the broken dirs).
//
// FAIL severity rather than WARN: every multi-worktree bosun command
// will misbehave when corruption is present.
func CheckWorktreeAdminCorruption(_ context.Context, repoRoot string) Result {
	res, err := phantom.ScanWorktreeAdmin(repoRoot)
	if err != nil {
		return Result{
			Name:    "worktree-admin-integrity",
			Status:  Warn,
			Message: fmt.Sprintf("could not scan .git/worktrees/: %v", err),
		}
	}

	if len(res.PhantomDirs) == 0 && len(res.BrokenDirs) == 0 {
		return Result{
			Name:    "worktree-admin-integrity",
			Status:  Pass,
			Message: ".git/worktrees/ admin dirs intact",
		}
	}

	parts := []string{}
	if n := len(res.PhantomDirs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d phantom admin dir(s)", n))
	}
	if n := len(res.BrokenDirs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d admin dir(s) missing HEAD/commondir/gitdir", n))
	}
	msg := "worktree admin corruption: " + strings.Join(parts, ", ")

	// Build a fixer that reaps the phantoms and prunes git's view of
	// the broken admin dirs. We rescan inside the fixer so the
	// closure operates on the source-of-truth state at fix time, not
	// the snapshot taken when the check ran.
	fix := func(repoRoot string) error {
		rescan, err := phantom.ScanWorktreeAdmin(repoRoot)
		if err != nil {
			return fmt.Errorf("rescan: %w", err)
		}
		for _, p := range rescan.PhantomDirs {
			if err := os.RemoveAll(p); err != nil {
				return fmt.Errorf("remove phantom %s: %w", p, err)
			}
		}
		// Broken admin dirs can't be repaired in-place — the
		// metadata files git needs are gone and reconstructing them
		// requires info bosun doesn't have. We delete the broken
		// admin dir; the worktree's working tree (under
		// <repo>-bosun-N/) is untouched, so any uncommitted work
		// there is preserved. Operator can `bosun init --resume` or
		// rescue manually after the fix.
		for _, p := range rescan.BrokenDirs {
			if err := os.RemoveAll(p); err != nil {
				return fmt.Errorf("remove broken admin %s: %w", p, err)
			}
		}
		// git worktree prune cleans up any remaining stale entries
		// from git's internal worktree list. Best-effort: a prune
		// failure shouldn't block the rest of the fix.
		_ = exec.Command("git", "-C", repoRoot, "worktree", "prune").Run()
		return nil
	}

	return Result{
		Name:    "worktree-admin-integrity",
		Status:  Fail,
		Message: msg,
		Fix:     "remove the offending dirs under .git/worktrees/ (operator: inspect <repo>-bosun-*/ for uncommitted work first), or `bosun doctor --fix` to reap them",
		FixFn:   fix,
		FixDescription: fmt.Sprintf(
			"reaped %d phantom + %d broken admin dir(s) and pruned git's worktree list",
			len(res.PhantomDirs), len(res.BrokenDirs),
		),
	}
}

// IsICloudManagedPath reports whether repoRoot is in a directory tree
// macOS routinely syncs to iCloud. Used by `bosun init` to refuse
// operation on iCloud paths by default — issue #15 confirmed iCloud
// File Provider strips git's worktree admin metadata under load.
//
// Returns (true, reason) when the path is under either:
//
//   - ~/Library/Mobile Documents/com~apple~CloudDocs (the actual
//     iCloud Drive root)
//   - ~/Documents or ~/Desktop (default-iCloud-synced since Sierra)
//
// Returns (false, "") on Linux/Windows or when the path is clear.
func IsICloudManagedPath(repoRoot string) (bool, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, ""
	}
	clean, err := filepath.Abs(repoRoot)
	if err != nil {
		clean = repoRoot
	}
	icloudRoot := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")
	if clean == icloudRoot || strings.HasPrefix(clean, icloudRoot+string(filepath.Separator)) {
		return true, "iCloud Drive (" + icloudRoot + ")"
	}
	for _, dir := range []string{"Documents", "Desktop"} {
		candidate := filepath.Join(home, dir)
		if clean == candidate || strings.HasPrefix(clean, candidate+string(filepath.Separator)) {
			return true, "~/" + dir + " (macOS default-iCloud-synced)"
		}
	}
	return false, ""
}
