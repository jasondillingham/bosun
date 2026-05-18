package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckLegacyWorktrees_NoneFound exercises the happy path: a fresh
// repo with no sibling worktrees produces a Pass with a deterministic
// message — no surprises in `bosun doctor` output for first-time users.
func TestCheckLegacyWorktrees_NoneFound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", repo, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	r := CheckLegacyWorktrees(context.Background(), repo)
	if r.Status != Pass {
		t.Fatalf("fresh repo = %v, want Pass; message=%q", r.Status, r.Message)
	}
	if r.Name != "legacy-worktrees" {
		t.Errorf("Name = %q, want legacy-worktrees", r.Name)
	}
}

// TestCheckLegacyWorktrees_DetectsLegacy plants a real legacy-named
// worktree via `git worktree add` and asserts the check warns. The point
// is to exercise the full ListWorktrees → IsLegacyWorktreePath pipeline,
// not just the path-classification predicate.
func TestCheckLegacyWorktrees_DetectsLegacy(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "Test")
	// Need at least one commit so worktree add can branch from HEAD.
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "seed.txt")
	mustGit(t, repo, "commit", "-qm", "seed")

	legacyPath := filepath.Join(dir, "myrepo-bosun-1")
	mustGit(t, repo, "worktree", "add", "-b", "bosun/session-1", legacyPath)

	r := CheckLegacyWorktrees(context.Background(), repo)
	if r.Status != Warn {
		t.Fatalf("legacy worktree present = %v, want Warn; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1 legacy") {
		t.Errorf("message missing legacy count; got %q", r.Message)
	}
	if !strings.Contains(r.Fix, "bosun migrate") {
		t.Errorf("Fix should point at `bosun migrate`; got %q", r.Fix)
	}
	if r.FixFn != nil {
		t.Error("CheckLegacyWorktrees must NOT auto-fix — operators opt in via `bosun migrate`")
	}
}

// TestCheckLegacyWorktrees_IgnoresNewShape guarantees that the moment
// `bosun migrate` runs and the worktree is renamed to
// `<repo>-bosun-<timestamp>-<sub>`, the doctor stops warning. Without this
// the check would be a noise generator post-migration.
func TestCheckLegacyWorktrees_IgnoresNewShape(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "seed.txt")
	mustGit(t, repo, "commit", "-qm", "seed")

	newShapePath := filepath.Join(dir, "myrepo-bosun-20260518143025-1")
	mustGit(t, repo, "worktree", "add", "-b", "bosun/session-1", newShapePath)

	r := CheckLegacyWorktrees(context.Background(), repo)
	if r.Status != Pass {
		t.Fatalf("new-shape worktree = %v, want Pass; message=%q", r.Status, r.Message)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
