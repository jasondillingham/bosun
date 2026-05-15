package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIntegration_EndToEnd builds the binary into a temp dir, creates a fresh
// git repo, and exercises init → claim → commit → done → merge.
//
// Skipped on Windows for v0.1 — the integration shell helpers (running bash
// inside the harness) aren't worth porting until we have Windows CI standing
// up to validate. Acceptance criteria 16 (cross-OS CI) will cover that.
func TestIntegration_EndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration test uses POSIX shell helpers; covered by CI on Linux/macOS")
	}

	// Build the bosun binary.
	tmpBin := filepath.Join(t.TempDir(), "bosun")
	build := exec.Command("go", "build", "-o", tmpBin, ".")
	build.Dir = mustCwd(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Set up a fresh repo with a single commit on main.
	repo := t.TempDir()
	gitInit(t, repo)

	// Run bosun init 2 inside the repo.
	out, err := runCmd(t, repo, tmpBin, "init", "2")
	if err != nil {
		t.Fatalf("init failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "session-1") || !strings.Contains(out, "session-2") {
		t.Fatalf("init output missing session names:\n%s", out)
	}

	// Worktrees exist.
	wt1 := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-bosun-1")
	wt2 := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-bosun-2")
	if _, err := os.Stat(wt1); err != nil {
		t.Fatalf("worktree 1 missing: %v", err)
	}
	if _, err := os.Stat(wt2); err != nil {
		t.Fatalf("worktree 2 missing: %v", err)
	}
	defer cleanupWorktrees(t, repo, wt1, wt2)

	// Make a commit in session-1's worktree.
	writeFile(t, filepath.Join(wt1, "feature.txt"), "session-1 feature\n")
	runGit(t, wt1, "add", "feature.txt")
	runGit(t, wt1, "commit", "-m", "add feature")

	// Claim a path from session-1.
	if out, err := runCmd(t, repo, tmpBin, "claim", "session-1", "feature.txt"); err != nil {
		t.Fatalf("claim failed: %v\n%s", err, out)
	}

	// Status should show session-1 ahead=1, claimed=1.
	statusOut, err := runCmd(t, repo, tmpBin, "status")
	if err != nil {
		t.Fatalf("status failed: %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, "session-1") || !strings.Contains(statusOut, "add feature") {
		t.Fatalf("status missing session-1 row:\n%s", statusOut)
	}

	// Done with session-1.
	if out, err := runCmd(t, repo, tmpBin, "done", "session-1"); err != nil {
		t.Fatalf("done failed: %v\n%s", err, out)
	}

	// list --ready should print session-1 only.
	listOut, err := runCmd(t, repo, tmpBin, "list", "--ready")
	if err != nil {
		t.Fatalf("list --ready failed: %v\n%s", err, listOut)
	}
	if strings.TrimSpace(listOut) != "session-1" {
		t.Fatalf("list --ready = %q, want session-1", strings.TrimSpace(listOut))
	}

	// merge defaults to ready-only — should merge session-1, skip session-2.
	mergeOut, err := runCmd(t, repo, tmpBin, "merge")
	if err != nil {
		t.Fatalf("merge failed: %v\n%s", err, mergeOut)
	}
	if !strings.Contains(mergeOut, "session-1: merged") {
		t.Fatalf("merge output missing session-1 success:\n%s", mergeOut)
	}
	if !strings.Contains(mergeOut, "session-2: skipped") {
		t.Fatalf("merge output missing session-2 skip:\n%s", mergeOut)
	}

	// main HEAD should now contain feature.txt.
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("feature.txt not on main after merge: %v", err)
	}

	// remove session-2 (no commits, no dirty — should succeed without --force).
	if out, err := runCmd(t, repo, tmpBin, "remove", "session-2"); err != nil {
		t.Fatalf("remove session-2 failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(wt2); err == nil {
		t.Fatalf("worktree 2 still exists after remove")
	}
}

func mustCwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func gitInit(t *testing.T, repo string) {
	t.Helper()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repo, "README.md"), "# test\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-q", "-m", "initial")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func runCmd(t *testing.T, dir, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func cleanupWorktrees(t *testing.T, repo string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", p).Run()
		_ = os.RemoveAll(p)
	}
}
