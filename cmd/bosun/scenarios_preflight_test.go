package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- v0.6.2 session-3: init stale-branch + merge load pre-flight ---

// TestScenario_InitRefusesDivergentStaleBranch covers the Sub-ask A
// classic shape: a prior round left a bosun/session-N branch on disk
// (typically because cleanup removed the worktree but not the branch),
// the branch has its own commits past base, and a fresh `bosun init N`
// must refuse rather than silently reuse the stale tip.
func TestScenario_InitRefusesDivergentStaleBranch(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Make session-1's branch diverge from main.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "stale.txt", "stale work\n")
	s.CommitIn(wt1, "stale commit")

	// Remove the worktree only (leave the branch). cleanup --force would
	// also drop the branch, so we drive git directly to land in the
	// "branch survived" shape the brief calls out.
	s.GitIn(s.repo, "worktree", "remove", "--force", wt1)

	// Sanity: branch still exists.
	s.AssertBranchExists("bosun/session-1")
	if _, err := s.BosunErr("init", "1"); err == nil {
		t.Fatal("expected refusal on stale bosun/session-1 branch")
	}

	out, _ := s.BosunErr("init", "1")
	for _, want := range []string{
		"branch bosun/session-1 already exists",
		"diverges from main",
		"git branch -D bosun/session-1",
		"--force",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal output missing %q:\n%s", want, out)
		}
	}

	// State machinery contract: the refusal happens before any
	// init.state is written, so an immediate `init --resume` would have
	// no breadcrumb to resume from. Verify init.state is absent.
	if _, err := os.Stat(filepath.Join(s.repo, ".bosun", "init.state")); err == nil {
		t.Fatal(".bosun/init.state should not exist after a refusal")
	}
}

// TestScenario_InitForceResetsStaleBranch covers the recovery path: with
// --force, the leftover branch is dropped and recreated at base HEAD
// (via the existing --force cleanup block), so init can proceed and the
// fresh worktree no longer carries the stale commit's content.
func TestScenario_InitForceResetsStaleBranch(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "stale.txt", "stale work\n")
	s.CommitIn(wt1, "stale commit")
	s.GitIn(s.repo, "worktree", "remove", "--force", wt1)
	s.AssertBranchExists("bosun/session-1")

	out := s.Bosun("init", "1", "--force")
	s.AssertContains(out, "session-1")

	// The new worktree must NOT carry the stale file — the branch was
	// reset to base HEAD before re-attachment.
	if _, err := os.Stat(filepath.Join(s.WorktreePath(1), "stale.txt")); err == nil {
		t.Fatal("stale.txt should be absent after --force reset")
	}
}

// TestScenario_InitProceedsWhenStaleBranchAtBase covers the equal-SHA
// no-op case. If the leftover branch happens to point at base HEAD (no
// divergent commits), there's nothing stale about it — proceed silently
// without printing the refusal hint or requiring --force.
func TestScenario_InitProceedsWhenStaleBranchAtBase(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Remove the worktree without committing anything — branch tip is
	// still base HEAD.
	wt1 := s.WorktreePath(1)
	s.GitIn(s.repo, "worktree", "remove", "--force", wt1)
	s.AssertBranchExists("bosun/session-1")

	out := s.Bosun("init", "1")
	s.AssertContains(out, "session-1")
	if strings.Contains(out, "already exists") {
		t.Fatalf("equal-SHA case should not trigger refusal hint:\n%s", out)
	}
}

// TestScenario_MergeWarnsOnHighLoad covers Sub-ask B: merge's pre-merge
// fsck is exactly the operation that hangs forever under load
// (v0.6.1's 11-minute fsck hang). The advisory mirrors init's wording
// so operators see the same warning surface.
func TestScenario_MergeWarnsOnHighLoad(t *testing.T) {
	t.Setenv("BOSUN_TEST_LOAD_AVERAGE", "8.0")
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "work")
	s.Bosun("done", "session-1")

	out := s.Bosun("merge")
	if !strings.Contains(out, "system load is 8.00") {
		t.Errorf("expected merge-time high-load warning, got:\n%s", out)
	}
	if !strings.Contains(out, "merge may be slow") {
		t.Errorf("expected merge-specific op label in warning, got:\n%s", out)
	}
	if !strings.Contains(out, "--no-load-check") {
		t.Errorf("expected --no-load-check hint in warning, got:\n%s", out)
	}
}

// TestScenario_MergeNoLoadCheckSkipsWarning covers the bypass: setting
// the threshold absurdly high should still produce no warning when the
// operator opts out.
func TestScenario_MergeNoLoadCheckSkipsWarning(t *testing.T) {
	t.Setenv("BOSUN_TEST_LOAD_AVERAGE", "99.0")
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "work")
	s.Bosun("done", "session-1")

	out := s.Bosun("merge", "--no-load-check")
	if strings.Contains(out, "system load is") {
		t.Errorf("--no-load-check should suppress merge warning, got:\n%s", out)
	}
}

