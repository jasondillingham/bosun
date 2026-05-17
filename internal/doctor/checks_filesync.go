package doctor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CheckFileSync detects cloud-sync daemons that index the repository
// and create phantom-duplicate files. This is the single most common
// "right in front of us" hazard from the v0.7 dogfood loop — every
// orphan-cleanup we performed was downstream of macOS File Provider
// (iCloud Drive) duplicating files inside the worktree.
//
// Detection is heuristic by design: there's no clean cross-platform
// API for "is this path under iCloud Drive?" We probe for known
// indicators and warn when the repo path looks suspicious. Worst case
// we false-positive a warning; the operator can dismiss.
func CheckFileSync(_ context.Context, repoRoot string) Result {
	if runtime.GOOS != "darwin" {
		// Linux and Windows have their own sync daemons (rclone,
		// OneDrive, Insync, etc.) but no equally-load-bearing default
		// like macOS ~/Documents-in-iCloud. Skip with a Pass; revisit
		// if real users report drift.
		return Result{
			Name:    "filesync-icloud",
			Status:  Pass,
			Message: "non-macOS: cloud-sync detection skipped",
		}
	}

	// macOS-specific check: walk up from repoRoot looking for
	// ~/Library/Mobile Documents/com~apple~CloudDocs (iCloud Drive's
	// real path), or for a path under ~/Documents or ~/Desktop which
	// macOS aggressively syncs to iCloud by default.
	home, err := os.UserHomeDir()
	if err != nil {
		return Result{
			Name:    "filesync-icloud",
			Status:  Warn,
			Message: "could not determine home directory; skipping iCloud probe",
		}
	}

	clean, err := filepath.Abs(repoRoot)
	if err != nil {
		clean = repoRoot
	}

	// iCloud Drive proper, under ~/Library/Mobile Documents/.
	icloudRoot := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")
	if strings.HasPrefix(clean, icloudRoot) {
		return Result{
			Name:    "filesync-icloud",
			Status:  Warn,
			Message: "repository is inside iCloud Drive (~/Library/Mobile Documents/...)",
			Fix:     "move the repository out of iCloud Drive; phantom file duplication will create stale worktree artifacts",
		}
	}

	// Documents-in-iCloud and Desktop-in-iCloud are macOS defaults
	// since Sierra. We can't reliably tell whether the user has
	// disabled the iCloud-sync option, but we *can* tell whether the
	// macOS File Provider daemon is touching this tree (presence of
	// `.iCloud` companion files OR fileproviderd holding a directory).
	// As a cheap proxy: warn whenever the repo is under ~/Documents or
	// ~/Desktop and let the operator confirm.
	for _, dir := range []string{"Documents", "Desktop"} {
		candidate := filepath.Join(home, dir)
		if strings.HasPrefix(clean, candidate+string(filepath.Separator)) || clean == candidate {
			return Result{
				Name:    "filesync-icloud",
				Status:  Warn,
				Message: "repository is under ~/" + dir + "; macOS may sync it to iCloud (creates phantom files)",
				Fix:     "either disable iCloud sync for " + dir + " in System Settings, or move the repository to ~/code/ or similar",
			}
		}
	}

	return Result{
		Name:    "filesync-icloud",
		Status:  Pass,
		Message: "repository is outside known iCloud-managed paths",
	}
}
