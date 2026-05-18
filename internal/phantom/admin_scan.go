package phantom

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// adminSuffixedPattern matches an admin-dir name with a " N" suffix —
// the iCloud File Provider conflict-resolution shape applied to a
// `.git/worktrees/<name>` directory. Example matches:
//
//	"bosun-bosun-1 2"
//	"bosun-bosun-1 (3)"
//
// Both forms (Spotlight-style " 2" and iCloud-style " (2)") show up
// in the same dirs depending on which Apple sync subsystem touched
// them last. The bosun corruption surfaced in issue #15 used the
// Spotlight shape exclusively, but documenting both is defensive.
var adminSuffixedPattern = regexp.MustCompile(`^(.+?) (\d+|\(\d+\))$`)

// AdminScanResult captures one pass over a repo's `.git/worktrees/`
// dir. Both lists are absolute paths to dirs under the repo's git
// admin tree.
type AdminScanResult struct {
	// PhantomDirs holds admin dirs whose names match adminSuffixedPattern.
	// These are not legitimate worktree admin dirs; they're iCloud
	// File Provider reconciliation artifacts. Bosun reaps them in
	// `bosun doctor --fix`.
	PhantomDirs []string
	// BrokenDirs holds admin dirs whose names look legitimate but
	// which are missing one or more of the three top-level files
	// (HEAD, commondir, gitdir) that git needs to recognize them as
	// valid worktree admins. Their worktrees won't appear in
	// `git worktree list` and bosun loses sight of them.
	BrokenDirs []string
}

// ScanWorktreeAdmin walks `<repoRoot>/.git/worktrees/` and returns
// the divergence shapes documented in
// `docs/macos-worktree-corruption-forensics.md`:
//
//  1. Phantom dirs — `<repo-bosun-N> N` or `<repo-bosun-N> (N)` —
//     iCloud File Provider conflict-resolution duplicates.
//  2. Broken dirs — admin dirs that lack any of HEAD / commondir /
//     gitdir, the three files git uses to bidirectionally link a
//     worktree to its repo.
//
// The function is read-only. Callers (bosun doctor, the --fix path)
// decide what to do with the results. Returns a zero-value result
// + nil error when `.git/worktrees/` doesn't exist (a fresh repo
// before any `bosun init`) — that's not a problem.
func ScanWorktreeAdmin(repoRoot string) (AdminScanResult, error) {
	var out AdminScanResult
	adminRoot := filepath.Join(repoRoot, ".git", "worktrees")

	entries, err := os.ReadDir(adminRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("read %s: %w", adminRoot, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(adminRoot, e.Name())
		if adminSuffixedPattern.MatchString(e.Name()) {
			out.PhantomDirs = append(out.PhantomDirs, path)
			continue
		}
		if missingAdminFiles(path) {
			out.BrokenDirs = append(out.BrokenDirs, path)
		}
	}

	return out, nil
}

// missingAdminFiles reports true when the admin dir is missing any of
// the three top-level files git creates during `git worktree add`.
// The check is read-only and OS-error-tolerant — any stat error
// counts as "missing" (the file isn't accessible, which is the same
// outcome from git's perspective).
func missingAdminFiles(adminDir string) bool {
	for _, name := range []string{"HEAD", "commondir", "gitdir"} {
		if _, err := os.Stat(filepath.Join(adminDir, name)); err != nil {
			return true
		}
	}
	return false
}

// PhantomAdminBaseName strips the " N" or " (N)" suffix off a phantom
// admin dir's name, returning the canonical worktree name the phantom
// is a duplicate of. Used by audit / report code that wants to group
// phantoms by their underlying worktree. Returns the original name
// unchanged when it doesn't match the suffixed pattern.
func PhantomAdminBaseName(name string) string {
	if m := adminSuffixedPattern.FindStringSubmatch(name); m != nil {
		return strings.TrimSpace(m[1])
	}
	return name
}
