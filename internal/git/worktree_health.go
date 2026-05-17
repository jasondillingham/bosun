package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WorktreeGitdirCorruption inspects a linked worktree's gitdir layout
// and returns nil if it has the expected files, or a descriptive error
// if the gitdir is missing the pieces git itself needs to operate.
//
// Background: a v0.6.2 trial reproduced an agent crash under load that
// truncated `<repo>/.git/worktrees/<name>/` down to just `index` —
// `HEAD`, `commondir`, `gitdir`, and `config` were gone. Every
// subsequent `git -C <worktree>` call then dies with "fatal: not a
// git repository", and `bosun rescue` would proceed into a confusing
// mid-snapshot error. Detecting the broken layout up front lets
// rescue refuse with an actionable recovery hint instead.
//
// Steps:
//  1. Read `<worktreePath>/.git` (a file, not a dir, on linked worktrees).
//     Its body is `gitdir: <path>` pointing into the main repo's
//     .git/worktrees/<name>/ admin dir.
//  2. Stat HEAD and commondir inside that admin dir. Either missing
//     ⇒ corruption.
//
// Best-effort: if the worktree's `.git` file itself is a real directory
// (the main worktree case) or is missing, we return a clear error so
// the caller knows the assumption doesn't hold. A bare-repo worktree
// (a rare bosun edge case) isn't a target for rescue and surfaces the
// same way.
func WorktreeGitdirCorruption(repoRoot, worktreePath string) error {
	gitFile := filepath.Join(worktreePath, ".git")
	info, err := os.Lstat(gitFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s/.git is missing", worktreePath)
		}
		return fmt.Errorf("stat %s: %w", gitFile, err)
	}
	if info.IsDir() {
		// Main worktree — there's no linked-worktree admin dir to inspect.
		// Not "corrupted" in the sense we care about; return nil so callers
		// can keep going.
		return nil
	}

	data, err := os.ReadFile(gitFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", gitFile, err)
	}
	body := strings.TrimSpace(string(data))
	prefix := "gitdir:"
	if !strings.HasPrefix(body, prefix) {
		return fmt.Errorf("%s has unexpected contents: %q", gitFile, body)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(body, prefix))
	if gitdir == "" {
		return fmt.Errorf("%s names an empty gitdir", gitFile)
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}

	if _, err := os.Stat(gitdir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("worktree gitdir %s is missing", gitdir)
		}
		return fmt.Errorf("stat %s: %w", gitdir, err)
	}

	for _, required := range []string{"HEAD", "commondir"} {
		p := filepath.Join(gitdir, required)
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("worktree gitdir missing %s (%s)", required, p)
			}
			return fmt.Errorf("stat %s: %w", p, err)
		}
	}
	return nil
}
