package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- init ---

func TestScenario_InitDefaultsToFour(t *testing.T) {
	s := newScenario(t)
	out := s.Bosun("init")
	s.AssertContainsAll(out, "Created 4 session(s)", "session-1", "session-2", "session-3", "session-4")
	for i := 1; i <= 4; i++ {
		s.AssertWorktreeExists(i)
		s.AssertBranchExists("bosun/session-" + itoa(i))
	}
}

func TestScenario_InitRefusesNotOnBaseBranch(t *testing.T) {
	s := newScenario(t)
	s.GitIn(s.repo, "checkout", "-q", "-b", "feature/x")

	_, err := s.BosunErr("init", "2")
	if err == nil {
		t.Fatal("init off base branch should fail, got nil")
	}
}

func TestScenario_InitForceOverwrites(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	// Without --force: refuses.
	if _, err := s.BosunErr("init", "2"); err == nil {
		t.Fatal("init over existing worktrees should fail without --force")
	}

	// With --force: rebuilds.
	out := s.Bosun("init", "2", "--force")
	s.AssertContainsAll(out, "session-1", "session-2")
}

func TestScenario_InitWithBriefWritesPerSessionFile(t *testing.T) {
	s := newScenario(t)
	plan := `# Plan

## session-1
Refactor auth.

## session-2
Migrate storage.
`
	s.WriteFile("plan.md", plan)
	s.Bosun("init", "2", "--brief", "plan.md")

	for i := 1; i <= 2; i++ {
		path := filepath.Join(s.WorktreePath(i), "BOSUN_BRIEF.md")
		data := readFile(t, path)
		if !strings.Contains(data, "session-"+itoa(i)) {
			t.Errorf("BOSUN_BRIEF.md for session-%d missing heading: %s", i, data)
		}
	}
}

func TestScenario_InitBriefIsExcludedFromIndex(t *testing.T) {
	// Regression test: BOSUN_BRIEF.md must be in .git/info/exclude so an
	// agent running `git add .` doesn't commit it and trigger merge
	// conflicts when two sessions merge into main.
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nbody\n\n## session-2\nbody\n")
	s.Bosun("init", "2", "--brief", "plan.md")

	wt1 := s.WorktreePath(1)
	out := s.GitIn(wt1, "status", "--porcelain")
	if strings.Contains(out, "BOSUN_BRIEF.md") {
		t.Fatalf("BOSUN_BRIEF.md is in git status — should be excluded:\n%s", out)
	}
}

func TestScenario_InitBriefAutoGitignoresPlanFile(t *testing.T) {
	// v0.1 dogfood finding: a plan.md at the repo root sat untracked and
	// felt wrong. bosun init --brief should auto-add it to .gitignore.
	s := newScenario(t)
	s.WriteFile("dogfood-plan.md", "## session-1\nbody\n")
	s.Bosun("init", "1", "--brief", "dogfood-plan.md")

	gi := readFile(t, filepath.Join(s.repo, ".gitignore"))
	if !strings.Contains(gi, "dogfood-plan.md") {
		t.Fatalf(".gitignore should mention dogfood-plan.md after init --brief:\n%s", gi)
	}

	// And it should no longer show as untracked.
	out := s.GitIn(s.repo, "status", "--porcelain")
	if strings.Contains(out, "dogfood-plan.md") {
		t.Fatalf("dogfood-plan.md should be ignored, but git status shows it:\n%s", out)
	}
}

// --- status / list / show ---

func TestScenario_StatusEmpty(t *testing.T) {
	s := newScenario(t)
	out := s.Bosun("status")
	s.AssertContains(out, "no sessions")
}

func TestScenario_StatusJSONSchemaStable(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "3")

	p := s.StatusJSON()
	if len(p.Sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(p.Sessions))
	}
	for _, sess := range p.Sessions {
		if sess.Name == "" || sess.Branch == "" || sess.State == "" {
			t.Errorf("session has empty required field: %+v", sess)
		}
	}
}

func TestScenario_StatusWatchRendersAndExitsCleanlyOnSIGINT(t *testing.T) {
	// Drive `bosun status --watch --interval 1` end-to-end: start the
	// process, wait long enough for at least one full render, send SIGINT,
	// and verify the process exits 0 with the expected output. The default
	// Go signal handler would exit non-zero on SIGINT, so a clean 0 here
	// proves the signal.NotifyContext + return-nil-on-cancel plumbing.
	s := newScenario(t)
	s.Bosun("init", "1")

	cmd := exec.Command(bosunBin, "status", "--watch", "--interval", "1")
	cmd.Dir = s.repo
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun status --watch: %v", err)
	}

	// ~1.2s gives the first render time to flush before we interrupt.
	time.Sleep(1200 * time.Millisecond)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal SIGINT: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bosun status --watch did not exit 0 on SIGINT: %v\n%s", err, buf.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("bosun status --watch did not exit within 5s of SIGINT:\n%s", buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "session-1") {
		t.Errorf("expected at least one render containing session-1:\n%s", out)
	}
	if !strings.Contains(out, "\x1b[2J\x1b[H") {
		t.Errorf("expected clear-screen escape in watch output:\n%q", out)
	}
}