// TestScenario_InitPersistsPerSessionAgentCommand pins Phase 1 of the
// agent-command design: a brief with mixed `(command: ...)` overrides
// produces matching state files for the overridden sessions and NO
// state file for sessions falling back to the config default. The
// no-file-when-default contract keeps the state dir uncluttered for
// the common (vanilla "claude") case.
func TestScenario_InitPersistsPerSessionAgentCommand(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", `## session-1 (command: ./scripts/ollama-claude.sh)
session-1 uses the Ollama wrapper.

## session-2
session-2 falls back to config.agent_command default.
`)

	// init persists the per-session command without --launch (we're
	// asserting on the state dir, not the spawn).
	s.Bosun("init", "2", "--brief", "plan.md")

	// session-1 should have a persisted override file.
	got, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "state", "session-1.agent-command"))
	if err != nil {
		t.Fatalf("session-1 agent-command file should exist: %v", err)
	}
	if want := "./scripts/ollama-claude.sh"; strings.TrimSpace(string(got)) != want {
		t.Errorf("session-1 agent-command body = %q, want %q", strings.TrimSpace(string(got)), want)
	}

	// session-2 should NOT have an override file — fell back to
	// config default ("claude"), no need for a state-dir entry.
	if _, err := os.Stat(filepath.Join(s.repo, ".bosun", "state", "session-2.agent-command")); err == nil {
		t.Errorf("session-2 agent-command file should not exist (no override)")
	}
}

// TestScenario_InitCommandFlagAppliesToAllSessions pins the precedence
// docs/agent-command-design.md commits to: a CLI --command flag applies
// to every session unless the brief overrides it. The brief clause
// wins for session-1; --command supplies session-2's value.
func TestScenario_InitCommandFlagAppliesToAllSessions(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", `## session-1 (command: ./brief-wins.sh)
brief clause takes precedence.

## session-2
no brief clause; --command flag should land here.
`)

	s.Bosun("init", "2", "--brief", "plan.md", "--command", "./cli-flag.sh")

	// session-1: brief clause wins.
	got1, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "state", "session-1.agent-command"))
	if err != nil {
		t.Fatalf("session-1 agent-command file should exist: %v", err)
	}
	if want := "./brief-wins.sh"; strings.TrimSpace(string(got1)) != want {
		t.Errorf("session-1: brief clause didn't win over --command (got %q, want %q)", strings.TrimSpace(string(got1)), want)
	}

	// session-2: CLI flag supplies the value (different from default).
	got2, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "state", "session-2.agent-command"))
	if err != nil {
		t.Fatalf("session-2 agent-command file should exist (CLI --command override): %v", err)
	}
	if want := "./cli-flag.sh"; strings.TrimSpace(string(got2)) != want {
		t.Errorf("session-2: --command didn't land (got %q, want %q)", strings.TrimSpace(string(got2)), want)
	}
}

// TestScenario_InitRefusesEmptyBriefBeforePreflight pins the brief-lint
// ordering contract: a malformed brief (no `## <label>` headings) must
// fail before any pre-flight runs and before any worktree mutation. We
// force a high load average so the pre-flight #2 warning *would* fire if
// reached — its absence proves the brief check ran first.
func TestScenario_InitRefusesEmptyBriefBeforePreflight(t *testing.T) {
	t.Setenv("BOSUN_TEST_LOAD_AVERAGE", "8.0")
	s := newScenario(t)
	// Brief with body but no `## label` headings — the canonical first-
	// touch failure mode Leonard trial #3 surfaced.
	s.WriteFile("plan.md", "Some prose without any session headings.\n\nMore prose.\n")

	out, err := s.BosunErr("init", "1", "--brief", "plan.md")
	if err == nil {
		t.Fatalf("init should refuse an empty-sections brief; got success:\n%s", out)
	}

	// Helpful-error contract: the message tells the operator what shape
	// the parser expects.
	for _, want := range []string{
		"## <label>",
		"Expected shape:",
		"## label-one",
		"(depends:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal output missing %q:\n%s", want, out)
		}
	}

	// Ordering contract: pre-flight #2's load warning must NOT have
	// fired. If it did, brief validation is still happening too late.
	if strings.Contains(out, "system load is") {
		t.Errorf("brief lint should run before pre-flight #2 (load check); got load warning in:\n%s", out)
	}

	// No worktree, no init.state — the refusal happens before any mutation.
	s.AssertWorktreeMissing(1)
	if _, err := os.Stat(filepath.Join(s.repo, ".bosun", "init.state")); err == nil {
		t.Fatal(".bosun/init.state should not exist after a brief-lint refusal")
	}
}
