package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorktreeGitdirCorruption_HealthyAndBroken pins the rescue-side
// guard added for v0.6.2 ask #5: a real `git worktree add` produces a
// fully-formed admin dir, so the helper returns nil; deleting the
// gitdir's HEAD (the v0.6.2 crash footprint) surfaces a descriptive
// error so cmd_rescue can refuse with an actionable recovery hint.
func TestWorktreeGitdirCorruption_HealthyAndBroken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	mustRun(t, repo, "git", "init", "-q", "-b", "main")
	mustRun(t, repo, "git", "config", "user.email", "test@example.com")
	mustRun(t, repo, "git", "config", "user.name", "Test User")
	mustRun(t, repo, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", "README.md")
	mustRun(t, repo, "git", "commit", "-q", "-m", "initial")

	worktree := filepath.Join(t.TempDir(), "wt-1")
	mustRun(t, repo, "git", "worktree", "add", "-q", "-b", "feature/x", worktree)

	// Healthy worktree: no corruption.
	if err := WorktreeGitdirCorruption(repo, worktree); err != nil {
		t.Fatalf("healthy worktree reported corrupted: %v", err)
	}

	// Simulate the v0.6.2 crash: gitdir admin dir lost its HEAD.
	gitdir := filepath.Join(repo, ".git", "worktrees", "wt-1")
	if err := os.Remove(filepath.Join(gitdir, "HEAD")); err != nil {
		t.Fatalf("remove HEAD: %v", err)
	}

	err := WorktreeGitdirCorruption(repo, worktree)
	if err == nil {
		t.Fatal("expected corruption error after deleting HEAD, got nil")
	}
	if !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("error should mention HEAD, got: %v", err)
	}
}

// TestWorktreeGitdirCorruption_MissingDotGit returns a clear error when
// the worktree's `.git` pointer file itself is gone — useful so the
// rescue path doesn't try to read a nonexistent file.
func TestWorktreeGitdirCorruption_MissingDotGit(t *testing.T) {
	wt := t.TempDir()
	err := WorktreeGitdirCorruption(t.TempDir(), wt)
	if err == nil {
		t.Fatal("expected error for missing .git, got nil")
	}
	if !strings.Contains(err.Error(), ".git is missing") {
		t.Fatalf("error should mention .git, got: %v", err)
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s in %s: %v\n%s", name, strings.Join(args, " "), dir, err, out)
	}
}