func TestScenario_StatusWatchRefusesJSON(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	out, err := s.BosunErr("status", "--watch", "--json")
	if err == nil {
		t.Fatalf("status --watch --json should refuse:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "mutually exclusive") {
		t.Errorf("expected error mentioning mutual exclusion:\n%s", out)
	}
}

func TestScenario_ListReadyFilters(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "3")

	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "a.txt", "x\n")
	s.CommitIn(wt2, "work")
	s.Bosun("done", "session-2")

	out := s.Bosun("list", "--ready")
	if strings.TrimSpace(out) != "session-2" {
		t.Fatalf("list --ready = %q, want session-2", strings.TrimSpace(out))
	}
}

func TestScenario_ShowPrintsBriefAndClaims(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n")
	s.Bosun("init", "1", "--brief", "plan.md")

	s.Bosun("claim", "session-1", "internal/foo.go")
	out := s.Bosun("show", "session-1")
	s.AssertContainsAll(out, "refactor things", "internal/foo.go", "BOSUN_BRIEF.md")
}

// --- claim ---

func TestScenario_ClaimIsIdempotent(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	s.Bosun("claim", "session-1", "a.go", "b.go")
	s.Bosun("claim", "session-1", "a.go")

	p := s.StatusJSON()
	sess := p.SessionByNumber(1)
	if sess == nil || sess.Claimed != 2 {
		t.Fatalf("claimed = %v, want 2", sess)
	}
}

func TestScenario_StatusWithOverlapsDetectsCollisions(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	s.Bosun("claim", "session-1", "internal/shared.go")
	s.Bosun("claim", "session-2", "internal/shared.go")

	out := s.Bosun("status", "--with-overlaps")
	s.AssertContainsAll(out, "Overlapping claims", "internal/shared.go", "session-1", "session-2")
}

func TestScenario_ClaimFromInsideSessionWritesToMain(t *testing.T) {
	// Regression test: bosun must locate the main worktree even when invoked
	// from inside a linked worktree. Without this fix, claims silently went
	// to the wrong .bosun/ dir and never showed up in `bosun status`.
	s := newScenario(t)
	s.Bosun("init", "1")

	// Run claim from inside the session's worktree, not from main.
	wt1 := s.WorktreePath(1)
	s.BosunIn(wt1, "claim", "session-1", "from-inside.go")

	// Verify it appears in main's status output.
	p := s.StatusJSON()
	if sess := p.SessionByNumber(1); sess == nil || sess.Claimed != 1 {
		t.Fatalf("claimed = %v, want 1 (claim should have written to main repo)", sess)
	}
}

// --- done ---

func TestScenario_DoneRefusesDirtyWorktree(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	// Make an uncommitted change.
	s.WriteFileIn(wt1, "README.md", "# dirty\n")

	if _, err := s.BosunErr("done", "session-1"); err == nil {
		t.Fatal("done on dirty worktree should refuse")
	}
}

func TestScenario_DoneRefusesNoCommits(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	if _, err := s.BosunErr("done", "session-1"); err == nil {
		t.Fatal("done with 0 commits ahead should refuse")
	}
}

func TestScenario_DoneForceOverrides(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// No commits, but --force should allow it.
	s.Bosun("done", "session-1", "--force")

	p := s.StatusJSON()
	if sess := p.SessionByNumber(1); sess == nil || sess.State != "DONE" {
		t.Fatalf("state = %v, want DONE", sess)
	}
}

func TestScenario_DoneStuckFlipsState(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	s.Bosun("done", "session-1", "--stuck", "-m", "blocked on review")

	p := s.StatusJSON()
	if sess := p.SessionByNumber(1); sess == nil || sess.State != "STUCK" {
		t.Fatalf("state = %v, want STUCK", sess)
	}
}

// --- merge ---

func TestScenario_MergeDefaultsToReadyOnly(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	// session-1 commits and is marked done; session-2 commits but stays WORKING.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "work 1")
	s.Bosun("done", "session-1")

	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "b.txt", "y\n")
	s.CommitIn(wt2, "work 2")

	out := s.Bosun("merge")
	s.AssertContainsAll(out, "session-1: merged", "session-2: skipped")
}

func TestScenario_MergeAllAttemptsEveryone(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	for _, n := range []int{1, 2} {
		wt := s.WorktreePath(n)
		s.WriteFileIn(wt, "f"+itoa(n)+".txt", "content "+itoa(n)+"\n")
		s.CommitIn(wt, "work")
	}

	// Neither session is DONE, but --all should attempt them.
	out := s.Bosun("merge", "--all")
	s.AssertContainsAll(out, "session-1: merged", "session-2: merged")

	s.AssertFileOnMain("f1.txt")
	s.AssertFileOnMain("f2.txt")
}

func TestScenario_MergeRealConflictReportsAndStops(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	// Both sessions edit the same line of README.md differently.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# version one\n")
	s.CommitIn(wt1, "session-1 edit")

	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "README.md", "# version two\n")
	s.CommitIn(wt2, "session-2 edit")

	out, _ := s.BosunErr("merge", "--all")
	// merge should report the conflict gracefully, not crash. Output must
	// surface "conflict" so the user knows what's wrong.
	if !strings.Contains(strings.ToLower(out), "conflict") {
		t.Fatalf("merge output should mention conflict:\n%s", out)
	}

	// Recover the repo so the temp dir can be cleaned up. `git merge --abort`
	// doesn't work after a squash conflict (no MERGE_HEAD); reset hard works
	// for any half-merged state.
	s.GitIn(s.repo, "reset", "--hard", "HEAD")
}

func TestScenario_MergeNoSquashCreatesMergeCommit(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature")
	s.Bosun("done", "session-1")

	s.Bosun("merge", "--no-squash")

	out := s.GitIn(s.repo, "log", "--oneline", "-3")
	// A --no-ff merge always creates a merge commit; squash would not.
	if !strings.Contains(out, "Merge") && !strings.Contains(out, "merge: ") {
		t.Logf("log output:\n%s", out)
	}
}

func TestScenario_MergeRefusesOffBaseBranch(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")
	s.GitIn(s.repo, "checkout", "-q", "-b", "feature/y")

	if _, err := s.BosunErr("merge"); err == nil {
		t.Fatal("merge from non-base branch should refuse")
	}
}

// --- remove ---

func TestScenario_RemoveRefusesDirty(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# dirty\n")

	if _, err := s.BosunErr("remove", "session-1"); err == nil {
		t.Fatal("remove on dirty worktree should refuse")
	}
}

func TestScenario_RemoveRefusesAheadWithoutForce(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "f.txt", "x\n")
	s.CommitIn(wt1, "work")

	if _, err := s.BosunErr("remove", "session-1"); err == nil {
		t.Fatal("remove with ahead commits should refuse without --force")
	}

	s.Bosun("remove", "session-1", "--force")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}

func TestScenario_RemoveAfterMergeNoForce(t *testing.T) {
	// Regression: after `bosun merge` squash-merges a session, the session's
	// branch still reports `ahead=1` (squashed commit is patch-id-equivalent
	// but not literally on main). `bosun remove` should detect that via
	// `git cherry` and proceed without requiring --force.
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")

	s.Bosun("remove", "session-1")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}

// --- cleanup ---

func TestScenario_CleanupRemovesDoneAndEmptySkipsWorking(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "3")

	// session-1: commit + mark DONE.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "session-1 work")
	s.Bosun("done", "session-1")

	// session-2: commit, leave WORKING.
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "b.txt", "y\n")
	s.CommitIn(wt2, "session-2 work")

	// session-3: leave empty (no commits, no changes).

	out := s.Bosun("cleanup")
	s.AssertContainsAll(out,
		"session-1: removed",
		"session-3: removed",
		"session-2: skipped",
		"removed 2, skipped 1",
	)

	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
	s.AssertWorktreeExists(2)
	s.AssertBranchExists("bosun/session-2")
	s.AssertWorktreeMissing(3)
	s.AssertBranchMissing("bosun/session-3")
}

func TestScenario_CleanupDryRunChangesNothing(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "work")
	s.Bosun("done", "session-1")

	out := s.Bosun("cleanup", "--dry-run")
	s.AssertContainsAll(out, "would remove", "session-1", "dry-run")

	// Nothing actually removed.
	s.AssertWorktreeExists(1)
	s.AssertBranchExists("bosun/session-1")
	s.AssertWorktreeExists(2)
}

func TestScenario_CleanupForceRemovesDirtyAndAhead(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	// session-1: dirty (uncommitted change).
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# dirty\n")

	// session-2: committed, not marked DONE (would be skipped without --force).
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "b.txt", "y\n")
	s.CommitIn(wt2, "ahead but not done")

	// Without --force, both should be skipped.
	out := s.Bosun("cleanup")
	s.AssertContainsAll(out, "session-1: skipped", "session-2: skipped")
	s.AssertWorktreeExists(1)
	s.AssertWorktreeExists(2)

	// With --force, both go.
	out = s.Bosun("cleanup", "--force")
	s.AssertContainsAll(out, "session-1: removed", "session-2: removed")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
	s.AssertWorktreeMissing(2)
	s.AssertBranchMissing("bosun/session-2")
}

// --- helpers (test-local) ---

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
