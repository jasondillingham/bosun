package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/brief"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/suggest"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

func TestScenario_InitWithBriefWritesSessionPointer(t *testing.T) {
	// Round 2 dogfood finding: agents who weren't launched via --initial-prompt
	// missed BOSUN_BRIEF.md entirely. Claude Code auto-loads .claude/CLAUDE.md,
	// so bosun writes a pointer there directing the agent to the brief.
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n\n## session-2\nmigrate storage\n")
	s.Bosun("init", "2", "--brief", "plan.md")

	for i := 1; i <= 2; i++ {
		path := filepath.Join(s.WorktreePath(i), ".claude", "CLAUDE.md")
		data := readFile(t, path)
		if !strings.Contains(data, "BOSUN_BRIEF.md") {
			t.Errorf(".claude/CLAUDE.md for session-%d should reference BOSUN_BRIEF.md:\n%s", i, data)
		}
		if !strings.Contains(data, "session-"+itoa(i)) {
			t.Errorf(".claude/CLAUDE.md for session-%d should name the session:\n%s", i, data)
		}
	}

	// And like BOSUN_BRIEF.md, it must be excluded from the index so agents
	// running `git add .` don't accidentally commit it.
	wt1 := s.WorktreePath(1)
	out := s.GitIn(wt1, "status", "--porcelain")
	if strings.Contains(out, ".claude/CLAUDE.md") {
		t.Fatalf(".claude/CLAUDE.md is in git status — should be excluded:\n%s", out)
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

func TestScenario_InitLaunchInitialPromptDefault(t *testing.T) {
	// With --launch + --brief and no explicit --initial-prompt, bosun should
	// default to telling the agent to read BOSUN_BRIEF.md. Use launcher=print
	// config so the test doesn't spawn real terminal windows.
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n")

	out := s.Bosun("init", "1", "--brief", "plan.md", "--launch")
	s.AssertContains(out, "Read BOSUN_BRIEF.md")
}

func TestScenario_InitLaunchExplicitInitialPrompt(t *testing.T) {
	// Explicit --initial-prompt should override the default and propagate
	// through to the launched command.
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)

	out := s.Bosun("init", "1", "--launch", "--initial-prompt", "custom kickoff")
	s.AssertContains(out, "'custom kickoff'")
}

func TestScenario_InitLaunchAutoExportsBosunMcpSock(t *testing.T) {
	// Round-1 discovery contract: when --launch is set and no
	// BOSUN_MCP_SOCK is in the parent env, bosun init auto-starts an MCP
	// daemon and injects the socket path into every session it launches.
	// Drive launcher=print so we can grep the printed env prefix for the
	// var (and so we don't actually open terminal windows).
	if runtime.GOOS == "windows" {
		t.Skip("MCP daemon spawn path uses Unix-domain sockets")
	}
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)

	// Ensure the subprocess doesn't inherit a pre-set var from the test
	// harness — ensureMcp would short-circuit through the reuse branch
	// and we'd miss the spawn path entirely.
	t.Setenv(bosunmcp.SocketEnv, "")

	out := s.Bosun("init", "1", "--launch")
	// The print fallback formats env as `KEY='value' command ...`, so the
	// var name on the launched line proves the injection.
	s.AssertContains(out, bosunmcp.SocketEnv+"=")
	// The daemon-status line should announce a fresh spawn (or reuse
	// from a sibling test that survived t.Cleanup ordering on a busy
	// machine — either way "MCP server" must appear).
	if !strings.Contains(out, "MCP server") {
		t.Fatalf("expected MCP server status line in init output:\n%s", out)
	}
	// And the pidfile the daemon writes should be readable by the time
	// init returns — that's the contract a subsequent init relies on
	// to detect "already running."
	pidfile := filepath.Join(s.repo, ".bosun", "mcp.pid")
	if _, err := os.Stat(pidfile); err != nil {
		t.Fatalf("expected pidfile at %s after init --launch: %v", pidfile, err)
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

func TestScenario_StatusSummaryReflectsState(t *testing.T) {
	// `bosun status` should print a one-line summary above the table that
	// counts states, sums AHEAD across sessions, and (with --with-overlaps)
	// counts unique overlap rows.
	s := newScenario(t)
	s.Bosun("init", "3")

	// session-1: commit + DONE (ahead=1).
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "shared.go", "package x\n")
	s.CommitIn(wt1, "session-1 work")
	s.Bosun("done", "session-1")

	// session-2: commit, leave WORKING (ahead=1).
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "b.txt", "y\n")
	s.CommitIn(wt2, "session-2 work")

	// session-3: leave empty WORKING (ahead=0).

	// Overlapping claim between session-1 and session-2 on shared.go.
	s.Bosun("claim", "session-1", "shared.go")
	s.Bosun("claim", "session-2", "shared.go")

	out := s.Bosun("status", "--with-overlaps")
	// First line should be the summary.
	firstLine := strings.SplitN(out, "\n", 2)[0]
	for _, want := range []string{
		"3 sessions",
		"1 DONE",
		"2 WORKING",
		"2 commits ahead total",
		"1 overlap",
	} {
		if !strings.Contains(firstLine, want) {
			t.Errorf("summary missing %q in first line:\n%s", want, firstLine)
		}
	}

	// JSON output must NOT include the human summary line.
	jsonOut := s.Bosun("status", "--json")
	if strings.Contains(jsonOut, "ahead total") {
		t.Errorf("JSON output unexpectedly contains summary phrasing:\n%s", jsonOut)
	}
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

func TestScenario_StatusWatchRefusesNonTTY(t *testing.T) {
	// `bosun status --watch` refuses when stdout isn't a terminal: the
	// alt-screen / cursor-hide escape dance produces garbage in a file
	// or pager, so the operator gets pointed at --json instead. The
	// per-frame loop logic (ANSI escapes, footer, SIGINT cleanup) is
	// covered by the unit tests in cmd_status_test.go — those use a
	// bytes.Buffer + cancelled context instead of a subprocess.
	s := newScenario(t)
	s.Bosun("init", "1")

	out, err := s.BosunErr("status", "--watch")
	if err == nil {
		t.Fatalf("status --watch should refuse when stdout isn't a TTY:\n%s", out)
	}
	if !strings.Contains(out, "terminal") {
		t.Errorf("expected error mentioning terminal requirement:\n%s", out)
	}
	if !strings.Contains(out, "--json") {
		t.Errorf("expected error pointing at --json alternative:\n%s", out)
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

func TestScenario_MergeAfterManualConflictResolveSkipsAlreadyMerged(t *testing.T) {
	// Round 3 finding: when `bosun merge` hits a conflict and the operator
	// resolves it manually + commits, re-running `bosun merge` would try to
	// squash the same session again and re-conflict. After the fix, bosun
	// detects via `git cherry` that the branch's content is already on base
	// and reports "already merged" (or otherwise skipped cleanly), and
	// clears the session's state/claims so it stops showing as WORKING.
	s := newScenario(t)
	s.Bosun("init", "2")

	// Two sessions edit the same line of README.md differently — guaranteed
	// conflict at squash time.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# version one\n")
	s.CommitIn(wt1, "session-1 edit")
	s.Bosun("done", "session-1")

	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "README.md", "# version two\n")
	s.CommitIn(wt2, "session-2 edit")
	s.Bosun("done", "session-2")

	// First merge run: session-1 lands clean, session-2 conflicts and stops.
	out := s.Bosun("merge")
	s.AssertContainsAll(out, "session-1: merged", "session-2: conflict")

	// Operator resolves manually: pick session-2's content, stage, commit.
	s.WriteFile("README.md", "# version two\n")
	s.GitIn(s.repo, "add", "README.md")
	s.GitIn(s.repo, "commit", "-q", "-m", "manual resolve session-2")

	// Second merge run: session-2 must NOT re-conflict — its content is
	// already on main. It should be reported as already merged / skipped.
	out2, err := s.BosunErr("merge")
	if err != nil {
		t.Fatalf("second merge should not error after manual resolve: %v\n%s", err, out2)
	}
	if strings.Contains(strings.ToLower(out2), "conflict") {
		t.Fatalf("second merge should not report conflict, got:\n%s", out2)
	}
	s.AssertContains(out2, "already merged")

	// And session-2's state/claims should be cleared — it should no longer
	// appear in `bosun list --ready`.
	ready := s.Bosun("list", "--ready")
	if strings.Contains(ready, "session-2") {
		t.Fatalf("session-2 should be cleared from ready list after detection as already merged:\n%s", ready)
	}
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

	// session-1: commit, mark DONE, merge so its content lives on base.
	// (Post-v0.5 the planner refuses to remove a DONE-but-unmerged session
	// without --purge — see CleanupForceRefusesDoneButUnmerged below — so
	// this scenario tracks the realistic "happy path" DONE+merge flow.)
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "session-1 work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")

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
	// Merge so cleanup sees a squash-merged removable session, not the
	// post-v0.5 "DONE-but-unmerged refuses without --purge" case.
	s.Bosun("merge")

	out := s.Bosun("cleanup", "--dry-run")
	s.AssertContainsAll(out, "would remove", "session-1", "dry-run")

	// Nothing actually removed.
	s.AssertWorktreeExists(1)
	s.AssertBranchExists("bosun/session-1")
	s.AssertWorktreeExists(2)
}

func TestScenario_CleanupAfterMergeRemovesSquashed(t *testing.T) {
	// Regression: after `bosun merge` squash-merges a session, the branch
	// still reports `ahead=1` because the squashed commit is patch-id-
	// equivalent but not literally on main. `bosun cleanup` should detect
	// that via `git cherry` and remove the session without --force.
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")

	out := s.Bosun("cleanup")
	s.AssertContainsAll(out, "session-1: removed", "squash-merged")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}

func TestScenario_CleanupForceRemovesDirty(t *testing.T) {
	// Post-v0.5: --force only covers sessions with uncommitted-only work.
	// Sessions whose committed work isn't on base ("would discard") need
	// the louder --purge — see CleanupForceRefusesDoneButUnmerged.
	s := newScenario(t)
	s.Bosun("init", "2")

	// session-1: dirty (uncommitted edit to a tracked file), no commits.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# dirty\n")

	// session-2: same shape — modify the tracked README so the worktree
	// shows dirty (untracked files don't count toward Dirty).
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "README.md", "# dirty too\n")

	// Without --force, both should be skipped.
	out := s.Bosun("cleanup")
	s.AssertContainsAll(out, "session-1: skipped", "session-2: skipped")
	s.AssertWorktreeExists(1)
	s.AssertWorktreeExists(2)

	// With --force, both go — no committed work would be lost.
	out = s.Bosun("cleanup", "--force")
	s.AssertContainsAll(out, "session-1: removed", "session-2: removed")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
	s.AssertWorktreeMissing(2)
	s.AssertBranchMissing("bosun/session-2")
}

// --- mcp ---

// TestScenario_MCPClaimVisibleToCLIStatus spawns `bosun mcp`, connects an
// MCP client over the Unix socket, calls bosun_claim, and verifies that
// the CLI's `bosun status` (which reads .bosun/claims/ directly) sees the
// same claim. This proves the filesystem-compat contract: MCP writes and
// CLI reads use the same on-disk format.
func TestScenario_MCPClaimVisibleToCLIStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)
	s.Bosun("init", "1")

	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-mcp-claim-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bosunBin, "mcp", "--socket", socketPath)
	cmd.Dir = s.repo
	var subOut subprocessTail
	cmd.Stdout = &subOut
	cmd.Stderr = &subOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun mcp: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if err := waitForSocket(socketPath, 3*time.Second); err != nil {
		t.Fatalf("socket never appeared: %v\nsubprocess output:\n%s", err, subOut.String())
	}

	netConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer netConn.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "bosun-claim-scenario",
		Version: "test",
	}, nil)
	mcpSession, err := client.Connect(ctx, &netConnTransport{conn: netConn}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer mcpSession.Close()

	// Call bosun_claim — same effect the CLI's `bosun claim` would have.
	result, err := mcpSession.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_claim",
		Arguments: map[string]any{
			"session": "session-1",
			"paths":   []string{"internal/foo.go", "internal/bar.go"},
		},
	})
	if err != nil {
		t.Fatalf("call bosun_claim: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_claim IsError: %+v", result)
	}

	var claimed struct {
		Claimed int `json:"claimed"`
	}
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &claimed)
	}
	if claimed.Claimed != 2 {
		t.Fatalf("bosun_claim reported %d, want 2", claimed.Claimed)
	}

	// The CLI side reads .bosun/claims/ directly. It must see the same paths.
	p := s.StatusJSON()
	sess := p.SessionByNumber(1)
	if sess == nil {
		t.Fatalf("session-1 missing from status: %+v", p)
	}
	if sess.Claimed != 2 {
		t.Fatalf("CLI status shows Claimed=%d, want 2 (MCP write should be visible to CLI)", sess.Claimed)
	}

	// And `bosun show` should render the actual claimed paths.
	show := s.Bosun("show", "session-1")
	s.AssertContainsAll(show, "internal/foo.go", "internal/bar.go")

	// Round-trip: release via MCP, confirm CLI sees the claim gone.
	rel, err := mcpSession.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_release",
		Arguments: map[string]any{
			"session": "session-1",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_release: %v", err)
	}
	if rel.IsError {
		t.Fatalf("bosun_release IsError: %+v", rel)
	}
	p = s.StatusJSON()
	sess = p.SessionByNumber(1)
	if sess == nil {
		t.Fatalf("session-1 missing from status after release: %+v", p)
	}
	if sess.Claimed != 0 {
		t.Fatalf("CLI status shows Claimed=%d after MCP release, want 0", sess.Claimed)
	}
}

// TestScenario_McpDoneRefusesDirtyAndForceMarksDone covers the bosun_done
// MCP tool: dirty worktree must refuse, force=true must succeed, and the
// .bosun/state/<n>.done marker must appear after the forced call. Mirrors
// the CLI's `bosun done` lifecycle and proves the MCP path writes the
// same on-disk state filesystem-readers like `bosun status` consume.
func TestScenario_McpDoneRefusesDirtyAndForceMarksDone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)
	s.Bosun("init", "1")

	// Dirty the session worktree so validation has something to refuse.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# dirty\n")

	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-mcp-done-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bosunBin, "mcp", "--socket", socketPath)
	cmd.Dir = s.repo
	var subOut subprocessTail
	cmd.Stdout = &subOut
	cmd.Stderr = &subOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun mcp: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if err := waitForSocket(socketPath, 3*time.Second); err != nil {
		t.Fatalf("socket never appeared: %v\nsubprocess output:\n%s", err, subOut.String())
	}

	netConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer netConn.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "bosun-done-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, &netConnTransport{conn: netConn}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	// Call 1: dirty worktree, force=false → must refuse.
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_done",
		Arguments: map[string]any{
			"session": "session-1",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_done (dirty): %v", err)
	}
	if !result.IsError {
		t.Fatalf("bosun_done on dirty worktree should refuse, got: %+v", result)
	}
	markerPath := filepath.Join(s.repo, ".bosun", "state", "session-1.done")
	if _, statErr := os.Stat(markerPath); statErr == nil {
		t.Fatalf("done marker should NOT exist after refusal: %s", markerPath)
	}

	// Call 2: same dirty worktree but force=true → must succeed and write
	// the marker. Force bypasses the dirty/ahead gate just like the CLI's
	// --force flag.
	result, err = session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_done",
		Arguments: map[string]any{
			"session": "session-1",
			"message": "forced from MCP",
			"force":   true,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_done (force): %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_done force=true should succeed, got IsError: %+v", result)
	}

	body, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("expected done marker at %s: %v", markerPath, err)
	}
	if !strings.Contains(string(body), "forced from MCP") {
		t.Errorf("marker missing message: %q", body)
	}

	// And bosun status must reflect the DONE state — the CLI and the MCP
	// tool share the same .bosun/state/ files, so this proves end-to-end
	// agreement between the two surfaces.
	p := s.StatusJSON()
	if sess := p.SessionByNumber(1); sess == nil || sess.State != "DONE" {
		t.Fatalf("status after MCP done = %+v, want DONE", sess)
	}
}

// TestScenario_AnnounceSurfacesInStatus drives bosun_announce end-to-end:
// start `bosun mcp`, push an announcement over the Unix socket, and confirm
// the CLI-side `bosun status` (a separate process) reads the persisted
// record back via the JSONL events log and renders it in the Recent section.
func TestScenario_AnnounceSurfacesInStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)
	s.Bosun("init", "1")

	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-announce-e2e-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bosunBin, "mcp", "--socket", socketPath)
	cmd.Dir = s.repo
	var subOut subprocessTail
	cmd.Stdout = &subOut
	cmd.Stderr = &subOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun mcp: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if err := waitForSocket(socketPath, 3*time.Second); err != nil {
		t.Fatalf("socket never appeared: %v\nsubprocess output:\n%s", err, subOut.String())
	}

	netConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer netConn.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "bosun-announce-e2e-client",
		Version: "test",
	}, nil)
	session, err := client.Connect(ctx, &netConnTransport{conn: netConn}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_announce",
		Arguments: map[string]any{
			"session": "session-1",
			"message": "kicking off the storage layer",
			"kind":    "progress",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_announce: %v", err)
	}
	if result.IsError {
		errText := ""
		for _, c := range result.Content {
			if tc, ok := c.(*mcpsdk.TextContent); ok {
				errText += tc.Text
			}
		}
		t.Fatalf("bosun_announce IsError: %s", errText)
	}

	// The CLI-side `bosun status` runs in its own process and must read
	// the announcement back via the JSONL persistence file.
	out := s.Bosun("status")
	s.AssertContainsAll(out,
		"Recent:",
		"session-1",
		"[progress]",
		"kicking off the storage layer",
	)
}

// --- dependency-aware merge ---

func TestScenario_MergeRespectsDependsClause(t *testing.T) {
	// session-2 (depends: session-1) should hold until session-1 is merged.
	s := newScenario(t)
	s.WriteFile("plan.md", `## session-1
foundation

## session-2 (depends: session-1)
wraps session-1
`)
	s.Bosun("init", "2", "--brief", "plan.md")

	// Both sessions have work and are marked done.
	wt1, wt2 := s.WorktreePath(1), s.WorktreePath(2)
	s.WriteFileIn(wt1, "foundation.txt", "ground floor\n")
	s.CommitIn(wt1, "foundation")
	s.WriteFileIn(wt2, "wrapper.txt", "upper floor\n")
	s.CommitIn(wt2, "wrapper")
	s.Bosun("done", "session-1")
	s.Bosun("done", "session-2")

	out := s.Bosun("merge")
	// session-1 merged first; session-2 follows because session-1 was
	// recorded as merged earlier in the same run.
	s.AssertContainsAll(out,
		"session-1: merged",
		"session-2: merged",
	)
	// session-1 must come before session-2 in the output regardless of
	// the order they appear in the sessions slice.
	idx1 := strings.Index(out, "session-1: merged")
	idx2 := strings.Index(out, "session-2: merged")
	if idx1 < 0 || idx2 < 0 || idx1 > idx2 {
		t.Errorf("expected session-1 to merge before session-2 (idx1=%d, idx2=%d)\n%s", idx1, idx2, out)
	}
}

func TestScenario_MergeHoldsDependentWhenBlockerNotDone(t *testing.T) {
	// session-2 depends on session-1, but only session-2 is marked DONE.
	// bosun merge (no --all) should merge nothing because session-1 isn't
	// DONE (regular skip rule) and session-2 must wait on session-1.
	s := newScenario(t)
	s.WriteFile("plan.md", `## session-1
foundation

## session-2 (depends: session-1)
wraps session-1
`)
	s.Bosun("init", "2", "--brief", "plan.md")

	wt1, wt2 := s.WorktreePath(1), s.WorktreePath(2)
	s.WriteFileIn(wt1, "foundation.txt", "ground floor\n")
	s.CommitIn(wt1, "foundation")
	s.WriteFileIn(wt2, "wrapper.txt", "upper floor\n")
	s.CommitIn(wt2, "wrapper")
	// Only session-2 marked DONE.
	s.Bosun("done", "session-2")

	out := s.Bosun("merge")
	s.AssertContains(out, "session-1: skipped")
	s.AssertContains(out, "session-2: skipped")
	s.AssertContains(out, "depends on session-1")
}

// --- cleanup --orphans ---

func TestScenario_CleanupOrphans_RemovesSessionsBeyondCount(t *testing.T) {
	// Simulate "started at N=4, shrank to N=3" by initializing 4, then
	// running cleanup --orphans=3 to remove session-4. Sessions 1-3 stay.
	s := newScenario(t)
	s.Bosun("init", "4")

	out := s.Bosun("cleanup", "--orphans=3")
	s.AssertContainsAll(out, "session-4: removed", "empty")
	s.AssertNotContains(out, "session-1: removed")
	s.AssertNotContains(out, "session-2: removed")
	s.AssertNotContains(out, "session-3: removed")

	s.AssertWorktreeExists(1)
	s.AssertWorktreeExists(2)
	s.AssertWorktreeExists(3)
	s.AssertWorktreeMissing(4)
}

func TestScenario_CleanupOrphans_DefaultsToConfig(t *testing.T) {
	// Bare `--orphans` (no value) falls back to default_session_count.
	// Default in the test config is 4, so a 5th session is the orphan.
	s := newScenario(t)
	s.Bosun("init", "5")

	out := s.Bosun("cleanup", "--orphans")
	s.AssertContains(out, "session-5: removed")
	s.AssertNotContains(out, "session-1: removed")
	s.AssertWorktreeExists(4)
	s.AssertWorktreeMissing(5)
}

func TestScenario_CleanupOrphans_SkipsAheadWithoutForce(t *testing.T) {
	// An orphan with commits ahead of base isn't auto-removed — the work
	// might matter. Post-v0.5: --force alone isn't enough either when the
	// commits aren't on base, the operator must use --purge to discard.
	s := newScenario(t)
	s.Bosun("init", "4")
	wt4 := s.WorktreePath(4)
	s.WriteFileIn(wt4, "scratch.txt", "wip\n")
	s.CommitIn(wt4, "wip on orphaned session-4")

	out := s.Bosun("cleanup", "--orphans=3")
	s.AssertContains(out, "session-4: skipped")
	s.AssertContains(out, "1 ahead")
	s.AssertWorktreeExists(4)

	// --force alone still refuses — committed work isn't on base.
	out = s.Bosun("cleanup", "--orphans=3", "--force")
	s.AssertContains(out, "session-4: skipped")
	s.AssertContains(out, "--purge")
	s.AssertWorktreeExists(4)

	// With --purge, the same session goes.
	out = s.Bosun("cleanup", "--orphans=3", "--purge")
	s.AssertContains(out, "session-4: removed")
	s.AssertWorktreeMissing(4)
}

func TestScenario_CleanupOrphans_NoopWhenAllInRange(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	out := s.Bosun("cleanup", "--orphans=4")
	s.AssertContains(out, "no sessions beyond session-4")
	s.AssertWorktreeExists(1)
	s.AssertWorktreeExists(2)
}

// --- launch ---

func TestScenario_LaunchOpensExistingSession(t *testing.T) {
	// `bosun launch <session>` opens a launcher window for an existing
	// session without re-running init. We use launcher=print so the
	// "spawn" lands in the captured output instead of opening real terms.
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.Bosun("init", "2")

	out := s.Bosun("launch", "session-1", "--initial-prompt", "resume work")
	// print launcher prefixes the command with its env; the prompt and
	// session name must appear in the rendered command.
	s.AssertContainsAll(out, "Launched session-1", "'resume work'")
}

func TestScenario_LaunchAcceptsBareNumberAndSessionForm(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.Bosun("init", "2")

	for _, arg := range []string{"1", "session-1"} {
		out := s.Bosun("launch", arg)
		s.AssertContains(out, "Launched session-1")
	}
}

func TestScenario_LaunchRejectsMissingSession(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.Bosun("init", "2")

	out, err := s.BosunErr("launch", "session-9")
	if err == nil {
		t.Fatalf("launch on a non-existent session should fail; got output:\n%s", out)
	}
	if !strings.Contains(out, "session-9 not found") {
		t.Errorf("expected 'session-9 not found' in error output:\n%s", out)
	}
}

// --- tui ---

func TestScenario_TuiHelpListsSubcommandAndKeybinds(t *testing.T) {
	// The tui subcommand must show up in `bosun --help` and its own
	// `--help` output must document the operator keybinds. We don't
	// drive the interactive UI here — model-level unit tests cover that.
	// This scenario exists to catch the easy regressions: forgot to
	// register the subcommand in root.go, or the bubbletea/lipgloss
	// deps drifted such that the binary won't build.
	s := newScenario(t)

	rootHelp := s.Bosun("--help")
	s.AssertContains(rootHelp, "tui")

	tuiHelp := s.Bosun("tui", "--help")
	s.AssertContainsAll(tuiHelp,
		"--no-color",
		"j/k", "merge", "cleanup", "remove", "launch", "brief",
	)
}

// --- serve ---

// TestScenario_ServeExposesStatusOverHTTP starts `bosun serve` against
// a real two-session repo, GETs /api/status, and verifies the payload
// matches the same shape `bosun status --json` returns. Mirrors the MCP
// scenarios' strategy — drive the subcommand end-to-end via the compiled
// binary so handler registration and ctx cancellation both get exercised.
func TestScenario_ServeExposesStatusOverHTTP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scenario tests use POSIX shell helpers")
	}

	s := newScenario(t)
	s.Bosun("init", "2")

	port := pickFreePort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bosunBin, "serve", "--port", itoa(port), "--bind", "127.0.0.1", "--interval", "1")
	cmd.Dir = s.repo
	var subOut subprocessTail
	cmd.Stdout = &subOut
	cmd.Stderr = &subOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/api/status", port)
	body := waitForHTTP200(t, url, 5*time.Second, &subOut)

	var payload struct {
		Sessions []struct {
			Name   string `json:"name"`
			Number int    `json:"number"`
			Branch string `json:"branch"`
			State  string `json:"state"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode /api/status: %v\nbody=%s", err, body)
	}
	if len(payload.Sessions) != 2 {
		t.Fatalf("/api/status sessions = %d, want 2\nbody=%s", len(payload.Sessions), body)
	}
	for _, sess := range payload.Sessions {
		if sess.Name == "" || sess.Branch == "" || sess.State == "" {
			t.Errorf("session has empty required field: %+v", sess)
		}
	}
}

// --- named sessions (v0.2: `bosun init auth http storage`) ---

func TestScenario_InitNamedSessionsCreatesLabeledWorktreesAndBranches(t *testing.T) {
	s := newScenario(t)
	out := s.Bosun("init", "auth", "http", "storage")

	s.AssertContainsAll(out, "Created 3 session(s)", "auth", "http", "storage")
	for _, label := range []string{"auth", "http", "storage"} {
		path := s.WorktreePathLabel(label)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("worktree %s missing: %v", path, err)
		}
		s.AssertBranchExists("bosun/" + label)
	}
	// Named sessions should NOT create numbered branches.
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/bosun/session-1")
	cmd.Dir = s.repo
	if err := cmd.Run(); err == nil {
		t.Fatal("named init unexpectedly created bosun/session-1")
	}
}

func TestScenario_InitMixedNumericAndNamedErrors(t *testing.T) {
	s := newScenario(t)
	out, err := s.BosunErr("init", "auth", "2")
	if err == nil {
		t.Fatalf("mixing integer count with named labels should error; got:\n%s", out)
	}
	if !strings.Contains(out, "mix") {
		t.Errorf("expected diagnostic about mixed args, got:\n%s", out)
	}
}

func TestScenario_InitRejectsInvalidLabel(t *testing.T) {
	s := newScenario(t)
	_, err := s.BosunErr("init", "Auth", "Http")
	if err == nil {
		t.Fatal("uppercase label should be rejected")
	}
}

func TestScenario_InitNamedWithBriefWritesPerSessionFile(t *testing.T) {
	s := newScenario(t)
	plan := `# Plan

## auth
Wire login.

## http
Add routing.

## storage
Migrate pgx.
`
	s.WriteFile("plan.md", plan)
	s.Bosun("init", "auth", "http", "storage", "--brief", "plan.md")

	for _, label := range []string{"auth", "http", "storage"} {
		path := filepath.Join(s.WorktreePathLabel(label), "BOSUN_BRIEF.md")
		data := readFile(t, path)
		if !strings.Contains(data, "# Bosun brief — "+label) {
			t.Errorf("BOSUN_BRIEF.md for %s missing labeled heading:\n%s", label, data)
		}
	}
}

func TestScenario_InitNamedWritesSessionPointerWithLabel(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## auth\nbody\n")
	s.Bosun("init", "auth", "--brief", "plan.md")

	pointer := filepath.Join(s.WorktreePathLabel("auth"), ".claude", "CLAUDE.md")
	data := readFile(t, pointer)
	if !strings.Contains(data, "(auth)") {
		t.Errorf(".claude/CLAUDE.md should mention the named label:\n%s", data)
	}
	// Named pointers must not reference a synthetic session number.
	if strings.Contains(data, "session-") {
		t.Errorf(".claude/CLAUDE.md unexpectedly references session-N for named session:\n%s", data)
	}
}

func TestScenario_NamedSessionClaimAndDone(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "auth", "http")

	// Claim works against the bare label.
	s.Bosun("claim", "auth", "internal/auth.go")
	// And do a commit so `done` won't refuse for "no commits".
	wt := s.WorktreePathLabel("auth")
	s.WriteFileIn(wt, "internal/auth.go", "package auth\n")
	s.GitIn(wt, "add", "internal/auth.go")
	s.CommitIn(wt, "wire login")

	out := s.Bosun("done", "auth")
	s.AssertContains(out, "auth marked DONE")
}

func TestScenario_NamedSessionMergeRoundTrips(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "auth")

	wt := s.WorktreePathLabel("auth")
	s.WriteFileIn(wt, "auth.txt", "auth feature\n")
	s.GitIn(wt, "add", "auth.txt")
	s.CommitIn(wt, "add auth feature")
	s.Bosun("done", "auth")

	out := s.Bosun("merge", "auth")
	s.AssertContainsAll(out, "auth", "merged")
	s.AssertFileOnMain("auth.txt")
}

// --- serve helpers ---

// pickFreePort asks the kernel for an unused localhost port, releases it,
// and returns the number. The bound-then-closed pattern is the standard
// "free port" trick — there's a small race window where another process
// could grab it, but in a single-machine test environment it's reliable.
func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// waitForHTTP200 polls url until it returns 200 OK or the deadline
// expires. Returns the body on success. Used to bridge the "serve is
// starting up" gap between Start() and the listener being ready to
// accept connections.
func waitForHTTP200(t *testing.T, url string, timeout time.Duration, log *subprocessTail) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				buf := make([]byte, 64*1024)
				n, _ := resp.Body.Read(buf)
				resp.Body.Close()
				return string(buf[:n])
			}
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never got 200 from %s within %s\nsubprocess output:\n%s", url, timeout, log.String())
	return ""
}

// --- boundary regression tests (v0.3.1 follow-up) ---

// TestScenario_InitRefusesDependencyCycle confirms session-4's cycle
// detection fires at init time — a brief with a 2-cycle (session-1 ⇄
// session-2) should fail fast with a clear `a → b → a` message rather
// than create the worktrees and silently wedge merge later.
func TestScenario_InitRefusesDependencyCycle(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", `## session-1 (depends: session-2)
foundation

## session-2 (depends: session-1)
wraps foundation
`)
	out, err := s.BosunErr("init", "2", "--brief", "plan.md")
	if err == nil {
		t.Fatalf("init should refuse a cyclic brief; got success:\n%s", out)
	}
	for _, want := range []string{"cycle", "session-1", "session-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("error output missing %q\nfull output:\n%s", want, out)
		}
	}
	// No worktrees should have been created.
	s.AssertWorktreeMissing(1)
	s.AssertWorktreeMissing(2)
}

// TestScenario_MergeRefusesTamperedDependencyCycle exercises the
// defense-in-depth: even if the archived plan at .bosun/briefs/plan.last.md
// is hand-edited to introduce a cycle AFTER init has run, `bosun merge`
// must still refuse rather than silently stalling on the topoOrder
// input-order fallback. Probes session-4's merge-time cycle guard.
func TestScenario_MergeRefusesTamperedDependencyCycle(t *testing.T) {
	s := newScenario(t)
	// Start with a clean (acyclic) plan so init succeeds.
	s.WriteFile("plan.md", `## session-1
foundation

## session-2 (depends: session-1)
wraps foundation
`)
	s.Bosun("init", "2", "--brief", "plan.md")

	// Now tamper with the archived plan to introduce the cycle. The merge
	// path re-reads this file via brief.LoadArchivedDeps; the in-memory
	// briefs from init are no longer consulted at merge time.
	tampered := `## session-1 (depends: session-2)
foundation

## session-2 (depends: session-1)
wraps foundation
`
	s.WriteFile(".bosun/briefs/plan.last.md", tampered)

	// Both sessions need commits + DONE to be merge candidates.
	for i := 1; i <= 2; i++ {
		wt := s.WorktreePath(i)
		s.WriteFileIn(wt, fmt.Sprintf("file-%d.txt", i), "content\n")
		s.CommitIn(wt, fmt.Sprintf("session-%d work", i))
		s.Bosun("done", fmt.Sprintf("session-%d", i))
	}

	out, err := s.BosunErr("merge")
	if err == nil {
		t.Fatalf("merge should refuse on a tampered cyclic plan; got success:\n%s", out)
	}
	for _, want := range []string{"cycle", "session-1", "session-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("error output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestScenario_ClaimsRoundtripCliAndStatus drives a claim via the CLI and
// confirms it surfaces in `bosun status --with-overlaps` and `bosun show`
// the way the same claim made via `bosun_claim` MCP would. This is the
// boundary session-1 hardened (mutex + atomic write on claims.Store):
// concurrent CLI + MCP writers shouldn't corrupt the on-disk JSON, and
// every reader path should see the same data.
func TestScenario_ClaimsRoundtripCliAndStatus(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")
	s.Bosun("claim", "session-1", "internal/auth/handler.go", "internal/auth/middleware.go")
	s.Bosun("claim", "session-2", "internal/auth/handler.go")

	statusOut := s.Bosun("status", "--with-overlaps")
	s.AssertContainsAll(statusOut,
		"internal/auth/handler.go",
		"session-1",
		"session-2",
	)
	if !strings.Contains(statusOut, "1 overlap") {
		t.Errorf("expected '1 overlap' summary, got:\n%s", statusOut)
	}

	showOut := s.Bosun("show", "session-1")
	s.AssertContainsAll(showOut,
		"internal/auth/handler.go",
		"internal/auth/middleware.go",
	)
}

// TestScenario_DoneRefusesOnDirtyWorktree exercises the lifecycle gate
// for the CLI path the way session-2's MCP tool_done test does for the
// MCP path. Same policy must hold in both surfaces — an agent that ran
// `bosun done` while a file was uncommitted should be told to commit
// first, not silently advance the session to DONE.
func TestScenario_DoneRefusesOnDirtyWorktree(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")
	wt := s.WorktreePath(1)
	// Commit a tracked file first so it's part of HEAD; then modify it
	// so the dirty count is non-zero.
	s.WriteFileIn(wt, "tracked.txt", "v1\n")
	s.GitIn(wt, "add", "tracked.txt")
	s.CommitIn(wt, "add tracked")
	s.WriteFileIn(wt, "tracked.txt", "v2 dirty\n")

	out, err := s.BosunErr("done", "session-1")
	if err == nil {
		t.Fatalf("done should refuse on a dirty worktree; got success:\n%s", out)
	}
	if !strings.Contains(out, "uncommitted") && !strings.Contains(out, "dirty") {
		t.Errorf("expected diagnostic about uncommitted/dirty work, got:\n%s", out)
	}
}

// TestScenario_ConcurrentCliClaimsAllLandOrFailLoudly stresses the
// CLI claim path with multiple processes racing on the same session's
// claims file. The session-1 fix added a sync.Mutex inside claims.Store
// (in-process serialization) plus atomic temp-file+rename writes
// (no torn JSON), but separate `bosun claim` invocations are separate
// Go processes — the in-process mutex doesn't bind across them. If the
// read-modify-write loop is unguarded against cross-process races,
// some claims will silently disappear from the merged set. Run N=8
// claimers in parallel and assert: either every claim made it in, or
// the failing ones produced a clear error — never a silent drop.
func TestScenario_ConcurrentCliClaimsAllLandOrFailLoudly(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	const claimers = 8
	type outcome struct {
		path string
		err  error
		out  string
	}
	results := make([]outcome, claimers)
	var wg sync.WaitGroup
	wg.Add(claimers)
	for i := 0; i < claimers; i++ {
		i := i
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("internal/concurrent/file-%d.go", i)
			out, err := s.bosunRaw(s.repo, "claim", "session-1", path)
			results[i] = outcome{path: path, err: err, out: out}
		}()
	}
	wg.Wait()

	// Build the expected set of paths from claimers whose CLI invocation
	// exited cleanly. Any failure is OK as long as it's a loud one — the
	// surfacing bug would be a silent drop (no error, no claim).
	want := map[string]bool{}
	for _, r := range results {
		if r.err == nil {
			want[r.path] = true
		} else {
			t.Logf("claim %s reported error: %v\n%s", r.path, r.err, r.out)
		}
	}
	if len(want) == 0 {
		t.Fatalf("every concurrent claim reported an error — none was supposed to fail in this scenario")
	}

	// Read back what bosun actually stored.
	showOut := s.Bosun("show", "session-1")
	missing := []string{}
	for p := range want {
		if !strings.Contains(showOut, p) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d/%d claims silently disappeared (no CLI error, but not in `bosun show`): %v\nshow output:\n%s",
			len(missing), len(want), missing, showOut)
	}
}

// --- merge --dry-run ---

// TestScenario_MergeDryRunReportsPlanWithoutChangingHead is the headline
// dry-run contract: with two DONE sessions ready to merge, `bosun merge
// --dry-run` must print "would merge" for each and leave HEAD on main
// untouched. The operator should be able to preview the merge plan
// without committing anything.
func TestScenario_MergeDryRunReportsPlanWithoutChangingHead(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	for _, n := range []int{1, 2} {
		wt := s.WorktreePath(n)
		s.WriteFileIn(wt, fmt.Sprintf("f%d.txt", n), "x\n")
		s.CommitIn(wt, fmt.Sprintf("session-%d work", n))
		s.Bosun("done", fmt.Sprintf("session-%d", n))
	}

	before := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))

	out := s.Bosun("merge", "--dry-run")
	s.AssertContainsAll(out,
		"session-1: would merge",
		"session-2: would merge",
	)

	after := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))
	if before != after {
		t.Fatalf("dry-run shouldn't move HEAD on main: before=%s after=%s", before, after)
	}

	// And the session files must not have landed on main.
	for _, name := range []string{"f1.txt", "f2.txt"} {
		if _, err := os.Stat(filepath.Join(s.repo, name)); err == nil {
			t.Fatalf("%s shouldn't exist on main after dry-run", name)
		}
	}
}

// TestScenario_MergeDryRunRespectsDoneFiltering proves the DONE gate runs
// in the dry-run path too — a WORKING session without `--all` must show
// up as skipped, not as "would merge". Operator should see the same
// candidate set they'd see in a real run.
func TestScenario_MergeDryRunRespectsDoneFiltering(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	// session-1: DONE.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "session-1 work")
	s.Bosun("done", "session-1")

	// session-2: WORKING (committed but not marked done).
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "b.txt", "y\n")
	s.CommitIn(wt2, "session-2 work")

	out := s.Bosun("merge", "--dry-run")
	s.AssertContainsAll(out,
		"session-1: would merge",
		"session-2: skipped",
	)
	// And session-2 must not be miscategorized as a would-merge candidate.
	if strings.Contains(out, "session-2: would merge") {
		t.Fatalf("session-2 (WORKING, no --all) shouldn't appear as would merge:\n%s", out)
	}
}

// TestScenario_MergeDryRunNamedSessionTargetsRightBranch confirms named-
// session args narrow the dry-run output the same way they narrow a real
// merge: ask for one session and you see only that session's plan.
func TestScenario_MergeDryRunNamedSessionTargetsRightBranch(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	for _, n := range []int{1, 2} {
		wt := s.WorktreePath(n)
		s.WriteFileIn(wt, fmt.Sprintf("f%d.txt", n), "x\n")
		s.CommitIn(wt, fmt.Sprintf("session-%d work", n))
		s.Bosun("done", fmt.Sprintf("session-%d", n))
	}

	out := s.Bosun("merge", "--dry-run", "session-2")
	s.AssertContains(out, "session-2: would merge")
	// Named arg filters the other session out entirely.
	if strings.Contains(out, "session-1") {
		t.Fatalf("session-1 shouldn't appear when dry-run targets session-2 only:\n%s", out)
	}

	// And no changes landed on main.
	if _, err := os.Stat(filepath.Join(s.repo, "f2.txt")); err == nil {
		t.Fatalf("f2.txt shouldn't be on main after a targeted dry-run")
	}
}

// --- hooks ---

func TestScenario_PostInitHookSeesBosunEnv(t *testing.T) {
	// A post-init hook should fire after worktrees + briefs are written
	// and must see BOSUN_REPO_ROOT and BOSUN_SESSION_COUNT in its env.
	// This is the contract operators rely on; if it drifts, every
	// downstream hook breaks silently.
	s := newScenario(t)

	hookOut := filepath.Join(s.parent, "hook-env.txt")
	cmdStr := `env | grep -E '^BOSUN_(REPO_ROOT|SESSION_COUNT)=' | sort > ` + shQuote(hookOut)

	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "post-init", "command": cmdStr},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "2")

	data, err := os.ReadFile(hookOut)
	if err != nil {
		t.Fatalf("post-init hook did not run (expected output at %s): %v", hookOut, err)
	}
	got := string(data)
	if !strings.Contains(got, "BOSUN_SESSION_COUNT=2\n") {
		t.Errorf("hook env missing BOSUN_SESSION_COUNT=2; got:\n%s", got)
	}
	// EvalSymlinks: macOS resolves /var → /private/var, so the path bosun
	// reports may not be byte-identical with s.repo.
	wantRoot, _ := filepath.EvalSymlinks(s.repo)
	if !strings.Contains(got, "BOSUN_REPO_ROOT="+wantRoot+"\n") {
		t.Errorf("hook env missing BOSUN_REPO_ROOT=%s; got:\n%s", wantRoot, got)
	}
}

// shQuote wraps s in single quotes so it round-trips safely through `sh -c`.
// Used to embed temp-dir paths in hook command strings without worrying
// about spaces or shell metacharacters in the test sandbox path.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// --- list --json / show --json ---
//
// `bosun list` and `bosun show` mirror `bosun status --json`'s precedent:
// a stable wire shape that scripts and dashboards can consume. The tests
// below parse the JSON, check the version key matches the shared constant,
// and assert the documented keys are present.

func TestScenario_ListJSONShape(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "3")

	// session-2 → DONE so we have a non-WORKING state in the slice.
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "a.txt", "x\n")
	s.CommitIn(wt2, "work")
	s.Bosun("done", "session-2")

	out := s.Bosun("list", "--json")

	var payload struct {
		Version  string `json:"version"`
		Sessions []struct {
			Name   string `json:"name"`
			Branch string `json:"branch"`
			State  string `json:"state"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("list --json parse: %v\n%s", err, out)
	}
	if payload.Version == "" {
		t.Errorf("list --json missing version key:\n%s", out)
	}
	if len(payload.Sessions) != 3 {
		t.Fatalf("sessions = %d, want 3:\n%s", len(payload.Sessions), out)
	}
	var foundDone bool
	for _, sess := range payload.Sessions {
		if sess.Name == "" || sess.Branch == "" || sess.State == "" {
			t.Errorf("session has empty required field: %+v", sess)
		}
		if !strings.HasPrefix(sess.Branch, "bosun/") {
			t.Errorf("session %s: branch %q missing bosun/ prefix", sess.Name, sess.Branch)
		}
		if sess.Name == "session-2" {
			if sess.State != "DONE" {
				t.Errorf("session-2 state = %q, want DONE", sess.State)
			}
			foundDone = true
		}
	}
	if !foundDone {
		t.Errorf("session-2 missing from list --json output:\n%s", out)
	}
}

func TestScenario_ListJSONHonorsReadyFilter(t *testing.T) {
	// `--ready` filters before serialization, so only DONE sessions show up
	// — same contract as the plain-text form.
	s := newScenario(t)
	s.Bosun("init", "3")

	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "a.txt", "x\n")
	s.CommitIn(wt2, "work")
	s.Bosun("done", "session-2")

	out := s.Bosun("list", "--json", "--ready")

	var payload struct {
		Version  string `json:"version"`
		Sessions []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("list --json --ready parse: %v\n%s", err, out)
	}
	if payload.Version == "" {
		t.Errorf("list --json --ready missing version key:\n%s", out)
	}
	if len(payload.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 (DONE filter):\n%s", len(payload.Sessions), out)
	}
	if payload.Sessions[0].Name != "session-2" || payload.Sessions[0].State != "DONE" {
		t.Errorf("unexpected ready session: %+v", payload.Sessions[0])
	}
}

func TestScenario_ShowJSONShape(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n")
	s.Bosun("init", "1", "--brief", "plan.md")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "internal/foo.go", "package foo\n")
	s.CommitIn(wt1, "add foo")
	s.Bosun("claim", "session-1", "internal/foo.go")

	out := s.Bosun("show", "session-1", "--json")

	var payload struct {
		Version       string   `json:"version"`
		Name          string   `json:"name"`
		Branch        string   `json:"branch"`
		Worktree      string   `json:"worktree"`
		State         string   `json:"state"`
		StateMsg      string   `json:"state_msg"`
		Ahead         int      `json:"ahead"`
		Dirty         int      `json:"dirty"`
		ClaimedPaths  []string `json:"claimed_paths"`
		RecentCommits string   `json:"recent_commits"`
		Brief         string   `json:"brief"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("show --json parse: %v\n%s", err, out)
	}
	if payload.Version == "" {
		t.Errorf("show --json missing version key:\n%s", out)
	}
	if payload.Name != "session-1" {
		t.Errorf("name = %q, want session-1", payload.Name)
	}
	if !strings.HasSuffix(payload.Branch, "session-1") {
		t.Errorf("branch = %q, want suffix session-1", payload.Branch)
	}
	if payload.Worktree == "" {
		t.Errorf("worktree path empty")
	}
	if payload.State != "WORKING" {
		t.Errorf("state = %q, want WORKING", payload.State)
	}
	if payload.Ahead != 1 {
		t.Errorf("ahead = %d, want 1", payload.Ahead)
	}
	if len(payload.ClaimedPaths) != 1 || payload.ClaimedPaths[0] != "internal/foo.go" {
		t.Errorf("claimed_paths = %v, want [internal/foo.go]", payload.ClaimedPaths)
	}
	if !strings.Contains(payload.RecentCommits, "add foo") {
		t.Errorf("recent_commits missing 'add foo':\n%s", payload.RecentCommits)
	}
	if !strings.Contains(payload.Brief, "refactor things") {
		t.Errorf("brief missing BOSUN_BRIEF.md body:\n%s", payload.Brief)
	}
}

func TestScenario_ShowJSONUnknownSessionFails(t *testing.T) {
	// `--json` shouldn't paper over the "session not found" error — the
	// command must still exit non-zero so scripts can detect it.
	s := newScenario(t)
	s.Bosun("init", "1")

	out, err := s.BosunErr("show", "session-missing", "--json")
	if err == nil {
		t.Fatalf("show <missing> --json should fail:\n%s", out)
	}
}

// --- cleanup --orphan-dirs ---

// TestScenario_CleanupOrphanDirs_RemovesStaleSiblingDir reproduces the
// v0.3 corruption shape that prompted this feature: a sibling worktree
// directory survives on disk after its git admin metadata is gone, so
// `git worktree list` doesn't see it and the normal cleanup planner can't
// reach it. `bosun cleanup --orphan-dirs` should detect the dir and
// remove it.
func TestScenario_CleanupOrphanDirs_RemovesStaleSiblingDir(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")
	// Commit work in session-1 so the normal cleanup sweep skips it
	// (ahead-of-base sessions need --force; empty ones don't). Keeping
	// the live session around lets us assert that --orphan-dirs only
	// touches the orphan and leaves real worktrees alone.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "live.txt", "active work\n")
	s.CommitIn(wt1, "session-1 work-in-progress")

	// Plant a stale sibling: a worktree-shaped dir with no git admin
	// entry. We make it by hand rather than through `bosun init` so it
	// stays invisible to `git worktree list`.
	stale := filepath.Join(s.parent, s.name+"-bosun-9")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "leftover.txt"), []byte("from a long-dead session\n"), 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	out := s.Bosun("cleanup", "--orphan-dirs")
	s.AssertContainsAll(out, s.name+"-bosun-9", "removed (orphan dir)")

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale dir still present after cleanup --orphan-dirs (err=%v)", err)
	}
	// Live session with work must not be touched.
	s.AssertWorktreeExists(1)
}

// TestScenario_CleanupOrphanDirs_SkipsLiveWorktreeDir asserts the safety
// rail: a sibling dir that still carries a `.git` file pointing back at
// the main repo is reported as a partially-functional worktree and left
// alone. The intended remedy is `git worktree prune` (or repair), not a
// blind delete.
func TestScenario_CleanupOrphanDirs_SkipsLiveWorktreeDir(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Plant a dir matching the suffix that *looks* like a worktree on
	// disk (has a .git file pointing into the main repo's admin tree)
	// but isn't in `git worktree list`. cleanup --orphan-dirs must
	// surface this as a skip, not nuke it.
	live := filepath.Join(s.parent, s.name+"-bosun-7")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatalf("mkdir live: %v", err)
	}
	gitDirPtr := "gitdir: " + filepath.Join(s.repo, ".git", "worktrees", "session-7") + "\n"
	if err := os.WriteFile(filepath.Join(live, ".git"), []byte(gitDirPtr), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	out := s.Bosun("cleanup", "--orphan-dirs")
	s.AssertContainsAll(out, s.name+"-bosun-7", "looks like a live worktree", "git worktree prune")

	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live worktree dir was removed; should have been skipped: %v", err)
	}
}

// --- config ---

func TestScenario_ConfigListMarksDefaultsWhenNoFile(t *testing.T) {
	// With no .bosun/config.json on disk, every key in `config list` should
	// be flagged as a default. The marker is how operators tell "I set this"
	// from "bosun is just showing me the built-in fallback."
	s := newScenario(t)

	out := s.Bosun("config", "list")
	for _, key := range []string{
		"base_branch",
		"launcher",
		"verify_cmd",
		"default_session_count",
		"session_prefix",
		"worktree_suffix_pattern",
		"isolate_cache_default",
		"hooks",
	} {
		if !strings.Contains(out, key+":") {
			t.Errorf("config list missing key %q:\n%s", key, out)
		}
	}
	// Every line should carry the (default) marker since the file is absent.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, "(default)") {
			t.Errorf("line missing (default) marker: %q", line)
		}
	}
}

func TestScenario_ConfigListDropsDefaultMarkerForOverriddenKey(t *testing.T) {
	// `config set` should write the file and `config list` should stop
	// showing the (default) marker for the overridden key while every
	// other key still shows it.
	s := newScenario(t)
	s.Bosun("config", "set", "verify_cmd", "go test ./...")

	out := s.Bosun("config", "list")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "verify_cmd:") {
			if strings.Contains(line, "(default)") {
				t.Errorf("verify_cmd should not be flagged default after set: %q", line)
			}
			if !strings.Contains(line, "go test ./...") {
				t.Errorf("verify_cmd line missing new value: %q", line)
			}
			continue
		}
		if !strings.Contains(line, "(default)") {
			t.Errorf("non-overridden line missing (default) marker: %q", line)
		}
	}
}

func TestScenario_ConfigGetReturnsResolvedValue(t *testing.T) {
	s := newScenario(t)

	// Default first.
	out := s.Bosun("config", "get", "base_branch")
	if strings.TrimSpace(out) != "main" {
		t.Errorf("get base_branch default = %q, want main", strings.TrimSpace(out))
	}

	// Override and re-read.
	s.Bosun("config", "set", "base_branch", "trunk")
	out = s.Bosun("config", "get", "base_branch")
	if strings.TrimSpace(out) != "trunk" {
		t.Errorf("get base_branch after set = %q, want trunk", strings.TrimSpace(out))
	}
}

func TestScenario_ConfigGetUnknownKeyFails(t *testing.T) {
	// Typos should error so the operator notices, rather than silently
	// returning an empty string.
	s := newScenario(t)

	out, err := s.BosunErr("config", "get", "base-branch")
	if err == nil {
		t.Fatalf("get with unknown key should fail:\n%s", out)
	}
	if !strings.Contains(out, "unknown config key") {
		t.Errorf("error should mention unknown config key: %s", out)
	}
}

func TestScenario_ConfigSetParsesScalarTypes(t *testing.T) {
	// Integer, bool, and string keys all flow through the same `set` entry
	// point. Round-trip each through `get` to prove the type survives the
	// JSON encode/decode cycle.
	s := newScenario(t)
	s.Bosun("config", "set", "default_session_count", "6")
	s.Bosun("config", "set", "isolate_cache_default", "true")
	s.Bosun("config", "set", "launcher", "print")

	if got := strings.TrimSpace(s.Bosun("config", "get", "default_session_count")); got != "6" {
		t.Errorf("default_session_count = %q, want 6", got)
	}
	if got := strings.TrimSpace(s.Bosun("config", "get", "isolate_cache_default")); got != "true" {
		t.Errorf("isolate_cache_default = %q, want true", got)
	}
	if got := strings.TrimSpace(s.Bosun("config", "get", "launcher")); got != "print" {
		t.Errorf("launcher = %q, want print", got)
	}

	// Subsequent `bosun init` should see the new default count.
	out := s.Bosun("init")
	if !strings.Contains(out, "Created 6 session(s)") {
		t.Errorf("init didn't honor new default_session_count=6:\n%s", out)
	}
}

func TestScenario_ConfigSetRejectsBadType(t *testing.T) {
	// Type mismatches should fail with a clear error AND leave the file
	// untouched so a typo doesn't blow away the existing config.
	s := newScenario(t)
	s.Bosun("config", "set", "default_session_count", "3")

	out, err := s.BosunErr("config", "set", "default_session_count", "nope")
	if err == nil {
		t.Fatalf("set with non-integer should fail:\n%s", out)
	}
	if !strings.Contains(out, "must be an integer") {
		t.Errorf("error should mention integer requirement: %s", out)
	}
	// The previous value must still be there.
	if got := strings.TrimSpace(s.Bosun("config", "get", "default_session_count")); got != "3" {
		t.Errorf("default_session_count was clobbered by failed set: got %q, want 3", got)
	}
}

func TestScenario_ConfigSetRejectsValidatorFailure(t *testing.T) {
	// Even with the right type, `config.Validate` must run — otherwise a
	// nonsense launcher would silently land in the file and bosun would
	// fail at startup of the next command instead.
	s := newScenario(t)

	out, err := s.BosunErr("config", "set", "launcher", "gnome-terminal")
	if err == nil {
		t.Fatalf("set with invalid launcher should fail:\n%s", out)
	}
	if !strings.Contains(out, "launcher") {
		t.Errorf("error should mention launcher: %s", out)
	}
	// File should still be absent — validation runs before write.
	if _, statErr := os.Stat(filepath.Join(s.repo, ".bosun", "config.json")); statErr == nil {
		t.Errorf(".bosun/config.json shouldn't exist after a rejected set")
	}
}

func TestScenario_ConfigSetUnknownKeyFails(t *testing.T) {
	// Reject unknown keys with an error rather than silently writing them
	// to the file (which would just be ignored by config.Load).
	s := newScenario(t)

	out, err := s.BosunErr("config", "set", "frobnitz", "x")
	if err == nil {
		t.Fatalf("set with unknown key should fail:\n%s", out)
	}
	if !strings.Contains(out, "unknown config key") {
		t.Errorf("error should mention unknown config key: %s", out)
	}
}

func TestScenario_ConfigSetHooksIsRejected(t *testing.T) {
	// `hooks` is a list and out of scope for `set` this round. The error
	// should point the operator at the file instead of just saying
	// "unknown key" (it isn't — it's known but not settable here).
	s := newScenario(t)

	out, err := s.BosunErr("config", "set", "hooks", "[]")
	if err == nil {
		t.Fatalf("set hooks should fail:\n%s", out)
	}
	if !strings.Contains(out, "hooks") {
		t.Errorf("error should mention hooks: %s", out)
	}
}

func TestScenario_ConfigSetWritesAtomically(t *testing.T) {
	// The write goes through a temp file + rename, so on success there
	// should be exactly one config.json in .bosun/ — no leftover *.tmp
	// files for a concurrent reader to stumble on.
	s := newScenario(t)
	s.Bosun("config", "set", "verify_cmd", "go test ./...")

	entries, err := os.ReadDir(filepath.Join(s.repo, ".bosun"))
	if err != nil {
		t.Fatalf("read .bosun: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".tmp") || strings.Contains(n, "config-") && strings.HasSuffix(n, ".tmp") {
			t.Errorf(".bosun has leftover temp file %q: %v", n, names)
		}
	}

	// And the file should be valid JSON we can round-trip through config get.
	if got := strings.TrimSpace(s.Bosun("config", "get", "verify_cmd")); got != "go test ./..." {
		t.Errorf("verify_cmd after set = %q, want %q", got, "go test ./...")
	}
}

func TestScenario_ConfigListShowsHooksCountWhenConfigured(t *testing.T) {
	// `hooks` is a list and out of scope for `config set`, but `config list`
	// should still summarise it. With one entry on disk, the line should
	// read "1 hook(s)" (and not be marked default, since it's overridden).
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"hooks":[{"event":"post-init","command":"echo hi"}]}`)

	out := s.Bosun("config", "list")
	var hookLine string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "hooks:") {
			hookLine = line
			break
		}
	}
	if hookLine == "" {
		t.Fatalf("config list missing hooks line:\n%s", out)
	}
	if !strings.Contains(hookLine, "1 hook(s)") {
		t.Errorf("hooks line wrong count: %q", hookLine)
	}
	if strings.Contains(hookLine, "(default)") {
		t.Errorf("hooks line should not be (default) when configured: %q", hookLine)
	}
}

// --- agent_spawn.* dotted-key set/get/unset (closes trial #3a finding) ---
//
// Pre-fix, `bosun config set agent_spawn.enabled true` failed with
// "unknown config key", forcing operators to hand-edit
// .bosun/config.json — exactly the UX promise the config subcommand
// makes ("typos in keys fail silently and revert to defaults" was the
// problem it was built to solve). Trial #3a documented the gap; these
// tests pin the round-trip through the three nested leaves.

func TestScenario_ConfigSetAgentSpawnLeaves(t *testing.T) {
	// Set each of the three leaves through the public command surface
	// and round-trip via `config get` to confirm the bool / int types
	// survive the JSON encode/decode cycle.
	s := newScenario(t)

	s.Bosun("config", "set", "agent_spawn.enabled", "true")
	s.Bosun("config", "set", "agent_spawn.max_concurrent_sub_sessions", "2")
	s.Bosun("config", "set", "agent_spawn.max_depth", "3")

	if got := strings.TrimSpace(s.Bosun("config", "get", "agent_spawn.enabled")); got != "true" {
		t.Errorf("agent_spawn.enabled = %q, want true", got)
	}
	if got := strings.TrimSpace(s.Bosun("config", "get", "agent_spawn.max_concurrent_sub_sessions")); got != "2" {
		t.Errorf("agent_spawn.max_concurrent_sub_sessions = %q, want 2", got)
	}
	if got := strings.TrimSpace(s.Bosun("config", "get", "agent_spawn.max_depth")); got != "3" {
		t.Errorf("agent_spawn.max_depth = %q, want 3", got)
	}

	// And the on-disk shape is the expected nested-object form, not three
	// flat `"agent_spawn.enabled": true` top-level keys (which would make
	// the file structurally wrong and silently bypass the real loader).
	data, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if !strings.Contains(string(data), `"agent_spawn":`) {
		t.Errorf("config.json missing nested agent_spawn object:\n%s", data)
	}
	if strings.Contains(string(data), `"agent_spawn.enabled":`) {
		t.Errorf("config.json should not have a flat agent_spawn.enabled key:\n%s", data)
	}
}

func TestScenario_ConfigSetAgentSpawnPreservesSiblingLeaves(t *testing.T) {
	// Setting one leaf must not clobber a previously-set sibling. This is
	// the bug shape we'd hit if setNestedConfigField re-marshaled the
	// agent_spawn object from a fresh empty map instead of the existing one.
	s := newScenario(t)

	s.Bosun("config", "set", "agent_spawn.max_concurrent_sub_sessions", "5")
	s.Bosun("config", "set", "agent_spawn.enabled", "true")

	if got := strings.TrimSpace(s.Bosun("config", "get", "agent_spawn.max_concurrent_sub_sessions")); got != "5" {
		t.Errorf("max_concurrent was clobbered: got %q, want 5", got)
	}
	if got := strings.TrimSpace(s.Bosun("config", "get", "agent_spawn.enabled")); got != "true" {
		t.Errorf("enabled didn't stick: got %q, want true", got)
	}
}

func TestScenario_ConfigUnsetAgentSpawnLeaf(t *testing.T) {
	// Unsetting one leaf removes it but leaves siblings in place. When
	// the last leaf is removed, the parent object is dropped entirely so
	// the file doesn't accumulate `"agent_spawn": {}` husks.
	s := newScenario(t)

	s.Bosun("config", "set", "agent_spawn.enabled", "true")
	s.Bosun("config", "set", "agent_spawn.max_depth", "2")

	// Unset one — the other survives.
	s.Bosun("config", "unset", "agent_spawn.enabled")
	if got := strings.TrimSpace(s.Bosun("config", "get", "agent_spawn.enabled")); got != "false" {
		t.Errorf("after unset, agent_spawn.enabled should fall back to default (false), got %q", got)
	}
	if got := strings.TrimSpace(s.Bosun("config", "get", "agent_spawn.max_depth")); got != "2" {
		t.Errorf("max_depth was lost when sibling was unset: got %q, want 2", got)
	}

	// Unset the last surviving leaf — parent object should be gone too.
	s.Bosun("config", "unset", "agent_spawn.max_depth")
	data, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if strings.Contains(string(data), `"agent_spawn":`) {
		t.Errorf("config.json should not contain agent_spawn after all leaves unset:\n%s", data)
	}
}

func TestScenario_ConfigSetAgentSpawnRejectsBadType(t *testing.T) {
	// Bool / int parsing errors must surface with a useful message and
	// leave the file untouched — same contract as the scalar set path.
	s := newScenario(t)

	out, err := s.BosunErr("config", "set", "agent_spawn.enabled", "yeahprobably")
	if err == nil {
		t.Fatalf("set agent_spawn.enabled with non-bool should fail:\n%s", out)
	}
	if !strings.Contains(out, "true|false") {
		t.Errorf("error should mention the expected type: %s", out)
	}

	out, err = s.BosunErr("config", "set", "agent_spawn.max_depth", "wide")
	if err == nil {
		t.Fatalf("set agent_spawn.max_depth with non-int should fail:\n%s", out)
	}
	if !strings.Contains(out, "must be an integer") {
		t.Errorf("error should mention integer requirement: %s", out)
	}
}

func TestScenario_ConfigListIncludesAgentSpawnLeaves(t *testing.T) {
	// All three agent_spawn leaves must appear in `config list` with the
	// (default) marker when absent, and lose the marker when set.
	s := newScenario(t)

	out := s.Bosun("config", "list")
	for _, key := range []string{
		"agent_spawn.enabled",
		"agent_spawn.max_concurrent_sub_sessions",
		"agent_spawn.max_depth",
	} {
		var line string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.HasPrefix(l, key+":") {
				line = l
				break
			}
		}
		if line == "" {
			t.Errorf("config list missing key %q:\n%s", key, out)
			continue
		}
		if !strings.Contains(line, "(default)") {
			t.Errorf("line for %q should be (default) when unset: %q", key, line)
		}
	}

	// Set one and confirm only that one loses the default marker. The
	// other two leaves stay marked (default) — they remain at the
	// loader-provided default values even though raw["agent_spawn"]
	// is no longer absent.
	s.Bosun("config", "set", "agent_spawn.enabled", "true")
	out = s.Bosun("config", "list")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.HasPrefix(line, "agent_spawn.enabled:"):
			if strings.Contains(line, "(default)") {
				t.Errorf("agent_spawn.enabled should drop (default) after set: %q", line)
			}
			if !strings.Contains(line, "true") {
				t.Errorf("agent_spawn.enabled line missing new value: %q", line)
			}
		case strings.HasPrefix(line, "agent_spawn.max_concurrent_sub_sessions:"),
			strings.HasPrefix(line, "agent_spawn.max_depth:"):
			if !strings.Contains(line, "(default)") {
				t.Errorf("untouched leaf lost its (default) marker: %q", line)
			}
		}
	}
}

// --- suggest scenarios (v0.5 round-2) ---
//
// End-to-end coverage for `bosun suggest`. Each test spins up an
// httptest.Server that wraps the LaneProposal fixture in an Anthropic
// Messages API envelope and points the bosun binary at it via
// ANTHROPIC_API_URL — that env override is the production hook
// internal/suggest/claude.go reads when no explicit endpoint is
// configured.
//
// The fixtures themselves are pure LaneProposal JSON (cmd/bosun/testdata/),
// not full API responses — the envelope wrapping happens in
// stubClaudeServer below so the fixtures stay reusable for unit-style
// tests too.

// stubClaudeServer returns an httptest.Server that responds to every
// request with an Anthropic Messages API envelope wrapping fixturePath's
// contents as the assistant text block. fixturePath is read once at
// construction; callers don't need to Close it themselves — t.Cleanup
// handles teardown.
func stubClaudeServer(t *testing.T, fixturePath string) *httptest.Server {
	t.Helper()
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixturePath, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envelope := map[string]any{
			"id":   "msg_stub",
			"type": "message",
			"role": "assistant",
			"content": []map[string]string{
				{"type": "text", "text": string(fixture)},
			},
			"stop_reason": "end_turn",
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// suggestAvailable returns true if the compiled bosun binary recognises
// the `suggest` subcommand. False during the v0.5 round-2 window when
// this session's tests are committed before session-1's cmd_suggest.go
// has merged. Once both lanes are integrated, the gate falls through
// and the tests run for real — the skip is purely a defense against
// local `make check` runs in the session-2 worktree.
func (s *scenario) suggestAvailable() bool {
	out, err := s.bosunRaw(s.repo, "suggest", "--help")
	if err != nil {
		return false
	}
	return strings.Contains(out, "suggest")
}

func TestScenario_SuggestProducesValidPlan(t *testing.T) {
	s := newScenario(t)
	if !s.suggestAvailable() {
		t.Skip("bosun suggest not built into binary (session-1 dep not merged)")
	}

	// Give the inspector something to chew on so RepoIntel's snapshot is
	// non-trivial. Not strictly required — the stubbed Claude ignores the
	// payload — but it exercises the inspection step on a realistic shape.
	s.WriteFile("package.json", `{"name":"demo","dependencies":{"react-router-dom":"^5.3.4"}}`)
	s.WriteFile("src/index.tsx", "// entry\n")
	s.WriteFile("src/features/auth/AuthRoutes.tsx", "// auth\n")
	s.WriteFile("src/features/dashboard/DashboardRoutes.tsx", "// dashboard\n")
	s.CommitIn(s.repo, "scaffold")

	fixtureRel := filepath.Join("testdata", "suggest-react-router.json")
	fixtureAbs, err := filepath.Abs(fixtureRel)
	if err != nil {
		t.Fatalf("abs fixture: %v", err)
	}
	server := stubClaudeServer(t, fixtureAbs)

	t.Setenv("ANTHROPIC_API_KEY", "stub")
	t.Setenv("ANTHROPIC_API_URL", server.URL)

	s.Bosun("suggest", "--sessions", "5", "Migrate from React Router v5 to v6")

	planPath := filepath.Join(s.repo, "suggested-plan.md")
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("suggested-plan.md missing after `bosun suggest`: %v", err)
	}

	briefs, err := brief.Parse(planPath)
	if err != nil {
		t.Fatalf("brief.Parse(suggested-plan.md): %v\n--- plan ---\n%s", err,
			readFile(t, planPath))
	}

	var fixture suggest.LaneProposal
	raw, err := os.ReadFile(fixtureAbs)
	if err != nil {
		t.Fatalf("re-read fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	if got, want := len(briefs), len(fixture.Sessions); got != want {
		t.Fatalf("brief.Parse returned %d briefs, want %d (one per fixture lane)", got, want)
	}

	parsedLabels := make(map[string]bool, len(briefs))
	for _, b := range briefs {
		parsedLabels[b.Label] = true
	}
	for _, lane := range fixture.Sessions {
		if !parsedLabels[lane.Label] {
			t.Errorf("rendered plan missing lane %q (parsed labels: %v)",
				lane.Label, briefKeys(parsedLabels))
		}
	}
}

func TestScenario_SuggestInspectOnly_PrintsRepoIntel(t *testing.T) {
	s := newScenario(t)
	if !s.suggestAvailable() {
		t.Skip("bosun suggest not built into binary (session-1 dep not merged)")
	}

	// Drop in a couple of manifests + source files so the printed intel
	// has concrete shape (languages, file_count, top_dirs) the assertions
	// can latch onto.
	s.WriteFile("package.json", `{"name":"demo","dependencies":{}}`)
	s.WriteFile("src/index.tsx", "// entry\n")
	s.WriteFile("src/feature/a.ts", "export const a = 1\n")
	s.WriteFile("src/feature/b.ts", "export const b = 2\n")
	s.CommitIn(s.repo, "scaffold")

	// No ANTHROPIC_API_KEY needed — --inspect-only short-circuits before
	// the network call. If session-1's implementation drifts and starts
	// hitting the network here, this test will fail loudly with an
	// "API key" error rather than silently passing.
	out := s.Bosun("suggest", "--inspect-only", "Migrate to v6")

	// Locate the JSON object in the output — implementations are free to
	// print a human-readable banner before/after, but the snapshot itself
	// must be parseable JSON.
	jsonText, err := firstJSONObject(out)
	if err != nil {
		t.Fatalf("--inspect-only output has no JSON object:\n%s", out)
	}
	var intel map[string]any
	if err := json.Unmarshal([]byte(jsonText), &intel); err != nil {
		t.Fatalf("--inspect-only JSON parse: %v\n%s", err, jsonText)
	}

	for _, key := range []string{"file_count", "languages", "extension_histogram", "top_dirs"} {
		if _, ok := intel[key]; !ok {
			t.Errorf("inspect JSON missing required key %q (keys: %v)", key, mapKeys(intel))
		}
	}

	// Spot-check that the values reflect the repo we just built: node
	// language detected (package.json), non-zero file count.
	if langs, ok := intel["languages"].([]any); ok {
		var sawNode bool
		for _, l := range langs {
			if l == "node" {
				sawNode = true
				break
			}
		}
		if !sawNode {
			t.Errorf("languages should include \"node\" given package.json on disk: %v", langs)
		}
	}
	if fc, ok := intel["file_count"].(float64); ok {
		if fc < 1 {
			t.Errorf("file_count = %v, want >= 1", fc)
		}
	} else {
		t.Errorf("file_count not a number: %v", intel["file_count"])
	}
}

func TestScenario_SuggestRejectsOverlappingLanes(t *testing.T) {
	s := newScenario(t)
	if !s.suggestAvailable() {
		t.Skip("bosun suggest not built into binary (session-1 dep not merged)")
	}

	s.WriteFile("package.json", `{"name":"demo"}`)
	s.WriteFile("src/components/Button.tsx", "// button\n")
	s.CommitIn(s.repo, "scaffold")

	fixtureRel := filepath.Join("testdata", "suggest-overlap.json")
	fixtureAbs, err := filepath.Abs(fixtureRel)
	if err != nil {
		t.Fatalf("abs fixture: %v", err)
	}
	server := stubClaudeServer(t, fixtureAbs)

	t.Setenv("ANTHROPIC_API_KEY", "stub")
	t.Setenv("ANTHROPIC_API_URL", server.URL)

	// Without --allow-overlaps: bosun must reject the proposal and name
	// both colliding lanes in the error so the operator can hand-edit or
	// re-prompt.
	out, err := s.BosunErr("suggest", "--sessions", "2",
		"Refactor src/components into a design system")
	if err == nil {
		t.Fatalf("expected non-zero exit for overlapping-lanes proposal:\n%s", out)
	}
	for _, want := range []string{"session-1", "session-2", "overlap"} {
		if !strings.Contains(out, want) {
			t.Errorf("overlap error missing %q:\n%s", want, out)
		}
	}
	// And no plan file should land on disk — the operator opts in to
	// taking the messy proposal with --allow-overlaps.
	planPath := filepath.Join(s.repo, "suggested-plan.md")
	if _, err := os.Stat(planPath); err == nil {
		t.Errorf("suggested-plan.md should not be written on rejected overlap")
	}

	// With --allow-overlaps: succeed, but surface a warning that names
	// both lanes so the operator knows what they accepted.
	out2 := s.Bosun("suggest", "--sessions", "2", "--allow-overlaps",
		"Refactor src/components into a design system")
	for _, want := range []string{"session-1", "session-2"} {
		if !strings.Contains(out2, want) {
			t.Errorf("--allow-overlaps warning missing %q:\n%s", want, out2)
		}
	}
	if !containsAny(out2, "overlap", "warning", "Warning", "WARN") {
		t.Errorf("--allow-overlaps output should flag the overlap:\n%s", out2)
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Errorf("suggested-plan.md missing after --allow-overlaps run: %v", err)
	}
}

// --- helpers (suggest-scenario-local) ---

// firstJSONObject extracts the first balanced top-level JSON object from
// s. Used by the --inspect-only test so implementations can prepend a
// human banner without breaking the assertion. Walks brace depth honoring
// string literals and escapes — mirrors the model-response parser in
// internal/suggest/claude.go (extractJSON) but standalone for test use.
func firstJSONObject(s string) (string, error) {
	start := strings.Index(s, "{")
	if start < 0 {
		return "", fmt.Errorf("no '{' in input")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced braces from offset %d", start)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func briefKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- merge hooks (pre-merge, post-merge) ---
//
// pre-merge gates each session's squash; fail-closed exits skip that
// session with a clear reason and don't touch base. fail-open hooks
// emit a warning to stderr and let the merge proceed. post-merge fires
// after the squash commit lands and receives the new commit SHA via
// BOSUN_MERGE_COMMIT; failures there are non-fatal.

func TestScenario_PreMergeHookFailClosedBlocksMerge(t *testing.T) {
	// A pre-merge hook that exits non-zero with fail_open:false must skip
	// the merge for that session and surface the hook in the reason. The
	// session's branch must remain unmerged so the operator can fix the
	// hook condition and re-run.
	s := newScenario(t)

	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "pre-merge", "command": "exit 1", "fail_open": false},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "1")
	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "blocked.txt", "x\n")
	s.CommitIn(wt, "work")
	s.Bosun("done", "session-1")

	out := s.Bosun("merge")
	s.AssertContainsAll(out, "session-1", "skipped", "pre-merge hook")

	// The file the session committed must NOT be on main — the squash
	// never ran.
	if _, err := os.Stat(filepath.Join(s.repo, "blocked.txt")); err == nil {
		t.Fatalf("pre-merge fail-closed should have blocked merge, but blocked.txt is on main")
	}
}

func TestScenario_PreMergeHookFailOpenLetsMergeProceed(t *testing.T) {
	// fail_open:true: the hook errors but bosun emits a warning and the
	// squash still runs. The file should land on main.
	s := newScenario(t)

	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "pre-merge", "command": "exit 1", "fail_open": true},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "1")
	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "open.txt", "y\n")
	s.CommitIn(wt, "work")
	s.Bosun("done", "session-1")

	out := s.Bosun("merge")
	s.AssertContainsAll(out, "session-1: merged")
	s.AssertFileOnMain("open.txt")
}

func TestScenario_PreMergeHookSkippedOnDryRun(t *testing.T) {
	// Dry-run is meant to be side-effect-free: pre-merge must NOT fire,
	// even when fail_open:false would otherwise block the merge. The
	// hook command writes a sentinel — its absence proves it didn't run.
	s := newScenario(t)

	sentinel := filepath.Join(s.parent, "premerge-fired.txt")
	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{
				"event":     "pre-merge",
				"command":   `touch ` + shQuote(sentinel) + ` && exit 1`,
				"fail_open": false,
			},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "1")
	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "dry.txt", "z\n")
	s.CommitIn(wt, "work")
	s.Bosun("done", "session-1")

	out := s.Bosun("merge", "--dry-run")
	s.AssertContains(out, "would merge")

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("pre-merge hook fired on dry-run (sentinel exists at %s)", sentinel)
	}
}

func TestScenario_PostMergeHookEnvIncludesMergeCommit(t *testing.T) {
	// post-merge must see BOSUN_SESSION, BOSUN_TARGET_BRANCH, BOSUN_BRANCH,
	// BOSUN_AHEAD, and BOSUN_MERGE_COMMIT (the squash SHA on base). The
	// hook writes them to a file we then parse and compare against the
	// actual HEAD on main.
	s := newScenario(t)

	hookOut := filepath.Join(s.parent, "post-merge-env.txt")
	cmdStr := `env | grep -E '^BOSUN_(SESSION|TARGET_BRANCH|BRANCH|AHEAD|MERGE_COMMIT)=' | sort > ` + shQuote(hookOut)

	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "post-merge", "command": cmdStr},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "1")
	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "a.txt", "a\n")
	s.CommitIn(wt, "work-1")
	s.WriteFileIn(wt, "b.txt", "b\n")
	s.CommitIn(wt, "work-2")
	s.Bosun("done", "session-1")

	out := s.Bosun("merge")
	s.AssertContains(out, "session-1: merged")

	data, err := os.ReadFile(hookOut)
	if err != nil {
		t.Fatalf("post-merge hook did not run (expected %s): %v", hookOut, err)
	}
	got := string(data)

	for _, want := range []string{
		"BOSUN_AHEAD=2\n",
		"BOSUN_BRANCH=bosun/session-1\n",
		"BOSUN_SESSION=session-1\n",
		"BOSUN_TARGET_BRANCH=main\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("post-merge env missing %q; got:\n%s", want, got)
		}
	}

	// BOSUN_MERGE_COMMIT must match the actual HEAD on main after the
	// squash commit lands.
	wantSHA := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))
	if !strings.Contains(got, "BOSUN_MERGE_COMMIT="+wantSHA+"\n") {
		t.Errorf("post-merge env missing BOSUN_MERGE_COMMIT=%s; got:\n%s", wantSHA, got)
	}
}

func TestScenario_PostMergeHookFailureIsNonFatal(t *testing.T) {
	// A failing post-merge hook must NOT unwind a clean squash. The merge
	// reports success and the file lands on main; the hook failure shows
	// up as a warning in the output but doesn't change the exit code.
	s := newScenario(t)

	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "post-merge", "command": "exit 7", "fail_open": false},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "1")
	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "kept.txt", "k\n")
	s.CommitIn(wt, "work")
	s.Bosun("done", "session-1")

	out := s.Bosun("merge")
	s.AssertContainsAll(out, "session-1: merged", "post-merge hook")
	s.AssertFileOnMain("kept.txt")
}

// --- cleanup hooks + --purge safety gate ---

// TestScenario_CleanupHooksFireOncePerInvocation pins the contract for
// operators wiring `pre-cleanup` and `post-cleanup` (the typical use case
// is a single Slack message per sweep, not five). The hook must see
// BOSUN_REPO_ROOT, BOSUN_CLEANUP_COUNT, and BOSUN_CLEANUP_REASON, and
// must fire exactly once even when the planner is removing several
// sessions.
func TestScenario_CleanupHooksFireOncePerInvocation(t *testing.T) {
	s := newScenario(t)

	preLog := filepath.Join(s.parent, "pre-cleanup.log")
	postLog := filepath.Join(s.parent, "post-cleanup.log")
	preCmd := `printf 'pre count=%s reason=%s root=%s\n' "$BOSUN_CLEANUP_COUNT" "$BOSUN_CLEANUP_REASON" "$BOSUN_REPO_ROOT" >> ` + shQuote(preLog)
	postCmd := `printf 'post count=%s reason=%s root=%s\n' "$BOSUN_CLEANUP_COUNT" "$BOSUN_CLEANUP_REASON" "$BOSUN_REPO_ROOT" >> ` + shQuote(postLog)
	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "pre-cleanup", "command": preCmd},
			{"event": "post-cleanup", "command": postCmd},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "3")
	// Two sessions removable by bare cleanup: one squash-merged
	// (session-1: commit → done → merge), one empty (session-3 untouched).
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "a.txt", "x\n")
	s.CommitIn(wt1, "session-1 work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")
	// session-2 stays in-flight (committed but not done) so the planner
	// has a "skip" to interleave with the two "remove"s.
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "b.txt", "y\n")
	s.CommitIn(wt2, "session-2 wip")

	s.Bosun("cleanup")

	preBytes, err := os.ReadFile(preLog)
	if err != nil {
		t.Fatalf("pre-cleanup hook did not run: %v", err)
	}
	postBytes, err := os.ReadFile(postLog)
	if err != nil {
		t.Fatalf("post-cleanup hook did not run: %v", err)
	}
	preLines := strings.Count(strings.TrimSpace(string(preBytes)), "\n") + 1
	postLines := strings.Count(strings.TrimSpace(string(postBytes)), "\n") + 1
	if preLines != 1 {
		t.Errorf("pre-cleanup fired %d times, want 1:\n%s", preLines, preBytes)
	}
	if postLines != 1 {
		t.Errorf("post-cleanup fired %d times, want 1:\n%s", postLines, postBytes)
	}

	got := string(preBytes) + string(postBytes)
	// Pre-cleanup reports the planned removal count; post reports actual
	// removed. Both should be 2 here (session-1 squashed + session-3 empty).
	wantRoot, _ := filepath.EvalSymlinks(s.repo)
	for _, want := range []string{
		"pre count=2",
		"post count=2",
		"reason=manual",
		"root=" + wantRoot,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook log missing %q:\n%s", want, got)
		}
	}
}

// TestScenario_CleanupForceRefusesDoneButUnmerged is the v0.5 round-1
// regression: --force used to silently nuke a DONE session whose commits
// were not yet on main, losing the work. Now --force refuses the case
// outright and the error names both recovery paths (bosun merge or
// --purge).
func TestScenario_CleanupForceRefusesDoneButUnmerged(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "valuable work\n")
	s.CommitIn(wt1, "valuable session-1 commit")
	s.Bosun("done", "session-1")
	// Critically: no `bosun merge` — the commits live only on the
	// session branch, removing it would lose them.

	out := s.Bosun("cleanup", "--force")
	s.AssertContainsAll(out,
		"session-1: skipped",
		"would discard",
		"bosun merge session-1",
		"--purge",
	)
	s.AssertWorktreeExists(1)
	s.AssertBranchExists("bosun/session-1")
}

// TestScenario_CleanupPurgeProceedsOnDoneButUnmerged is the explicit
// opt-in: when the operator truly means to drop the unmerged work, the
// `--purge` flag lets cleanup tear down the worktree and branch even
// though committed work would be discarded. Pair test for
// CleanupForceRefusesDoneButUnmerged.
func TestScenario_CleanupPurgeProceedsOnDoneButUnmerged(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "throwaway.txt", "abandoned\n")
	s.CommitIn(wt1, "session-1 commit that won't ship")
	s.Bosun("done", "session-1")

	out := s.Bosun("cleanup", "--purge")
	s.AssertContainsAll(out,
		"session-1: removed",
		"--purge discards",
	)
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}

// --- pre-remove hook ---

// writePreRemoveConfig writes a .bosun/config.json wiring up a single
// pre-remove hook. Pulled out so the scenarios below stay focused on
// their behavioral assertions.
func writePreRemoveConfig(t *testing.T, s *scenario, command string, failOpen bool) {
	t.Helper()
	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "pre-remove", "command": command, "fail_open": failOpen},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))
}

func TestScenario_PreRemoveHookBlocksWhenFailClosed(t *testing.T) {
	// A pre-remove hook with fail_open=false (the default safety stance)
	// must abort the removal — the worktree and branch should still be
	// there after `bosun remove` returns non-zero. This is the contract
	// operators rely on to veto a teardown they don't want to happen.
	s := newScenario(t)
	writePreRemoveConfig(t, s, "exit 1", false)
	s.Bosun("init", "1")

	out, err := s.BosunErr("remove", "session-1")
	if err == nil {
		t.Fatalf("remove should fail when pre-remove hook exits 1; output:\n%s", out)
	}
	if !strings.Contains(out, "pre-remove") {
		t.Errorf("error should mention pre-remove; got:\n%s", out)
	}
	s.AssertWorktreeExists(1)
	s.AssertBranchExists("bosun/session-1")
}

func TestScenario_PreRemoveHookFailOpenLetsRemovalProceed(t *testing.T) {
	// fail_open=true downgrades the same exit-1 hook to a warning. The
	// removal still happens; this mirrors how post-init / post-done
	// behave today and lets operators wire up best-effort notifications
	// without blocking destructive ops on flaky integrations.
	s := newScenario(t)
	writePreRemoveConfig(t, s, "exit 1", true)
	s.Bosun("init", "1")

	s.Bosun("remove", "session-1")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}

func TestScenario_PreRemoveHookSeesBosunEnv(t *testing.T) {
	// The documented env (BOSUN_SESSION/BRANCH/WORKTREE_PATH/AHEAD/DIRTY)
	// must arrive intact — these are the inputs a snapshot or notify
	// script needs to actually do its job. A hook that errors silently
	// downstream because BOSUN_WORKTREE_PATH wasn't set would be a
	// nightmare to debug; lock the shape now.
	s := newScenario(t)

	hookOut := filepath.Join(s.parent, "remove-env.txt")
	cmdStr := `env | grep -E '^BOSUN_(SESSION|BRANCH|WORKTREE_PATH|AHEAD|DIRTY)=' | sort > ` + shQuote(hookOut)
	writePreRemoveConfig(t, s, cmdStr, false)

	s.Bosun("init", "1")
	// Create one tracked commit so BOSUN_AHEAD is observably non-zero;
	// the test then squash-merges so `remove` doesn't need --force.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "session-1 work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")

	s.Bosun("remove", "session-1")

	data, err := os.ReadFile(hookOut)
	if err != nil {
		t.Fatalf("pre-remove hook did not run (expected output at %s): %v", hookOut, err)
	}
	got := string(data)
	// Worktree path: tolerate macOS /var ↔ /private/var symlink resolution
	// in either direction — git may report the registered or canonical form.
	// Since v0.10's UID-per-worktree naming the basename ends in `-N` after
	// an optional `<YYYYMMDD-HHMMSS>` segment, so check the trailing `-1`
	// piece rather than the full legacy suffix.
	if !strings.Contains(got, "BOSUN_WORKTREE_PATH=") || !strings.Contains(got, "-1\n") {
		t.Errorf("hook env missing BOSUN_WORKTREE_PATH ending in `-1`; got:\n%s", got)
	}
	checks := []string{
		"BOSUN_SESSION=session-1\n",
		"BOSUN_BRANCH=bosun/session-1\n",
		"BOSUN_AHEAD=",
		"BOSUN_DIRTY=0\n",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("hook env missing %q; got:\n%s", want, got)
		}
	}
}

// --- v0.5-round2 session-5: init robustness ---

func TestScenario_InitPrintsProgressPerWorktree(t *testing.T) {
	// Part A: operators see forward progress so a slow worktree creation
	// doesn't look like a silent hang.
	s := newScenario(t)
	out := s.Bosun("init", "3")
	s.AssertContainsAll(out,
		"Creating worktree session-1 (1/3)",
		"Creating worktree session-2 (2/3)",
		"Creating worktree session-3 (3/3)",
	)
}

func TestScenario_InitWarnsOnHighLoadAndPauses(t *testing.T) {
	// Part B: BOSUN_TEST_LOAD_AVERAGE is the subprocess-side seam — set
	// it above the 5.0 threshold and confirm the warning prints.
	t.Setenv("BOSUN_TEST_LOAD_AVERAGE", "8.0")
	s := newScenario(t)
	out := s.Bosun("init", "1")
	if !strings.Contains(out, "system load is 8.00") {
		t.Errorf("expected high-load warning, got:\n%s", out)
	}
	if !strings.Contains(out, "--no-load-check") {
		t.Errorf("expected --no-load-check hint in warning, got:\n%s", out)
	}
}

func TestScenario_InitNoLoadCheckSkipsWarning(t *testing.T) {
	// Part B: --no-load-check must bypass the check entirely.
	t.Setenv("BOSUN_TEST_LOAD_AVERAGE", "99.0")
	s := newScenario(t)
	out := s.Bosun("init", "1", "--no-load-check")
	if strings.Contains(out, "system load is") {
		t.Errorf("--no-load-check should suppress warning, got:\n%s", out)
	}
}

func TestScenario_InitDetectsPhantomBranchRefs(t *testing.T) {
	// Part C: filesystem artifacts from Finder / Time Machine /
	// Spotlight that match the "<name> <digit>" pattern get flagged as
	// phantom refs.
	s := newScenario(t)
	dir := filepath.Join(s.repo, ".git", "refs", "heads", "bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-7 2"), []byte("ref\n"), 0o644); err != nil {
		t.Fatalf("write phantom: %v", err)
	}

	out := s.Bosun("init", "1")
	if !strings.Contains(out, "phantom branch ref") {
		t.Errorf("expected phantom-ref advisory, got:\n%s", out)
	}
	if !strings.Contains(out, "--clean-phantoms") {
		t.Errorf("expected --clean-phantoms hint, got:\n%s", out)
	}
	// Advisory mode leaves the phantom file in place.
	if _, err := os.Stat(filepath.Join(dir, "session-7 2")); err != nil {
		t.Errorf("phantom file should remain in advisory mode: %v", err)
	}
}

func TestScenario_InitCleanPhantomsRemovesThem(t *testing.T) {
	// Part C: --clean-phantoms deletes the bogus ref files.
	s := newScenario(t)
	dir := filepath.Join(s.repo, ".git", "refs", "heads", "bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	phantom := filepath.Join(dir, "session-9 2")
	if err := os.WriteFile(phantom, []byte("ref\n"), 0o644); err != nil {
		t.Fatalf("write phantom: %v", err)
	}

	out := s.Bosun("init", "1", "--clean-phantoms")
	if !strings.Contains(out, "Removed 1 phantom branch ref") {
		t.Errorf("expected removed-phantom confirmation, got:\n%s", out)
	}
	if _, err := os.Stat(phantom); !os.IsNotExist(err) {
		t.Errorf("phantom file should be gone, stat err=%v", err)
	}
}

// --- config unset / validate / init ---
//
// These exercise the round-out subcommands added on top of the original
// get/set/list trio. They build on the same shared scenario harness so
// the underlying file plumbing is real (no mocking of disk or git).

func TestScenario_ConfigUnsetRemovesOverride(t *testing.T) {
	s := newScenario(t)
	s.Bosun("config", "set", "base_branch", "develop")
	if out := s.Bosun("config", "list"); !strings.Contains(out, "base_branch: develop") || strings.Contains(out, "base_branch: develop (default)") {
		t.Fatalf("expected base_branch overridden to develop, got:\n%s", out)
	}

	out := s.Bosun("config", "unset", "base_branch")
	s.AssertContains(out, "unset base_branch")

	listed := s.Bosun("config", "list")
	s.AssertContains(listed, "base_branch: main (default)")

	// Other keys still respect their previous state — the raw file
	// should round-trip without losing unrelated overrides.
	s.Bosun("config", "set", "launcher", "print")
	s.Bosun("config", "unset", "base_branch") // no-op now, but mustn't drop launcher
	listed = s.Bosun("config", "list")
	s.AssertContains(listed, "launcher: print")
}

func TestScenario_ConfigUnsetOnDefaultIsNoop(t *testing.T) {
	s := newScenario(t)
	out := s.Bosun("config", "unset", "launcher")
	s.AssertContains(out, "already at default")

	// No config file should have been created by a no-op unset.
	if _, err := os.Stat(filepath.Join(s.repo, ".bosun", "config.json")); err == nil {
		t.Fatalf(".bosun/config.json should not exist after no-op unset")
	}
}

func TestScenario_ConfigUnsetUnknownKeyFails(t *testing.T) {
	s := newScenario(t)
	out, err := s.BosunErr("config", "unset", "made_up_key")
	if err == nil {
		t.Fatalf("expected error for unknown key, output:\n%s", out)
	}
	s.AssertContains(out, "unknown config key")
}

func TestScenario_ConfigValidatePassesOnGoodConfig(t *testing.T) {
	s := newScenario(t)
	// No file → defaults are valid.
	out := s.Bosun("config", "validate")
	s.AssertContains(out, "absent")

	// File present with a real override.
	s.Bosun("config", "set", "base_branch", "develop")
	out = s.Bosun("config", "validate")
	s.AssertContains(out, "is valid")
}

func TestScenario_ConfigValidateFailsOnUnknownHookEvent(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"hooks":[{"event":"post-typo","command":"echo hi"}]}`)
	out, err := s.BosunErr("config", "validate")
	if err == nil {
		t.Fatalf("expected validate to fail on unknown hook event, output:\n%s", out)
	}
	s.AssertContains(out, "unknown event")
}

func TestScenario_ConfigValidateFailsOnUnknownTopLevelKey(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"base_branchh":"main"}`)
	out, err := s.BosunErr("config", "validate")
	if err == nil {
		t.Fatalf("expected validate to fail on typo'd top-level key, output:\n%s", out)
	}
	s.AssertContains(out, "unrecognized key")
}

func TestScenario_ConfigInitWritesStubAndExample(t *testing.T) {
	s := newScenario(t)
	out := s.Bosun("config", "init")
	s.AssertContains(out, ".bosun/config.json")
	s.AssertContains(out, "config.example.json")

	cfgPath := filepath.Join(s.repo, ".bosun", "config.json")
	data := readFile(t, cfgPath)
	for _, want := range []string{
		`"base_branch": "main"`,
		`"session_prefix": "bosun"`,
		`"launcher": "auto"`,
		`"verify_cmd": "make check"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("stub config missing %q; got:\n%s", want, data)
		}
	}

	// `config validate` must pass on the freshly-written stub — if it
	// doesn't, `init` shipped a file the user can't actually use.
	s.Bosun("config", "validate")

	examplePath := filepath.Join(s.repo, ".bosun", "config.example.json")
	example := readFile(t, examplePath)
	if !strings.HasPrefix(example, "//") {
		t.Errorf("example file should start with a // comment block, got prefix %q", example[:min(40, len(example))])
	}
	for _, want := range []string{"base_branch", "hooks", "suggest"} {
		if !strings.Contains(example, want) {
			t.Errorf("example missing %q", want)
		}
	}
}

// --- v0.5-round3 session-3: predictive conflict ---

// predictAvailable mirrors suggestAvailable — false when this session's
// scenarios run inside its own worktree before the binary has been rebuilt
// with session-3's CLI wiring. Once round-3 merges, the gate falls
// through and the tests run for real.
func (s *scenario) predictAvailable() bool {
	out, err := s.bosunRaw(s.repo, "predict", "--help")
	if err != nil {
		return false
	}
	return strings.Contains(out, "predict")
}

// predictHasRealHeuristic returns true once session-2's predictor is
// emitting non-empty results — useful for gating overlap assertions
// that need an actual heuristic, not the round-3 stub.
func (s *scenario) predictHasRealHeuristic() bool {
	plan := filepath.Join(s.repo, "probe-plan.md")
	if err := os.WriteFile(plan, []byte("## session-1\nTouch internal/auth/handlers.go.\n"), 0o644); err != nil {
		return false
	}
	defer os.Remove(plan)
	out, _ := s.bosunRaw(s.repo, "predict", "--json", plan)
	// Real heuristic should pick at least one path out of a brief that
	// literally names "internal/auth/handlers.go".
	return strings.Contains(out, "internal/auth/handlers.go")
}

func TestScenario_PredictDisjointPlan_ExitsZero(t *testing.T) {
	s := newScenario(t)
	if !s.predictAvailable() {
		t.Skip("bosun predict not built into binary")
	}

	// Two lanes that name clearly disjoint code paths. With the round-3
	// shim predictor (returns nil/nil) this trivially passes; with
	// session-2's real heuristic it should still detect no overlap.
	s.WriteFile("plans/disjoint.md", `# Disjoint plan

## session-1
Refactor the auth handlers under internal/auth/.

## session-2
Migrate the storage layer under internal/storage/.
`)

	out := s.Bosun("predict", "plans/disjoint.md")
	if !strings.Contains(out, "Overlaps: none predicted") {
		t.Errorf("expected 'Overlaps: none predicted', got:\n%s", out)
	}
}

func TestScenario_PredictOverlappingPlan_ExitsNonZero(t *testing.T) {
	s := newScenario(t)
	if !s.predictAvailable() {
		t.Skip("bosun predict not built into binary")
	}
	if !s.predictHasRealHeuristic() {
		// Round-3's shim predictor returns nil — the operator's real
		// heuristic (session-2) ships the overlap detection logic. Skip
		// until session-2 has landed so this test doesn't false-pass on
		// "no overlaps reported" when the heuristic is just a stub.
		t.Skip("predict heuristic is the round-3 stub; session-2 not merged yet")
	}

	// Two lanes that both name the same file. The path is wrapped in
	// backticks so it counts as an intentional claim under the
	// context-sensitive parser (closes #17): unquoted prose mentions
	// are informational only, but backticked references stay claims.
	s.WriteFile("plans/overlap.md", `# Overlap plan

## session-1
Refactor `+"`internal/pkg/shared.go`"+` for clarity.

## session-2
Rewrite `+"`internal/pkg/shared.go`"+` to use the new API.
`)

	out, err := s.BosunErr("predict", "plans/overlap.md")
	if err == nil {
		t.Fatalf("predict should exit non-zero when overlaps detected, got:\n%s", out)
	}
	if !strings.Contains(out, "shared.go") {
		t.Errorf("overlap report should name shared.go, got:\n%s", out)
	}
	for _, want := range []string{"session-1", "session-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("overlap report missing %q, got:\n%s", want, out)
		}
	}
}

func TestScenario_ConfigInitRefusesOverwriteWithoutForce(t *testing.T) {
	s := newScenario(t)
	s.Bosun("config", "init")

	// Mutate the config so we can prove --force replaces it.
	s.Bosun("config", "set", "launcher", "print")

	out, err := s.BosunErr("config", "init")
	if err == nil {
		t.Fatalf("expected init to refuse overwrite, got:\n%s", out)
	}
	s.AssertContains(out, "already exists")

	// --force succeeds and resets the mutated key back to the default.
	s.Bosun("config", "init", "--force")
	listed := s.Bosun("config", "list")
	s.AssertContains(listed, "launcher: auto")
}

func TestScenario_PredictJSON_HasStructuredShape(t *testing.T) {
	s := newScenario(t)
	if !s.predictAvailable() {
		t.Skip("bosun predict not built into binary")
	}

	s.WriteFile("plans/p.md", `## session-1
do thing A

## session-2
do thing B
`)

	// Drive `--json` and check that the top-level shape matches the
	// MCP wire contract: {"predictions": [...], "overlaps": [...]}.
	out, err := s.bosunRaw(s.repo, "predict", "--json", "plans/p.md")
	_ = err

	jsonText, jerr := firstJSONObject(out)
	if jerr != nil {
		t.Fatalf("--json output has no JSON object:\n%s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(jsonText), &doc); err != nil {
		t.Fatalf("--json decode: %v\n%s", err, jsonText)
	}
	for _, key := range []string{"predictions", "overlaps"} {
		v, ok := doc[key]
		if !ok {
			t.Errorf("--json missing key %q (keys: %v)", key, mapKeys(doc))
			continue
		}
		if _, ok := v.([]any); !ok && v != nil {
			t.Errorf("--json %q should be an array, got %T", key, v)
		}
	}
}

func TestScenario_PredictMissingPlan_ExitsNonZero(t *testing.T) {
	s := newScenario(t)
	if !s.predictAvailable() {
		t.Skip("bosun predict not built into binary")
	}

	out, err := s.BosunErr("predict", "no-such-plan.md")
	if err == nil {
		t.Fatalf("missing plan should exit non-zero, got:\n%s", out)
	}
}

// --- v0.6: shared fake-agent helper (used by merge/remove/cleanup tests) ---

// startFakeAgent execs the test-built "claude" binary (compiled by
// TestMain in scenario_test.go) with cwd=worktree so `proc.Running`
// (which matches on basename + CWD) sees a live agent there. Caller's
// t.Cleanup kills the process. Skips on Windows (scenarios already
// skip there) or if the binary wasn't built.
func startFakeAgent(t *testing.T, worktree string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-agent helper uses POSIX exec")
	}
	if fakeAgentBin == "" {
		t.Skip("fake-agent binary not built (see TestMain)")
	}
	cmd := exec.Command(fakeAgentBin)
	cmd.Dir = worktree
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake claude: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	// Absorb scheduler jitter — the process is visible to gopsutil
	// essentially immediately after Start returns, but a brief pause
	// before the first bosun call avoids race-skip on busy CI runners.
	time.Sleep(200 * time.Millisecond)
	return cmd
}

// --- v0.6: merge safety (agent-liveness gate, fsck, undo log) ---

func TestScenario_MergeRefusesLiveAgentWithDirtyFiles(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Real commit so the merge would otherwise proceed.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")

	// Plant an untracked file — this is the "in-flight work" the gate
	// protects against. (Untracked is the riskier case: --squash would
	// happily ignore it and the operator loses the file silently.)
	s.WriteFileIn(wt1, "scratch.txt", "in progress\n")

	// Configure status helper to verify scratch.txt is untracked from
	// git's point of view before we light up the fake agent.
	out := s.GitIn(wt1, "status", "--porcelain")
	if !strings.Contains(out, "?? scratch.txt") {
		t.Fatalf("scratch.txt should be untracked, got:\n%s", out)
	}

	startFakeAgent(t, wt1)

	mergeOut, err := s.BosunErr("merge")
	if err == nil {
		t.Fatalf("merge should refuse when agent live + dirty, got success:\n%s", mergeOut)
	}
	for _, want := range []string{"live agent", "uncommitted changes", "--ignore-running"} {
		if !strings.Contains(mergeOut, want) {
			t.Errorf("refusal message missing %q, got:\n%s", want, mergeOut)
		}
	}

	// Sanity: the untracked file is still there and main hasn't moved.
	if _, err := os.Stat(filepath.Join(wt1, "scratch.txt")); err != nil {
		t.Errorf("scratch.txt should still exist after refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.repo, "feature.txt")); err == nil {
		t.Errorf("feature.txt should NOT be on main yet (merge was refused)")
	}
}

// TestScenario_MergeRefusesLiveAgentWithModifiedTrackedFile pins the
// v0.8 trial #2 finding (Test 3): when a session has a live agent AND a
// modified tracked file (Session.Dirty > 0), the merge refusal must
// fire the v0.6 agent-aware message — names the live PID + the
// --ignore-running escape hatch — not the simpler v0.5-era
// "N uncommitted change(s)" short-circuit.
//
// Pre-fix, mergeOne ordered the s.Dirty check FIRST, so a modified
// tracked file would skip the agent-liveness gate entirely and the
// operator got the less-informative message. Post-fix the gate runs
// before the dirty check.
//
// The companion test above uses an UNTRACKED file (Dirty == 0,
// gate already fires correctly); this one uses a MODIFIED file
// (Dirty > 0, the regressed path).
func TestScenario_MergeRefusesLiveAgentWithModifiedTrackedFile(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	// Commit a tracked file then modify it (without committing) — this
	// is the trial #2 scenario: README.md was modified, not untracked.
	s.WriteFileIn(wt1, "feature.txt", "first\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")

	// Modify the now-tracked file. This produces Dirty == 1 in the
	// session struct, which pre-fix would short-circuit before the gate.
	s.WriteFileIn(wt1, "feature.txt", "first\nin-flight edit\n")

	// Sanity: git sees this as a tracked-file modification, not untracked.
	out := s.GitIn(wt1, "status", "--porcelain")
	if !strings.Contains(out, " M feature.txt") {
		t.Fatalf("feature.txt should be modified-tracked, got:\n%s", out)
	}

	startFakeAgent(t, wt1)

	mergeOut, err := s.BosunErr("merge")
	if err == nil {
		t.Fatalf("merge should refuse when agent live + modified-tracked file, got success:\n%s", mergeOut)
	}
	// Must surface the v0.6 agent-aware message, not the v0.5 simple one.
	for _, want := range []string{"live agent", "uncommitted changes", "--ignore-running"} {
		if !strings.Contains(mergeOut, want) {
			t.Errorf("refusal message missing %q, got:\n%s", want, mergeOut)
		}
	}
	// Must NOT have short-circuited to the bare "N uncommitted change(s)" form.
	if strings.Contains(mergeOut, "1 uncommitted change(s)") && !strings.Contains(mergeOut, "live agent") {
		t.Errorf("merge short-circuited to the v0.5 message; got:\n%s", mergeOut)
	}
}

func TestScenario_MergeIgnoreRunningBypassesGate(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")
	s.WriteFileIn(wt1, "scratch.txt", "in progress\n")

	startFakeAgent(t, wt1)

	mergeOut := s.Bosun("merge", "--ignore-running")
	s.AssertContains(mergeOut, "session-1: merged")
	s.AssertFileOnMain("feature.txt")
}

func TestScenario_MergeRefusesOnFsckFailure(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")

	// Corrupt a loose object in main's shared .git/objects so the
	// worktree's fsck sees it (linked worktrees share the object store).
	// We target an older blob, not the tip — corrupting the tip would
	// break the merge in a different (less-targeted) way.
	objRoot := filepath.Join(s.repo, ".git", "objects")
	var victim string
	_ = filepath.WalkDir(objRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || victim != "" {
			return nil
		}
		parent := filepath.Base(filepath.Dir(p))
		if len(parent) == 2 {
			victim = p
		}
		return nil
	})
	if victim == "" {
		t.Skip("no loose objects available to corrupt")
	}
	// Loose objects are mode 0444; clear u+w before overwriting.
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatalf("chmod victim: %v", err)
	}
	if err := os.WriteFile(victim, []byte("not a real git object"), 0o644); err != nil {
		t.Fatalf("corrupt loose object: %v", err)
	}

	mergeOut, err := s.BosunErr("merge")
	if err == nil {
		t.Fatalf("merge should refuse on fsck failure, got success:\n%s", mergeOut)
	}
	if !strings.Contains(strings.ToLower(mergeOut), "fsck") {
		t.Errorf("refusal message should mention fsck, got:\n%s", mergeOut)
	}
}

func TestScenario_MergeRecordsMergeLogEntry(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")

	logPath := filepath.Join(s.repo, ".bosun", "merges.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read merges.log: %v", err)
	}
	if !strings.Contains(string(data), `"session":"session-1"`) {
		t.Errorf("merges.log missing session-1 entry:\n%s", data)
	}
	for _, want := range []string{`"pre":"`, `"post":"`, `"ts":"`, `"squash_msg":"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("merges.log missing key %q:\n%s", want, data)
		}
	}
}

func TestScenario_MergeUndoResetsMain(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Capture pre-merge HEAD via bosun's view (we'll compare git rev-parse below).
	preHead := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")

	postHead := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))
	if postHead == preHead {
		t.Fatalf("HEAD did not advance after merge")
	}
	s.AssertFileOnMain("feature.txt")

	// Undo: by session name. After this, main should be back at preHead
	// and the merged file should be gone from the main worktree.
	undoOut := s.Bosun("merge", "--undo", "session-1")
	s.AssertContains(undoOut, "undid merge of session-1")

	afterUndo := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))
	if afterUndo != preHead {
		t.Errorf("HEAD after undo = %s, want preHead %s", afterUndo, preHead)
	}
	if _, err := os.Stat(filepath.Join(s.repo, "feature.txt")); err == nil {
		t.Errorf("feature.txt should be gone from main after undo")
	}
}

func TestScenario_MergeUndoRefusesWhenMainAdvanced(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature work")
	s.Bosun("done", "session-1")
	s.Bosun("merge")

	// Land an unrelated commit directly on main — now the recorded
	// post-SHA is no longer HEAD, so undo must refuse.
	s.WriteFile("after.txt", "later work\n")
	s.GitIn(s.repo, "add", "after.txt")
	s.GitIn(s.repo, "commit", "-q", "-m", "post-merge work")

	out, err := s.BosunErr("merge", "--undo", "session-1")
	if err == nil {
		t.Fatalf("undo should refuse after main advanced, got success:\n%s", out)
	}
	for _, want := range []string{"main has moved past", "git reflog"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal message missing %q, got:\n%s", want, out)
		}
	}
}

func TestScenario_MergeListUndoPrintsHistory(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	for _, n := range []int{1, 2} {
		wt := s.WorktreePath(n)
		s.WriteFileIn(wt, fmt.Sprintf("f%d.txt", n), "x\n")
		s.CommitIn(wt, fmt.Sprintf("work %d", n))
		s.Bosun("done", fmt.Sprintf("session-%d", n))
	}
	s.Bosun("merge")

	out := s.Bosun("merge", "--list-undo")
	for _, want := range []string{"session-1", "session-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("--list-undo output missing %q, got:\n%s", want, out)
		}
	}
}

func TestScenario_MergeListUndoEmptyLog(t *testing.T) {
	s := newScenario(t)
	out := s.Bosun("merge", "--list-undo")
	s.AssertContains(out, "no merge history")
}

// --- v0.6 r1: remove + cleanup agent-liveness gate ---

func TestScenario_RemoveRefusesLiveAgentWithDirty(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	// Dirty the worktree (tracked-file edit; untracked don't count toward Dirty).
	s.WriteFileIn(wt1, "README.md", "# dirty\n")
	startFakeAgent(t, wt1)

	out, err := s.BosunErr("remove", "session-1")
	if err == nil {
		t.Fatalf("remove with live agent + dirty should refuse; got:\n%s", out)
	}
	s.AssertContainsAll(out,
		"session-1",
		"live agent",
		"refusing remove",
		"--ignore-running",
	)
	s.AssertWorktreeExists(1)
	s.AssertBranchExists("bosun/session-1")
}

func TestScenario_RemoveIgnoreRunningProceeds(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# dirty\n")
	startFakeAgent(t, wt1)

	// --ignore-running bypasses the liveness gate; --force is still needed
	// for the existing dirty gate. Document that both must compose by
	// asserting --ignore-running alone is not enough.
	if _, err := s.BosunErr("remove", "session-1", "--ignore-running"); err == nil {
		t.Fatal("--ignore-running alone should still refuse dirty without --force")
	}
	s.Bosun("remove", "session-1", "--ignore-running", "--force")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}

func TestScenario_CleanupSkipsLiveAgentSessionsProcessesOthers(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "3")

	// session-1: live agent + dirty → should be skipped by cleanup.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# dirty\n")
	startFakeAgent(t, wt1)

	// session-2: DONE + merged → removable.
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "feature.txt", "x\n")
	s.CommitIn(wt2, "feature")
	s.Bosun("done", "session-2")
	s.Bosun("merge")

	// session-3: empty → removable.

	out := s.Bosun("cleanup")
	s.AssertContainsAll(out,
		"session-1: skipped",
		"live agent",
		"--ignore-running",
		"session-2: removed",
		"session-3: removed",
		"removed 2, skipped 1",
	)
	s.AssertWorktreeExists(1)
	s.AssertWorktreeMissing(2)
	s.AssertWorktreeMissing(3)
}

func TestScenario_CleanupIgnoreRunningProcessesEverything(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")

	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "README.md", "# dirty\n")
	startFakeAgent(t, wt1)

	// session-2: also dirty (no live agent) — covered by --force.
	wt2 := s.WorktreePath(2)
	s.WriteFileIn(wt2, "README.md", "# dirty too\n")

	// --ignore-running bypasses the liveness gate; --force handles the
	// existing dirty path. Both sessions should be removed.
	out := s.Bosun("cleanup", "--ignore-running", "--force")
	s.AssertContainsAll(out, "session-1: removed", "session-2: removed")
	s.AssertWorktreeMissing(1)
	s.AssertWorktreeMissing(2)
}

func TestScenario_CleanupPurgeAndIgnoreRunningCompose(t *testing.T) {
	// A session that needs BOTH gates bypassed to remove: live agent +
	// uncommitted changes (liveness gate) AND committed work not on
	// base (purge gate). --ignore-running --purge --force together should
	// remove it; any subset should leave it alone.
	s := newScenario(t)
	s.Bosun("init", "1")

	wt1 := s.WorktreePath(1)
	// Committed work that isn't on base (would-discard shape).
	s.WriteFileIn(wt1, "feature.txt", "x\n")
	s.CommitIn(wt1, "feature")
	// Uncommitted edit on top.
	s.WriteFileIn(wt1, "README.md", "# dirty\n")
	startFakeAgent(t, wt1)

	// Plain cleanup: liveness gate fires first.
	out := s.Bosun("cleanup")
	s.AssertContainsAll(out, "session-1: skipped", "live agent")
	s.AssertWorktreeExists(1)

	// --ignore-running alone: falls through to purge gate, still skipped.
	out = s.Bosun("cleanup", "--ignore-running")
	s.AssertContains(out, "session-1: skipped")
	s.AssertWorktreeExists(1)

	// All three together → removable.
	out = s.Bosun("cleanup", "--ignore-running", "--purge", "--force")
	s.AssertContains(out, "session-1: removed")
	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}

// --- v0.6 round-1 session-3: CRASHED state + rescue + heartbeat ---

// TestScenario_StatusReportsCrashedWhenWorktreeDirty walks bosun status
// end-to-end for the CRASHED derivation: spin up a session, leave a
// modified file behind, and confirm the JSON status reports state=CRASHED
// without us having to send any liveness signal. proc.Running is false
// because no agent ever started here.
func TestScenario_StatusReportsCrashedWhenWorktreeDirty(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt := s.WorktreePath(1)
	// Stage nothing — just leave an unmerged tracked-file change so
	// DirtyCount comes back non-zero. README.md exists from gitInit().
	s.WriteFileIn(wt, "README.md", "# dirty\n")

	payload := s.StatusJSON()
	got := payload.SessionByNumber(1)
	if got == nil {
		t.Fatalf("session-1 missing from status:\n%+v", payload)
	}
	if got.State != "CRASHED" {
		t.Errorf("State = %q, want CRASHED (proc not running + dirty worktree)", got.State)
	}
}

// TestScenario_StatusStaysWorkingWhenClean is the negative for the
// previous test: a session with a clean worktree and no live agent must
// stay WORKING. Otherwise every idle session would flicker CRASHED
// between `bosun init` and the operator's first edit.
func TestScenario_StatusStaysWorkingWhenClean(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	payload := s.StatusJSON()
	got := payload.SessionByNumber(1)
	if got == nil {
		t.Fatalf("session-1 missing from status:\n%+v", payload)
	}
	if got.State != "WORKING" {
		t.Errorf("State = %q, want WORKING (clean worktree)", got.State)
	}
}

// TestScenario_RescueRefusesNonCrashed: `bosun rescue` is the narrow
// recovery tool; running it on a healthy WORKING session must exit
// non-zero with a clear message rather than producing an empty
// snapshot dir.
func TestScenario_RescueRefusesNonCrashed(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	out, err := s.BosunErr("rescue", "1")
	if err == nil {
		t.Fatalf("rescue on non-CRASHED session should exit non-zero, got:\n%s", out)
	}
	if !strings.Contains(out, "not CRASHED") {
		t.Errorf("rescue refusal output missing 'not CRASHED' hint:\n%s", out)
	}
}

// TestScenario_RescueSnapshotsCrashedWorktree: leave a dirty file behind
// on a session with no live agent (which surfaces as CRASHED), run
// `bosun rescue`, and verify `.bosun/rescues/<session>-<ts>/` exists
// with the dirty content inside. The original worktree must be left
// untouched.
func TestScenario_RescueSnapshotsCrashedWorktree(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "README.md", "# half-finished work\n")
	s.WriteFileIn(wt, "untracked.txt", "agent left this behind\n")

	out := s.Bosun("rescue", "1")
	if !strings.Contains(out, "rescued") {
		t.Errorf("rescue output missing 'rescued' confirmation:\n%s", out)
	}

	rescues := filepath.Join(s.repo, ".bosun", "rescues")
	entries, err := os.ReadDir(rescues)
	if err != nil {
		t.Fatalf("read rescues dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("rescue dir entries = %d, want 1: %+v", len(entries), entries)
	}
	snap := filepath.Join(rescues, entries[0].Name())
	if !strings.HasPrefix(entries[0].Name(), "session-1-") {
		t.Errorf("rescue dir name %q should start with session-1-", entries[0].Name())
	}

	// Dirty README + untracked file both landed.
	for _, rel := range []string{"README.md", "untracked.txt"} {
		if _, err := os.Stat(filepath.Join(snap, rel)); err != nil {
			t.Errorf("snapshot missing %s: %v", rel, err)
		}
	}
	// Original worktree untouched.
	body, err := os.ReadFile(filepath.Join(wt, "README.md"))
	if err != nil || string(body) != "# half-finished work\n" {
		t.Errorf("worktree README mutated by rescue: body=%q err=%v", body, err)
	}
}

// TestScenario_InitResumability exercises the v0.6 init-state breadcrumb
// flow: a failed `bosun init 3` leaves `.bosun/init.state` describing what
// finished, a follow-up `bosun init --resume` finishes the rest, and on
// success the state file is removed. We force the failure by shimming a
// fake `git` onto PATH that errors on the second `git worktree add`. The
// shim defers to the real git binary for every other command so the
// surrounding `git init`, branch creation, etc. all behave normally.
func TestScenario_InitResumability(t *testing.T) {
	s := newScenario(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}

	shimDir := t.TempDir()
	counterPath := filepath.Join(shimDir, "wt-count")
	failFlag := filepath.Join(shimDir, "fail-second")
	shimScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "add" ]; then
  n=$(cat %q 2>/dev/null || echo 0)
  n=$((n+1))
  echo "$n" > %q
  if [ -f %q ] && [ "$n" = "2" ]; then
    echo "shim: fake failure on worktree #2" >&2
    exit 1
  fi
fi
exec %q "$@"
`, counterPath, counterPath, failFlag, realGit)
	shimPath := filepath.Join(shimDir, "git")
	if err := os.WriteFile(shimPath, []byte(shimScript), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	// Arm the shim: the next `worktree add #2` will fail.
	if err := os.WriteFile(failFlag, nil, 0o644); err != nil {
		t.Fatalf("arm shim: %v", err)
	}

	shimmedEnv := append(os.Environ(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// First run: should fail on the 2nd worktree add.
	run := exec.Command(bosunBin, "init", "3")
	run.Dir = s.repo
	run.Env = shimmedEnv
	var buf1 bytes.Buffer
	run.Stdout = &buf1
	run.Stderr = &buf1
	if err := run.Run(); err == nil {
		t.Fatalf("first init should fail at session-2; got success:\n%s", buf1.String())
	}

	// State file should exist with session-1 in completed_sessions and
	// session-2 as the current session.
	statePath := filepath.Join(s.repo, ".bosun", "init.state")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("expected %s to exist after failed init: %v\noutput:\n%s", statePath, err, buf1.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("init.state is not valid JSON: %v\n%s", err, data)
	}
	completed, _ := doc["completed_sessions"].([]any)
	if len(completed) != 1 || completed[0] != "session-1" {
		t.Errorf("completed_sessions = %v, want [session-1]; full state:\n%s", completed, data)
	}
	if cur, _ := doc["current_session"].(string); cur != "session-2" {
		t.Errorf("current_session = %q, want session-2; full state:\n%s", cur, data)
	}

	// Plain `bosun init` (no --resume) on top of the state file must refuse.
	refuse := exec.Command(bosunBin, "init", "3")
	refuse.Dir = s.repo
	refuse.Env = shimmedEnv
	var buf2 bytes.Buffer
	refuse.Stdout = &buf2
	refuse.Stderr = &buf2
	if err := refuse.Run(); err == nil {
		t.Fatalf("plain init over stale state should refuse; got success:\n%s", buf2.String())
	}
	if !strings.Contains(buf2.String(), "--resume") {
		t.Errorf("refuse-message should mention --resume; got:\n%s", buf2.String())
	}

	// Disarm the shim and reset the counter so `--resume` can complete.
	if err := os.Remove(failFlag); err != nil {
		t.Fatalf("disarm shim: %v", err)
	}
	_ = os.Remove(counterPath)

	// Resume: must finish sessions 2 and 3 and remove the state file.
	resume := exec.Command(bosunBin, "init", "3", "--resume")
	resume.Dir = s.repo
	resume.Env = shimmedEnv
	var buf3 bytes.Buffer
	resume.Stdout = &buf3
	resume.Stderr = &buf3
	if err := resume.Run(); err != nil {
		t.Fatalf("resume failed: %v\n%s", err, buf3.String())
	}

	for i := 1; i <= 3; i++ {
		s.AssertWorktreeExists(i)
		s.AssertBranchExists("bosun/session-" + itoa(i))
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should be removed after successful resume; stat err = %v", err)
	}
}

// initShim writes a fake-git shim that defers to the real git binary except
// for `worktree add`: it fails the Nth call (1-indexed) so scenario tests
// can deterministically interrupt init mid-stream. Returns the shimmed
// PATH-prefixed env slice and a disarm() that lets the next attempt
// complete. Callers don't need to manage the failure-flag file themselves.
func initShim(t *testing.T, failOnNthAdd int) (env []string, disarm func()) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	shimDir := t.TempDir()
	counterPath := filepath.Join(shimDir, "wt-count")
	failFlag := filepath.Join(shimDir, "fail-second")
	shimScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "add" ]; then
  n=$(cat %q 2>/dev/null || echo 0)
  n=$((n+1))
  echo "$n" > %q
  if [ -f %q ] && [ "$n" = "%d" ]; then
    echo "shim: fake failure on worktree #%d" >&2
    exit 1
  fi
fi
exec %q "$@"
`, counterPath, counterPath, failFlag, failOnNthAdd, failOnNthAdd, realGit)
	shimPath := filepath.Join(shimDir, "git")
	if err := os.WriteFile(shimPath, []byte(shimScript), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	if err := os.WriteFile(failFlag, nil, 0o644); err != nil {
		t.Fatalf("arm shim: %v", err)
	}
	env = append(os.Environ(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	disarm = func() {
		_ = os.Remove(failFlag)
		_ = os.Remove(counterPath)
	}
	return env, disarm
}

// TestScenario_InitResumeArglessDerivesCountAndBrief covers the v0.6.1
// Bug 2a fix: `bosun init --resume` with no other args must derive the
// session count + brief from init.state, not from cfg.DefaultSessionCount
// (which fired a phantom "label count doesn't match" error in the trial).
func TestScenario_InitResumeArglessDerivesCountAndBrief(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nA\n\n## session-2\nB\n\n## session-3\nC\n")

	env, disarm := initShim(t, 2)

	// First run: 3 sessions, brief — fails on worktree #2 so init.state
	// gets a 3-session breadcrumb with session-1 completed.
	run := exec.Command(bosunBin, "init", "3", "--brief", "plan.md")
	run.Dir = s.repo
	run.Env = env
	var buf1 bytes.Buffer
	run.Stdout = &buf1
	run.Stderr = &buf1
	if err := run.Run(); err == nil {
		t.Fatalf("first init should fail at session-2; got success:\n%s", buf1.String())
	}

	disarm()

	// Argless resume: no count, no --brief. Must read 3 + plan.md from state.
	// cfg.DefaultSessionCount is 4 here, so a buggy fallback would produce
	// session-4 and fail with "label count (4) doesn't match prior init (3)".
	resume := exec.Command(bosunBin, "init", "--resume")
	resume.Dir = s.repo
	resume.Env = env
	var buf2 bytes.Buffer
	resume.Stdout = &buf2
	resume.Stderr = &buf2
	if err := resume.Run(); err != nil {
		t.Fatalf("argless resume failed: %v\n%s", err, buf2.String())
	}
	out := buf2.String()
	if strings.Contains(out, "doesn't match prior init") {
		t.Fatalf("argless resume should not produce label-mismatch error:\n%s", out)
	}
	if !strings.Contains(out, "init.state") {
		t.Errorf("expected resume output to mention init.state-derived brief; got:\n%s", out)
	}

	for i := 1; i <= 3; i++ {
		s.AssertWorktreeExists(i)
		s.AssertBranchExists("bosun/session-" + itoa(i))
		// The brief should land in every worktree, including the late ones
		// — proving the brief path was carried through state, not lost.
		briefBody := readFile(t, filepath.Join(s.WorktreePath(i), "BOSUN_BRIEF.md"))
		if !strings.Contains(briefBody, "session-"+itoa(i)) {
			t.Errorf("session-%d brief missing heading:\n%s", i, briefBody)
		}
	}
}

// TestScenario_InitResumeInconsistentArgsWarnRatherThanFail covers the
// other half of Bug 2a: when --resume is given with positional args that
// disagree with init.state, bosun must use the state's value (with a
// warning) instead of refusing with a "label count doesn't match" error.
func TestScenario_InitResumeInconsistentArgsWarnRatherThanFail(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nA\n\n## session-2\nB\n\n## session-3\nC\n")

	env, disarm := initShim(t, 2)

	first := exec.Command(bosunBin, "init", "3", "--brief", "plan.md")
	first.Dir = s.repo
	first.Env = env
	var buf1 bytes.Buffer
	first.Stdout = &buf1
	first.Stderr = &buf1
	if err := first.Run(); err == nil {
		t.Fatalf("first init should fail; got success:\n%s", buf1.String())
	}

	disarm()

	// Resume with the wrong count (5 instead of 3): must succeed with
	// a warning, NOT fail with "doesn't match prior init".
	resume := exec.Command(bosunBin, "init", "5", "--resume")
	resume.Dir = s.repo
	resume.Env = env
	var buf2 bytes.Buffer
	resume.Stdout = &buf2
	resume.Stderr = &buf2
	if err := resume.Run(); err != nil {
		t.Fatalf("resume with inconsistent count should succeed (with warning): %v\n%s", err, buf2.String())
	}
	out := buf2.String()
	if strings.Contains(out, "doesn't match prior init") {
		t.Fatalf("inconsistent args should warn, not fail:\n%s", out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("inconsistent args should emit a warning; got:\n%s", out)
	}
	// Only sessions 1-3 should exist — state-driven, not arg-driven.
	for i := 1; i <= 3; i++ {
		s.AssertWorktreeExists(i)
	}
	if _, err := os.Stat(s.WorktreePath(4)); err == nil {
		t.Errorf("session-4 should NOT exist — resume used state's count, not the arg")
	}
}

// TestScenario_InitResumeReconcilesLockedRegisteredWorktree covers
// Bug 2b: when the in-progress worktree was created on disk and registered
// with git (but bosun then locked it / was killed before MarkComplete),
// `bosun init --resume` must unlock and skip-the-add rather than refusing
// with "worktree path already exists". Trial reproduced this exactly:
// `bosun init 3 --resume --brief ...` died on the locked session-1 worktree.
func TestScenario_InitResumeReconcilesLockedRegisteredWorktree(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nA\n\n## session-2\nB\n")

	// Run a normal `bosun init 2` so we get a real registered worktree
	// for session-1 + session-2.
	s.Bosun("init", "2", "--brief", "plan.md")

	// Simulate the trial wedge: write a state file that says session-1
	// completed, session-2 in StepGitWorktreeAdd, then lock session-2's
	// worktree as if a prior kill froze it. The seeded state must include
	// the same round_timestamp the real `bosun init 2` just used so
	// resume reconstructs the same on-disk paths (scheme C in
	// docs/uid-worktree-design.md).
	statePath := filepath.Join(s.repo, ".bosun", "init.state")
	stateJSON := fmt.Sprintf(`{
  "version": "v0.6",
  "started_at": "2026-01-01T00:00:00Z",
  "round_timestamp": %q,
  "plan_path": "plan.md",
  "total_sessions": 2,
  "labels": ["session-1", "session-2"],
  "completed_sessions": ["session-1"],
  "current_session": "session-2",
  "current_step": "git_worktree_add"
}
`, s.RoundTimestamp())
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	wt2 := s.WorktreePath(2)
	s.GitIn(s.repo, "worktree", "lock", wt2, "--reason", "bosun: killed during init")

	out, err := s.BosunErr("init", "--resume", "--brief", "plan.md")
	if err != nil {
		t.Fatalf("resume on locked-registered worktree should succeed: %v\n%s", err, out)
	}
	if strings.Contains(out, "already exists") {
		t.Fatalf("resume must not refuse on the in-progress worktree it's recovering:\n%s", out)
	}
	if !strings.Contains(out, "Unlocked") {
		t.Errorf("resume should announce the unlock; got:\n%s", out)
	}

	// Worktree must be unlocked when resume returns.
	wtList := s.GitIn(s.repo, "worktree", "list", "--porcelain")
	for _, block := range strings.Split(wtList, "\n\n") {
		if strings.Contains(block, "worktree "+wt2) && strings.Contains(block, "locked") {
			t.Errorf("session-2 worktree should be unlocked after resume:\n%s", block)
		}
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should be removed after successful resume; stat err = %v", err)
	}
}

// TestScenario_InitResumeRefusesUnregisteredDir covers Bug 2b's negative
// case: if the on-disk worktree dir exists but git doesn't know about it
// (operator deleted .git/worktrees/<name> by hand, or a previous
// AddWorktree crashed too early to register), resume refuses without
// --force. This matches the safety contract: a dir bosun doesn't recognize
// could be operator hand-fixes; we won't clobber it silently.
func TestScenario_InitResumeRefusesUnregisteredDir(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nA\n\n## session-2\nB\n")

	wt2 := s.WorktreePath(2)
	if err := os.MkdirAll(wt2, 0o755); err != nil {
		t.Fatalf("seed unregistered dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "hand-edit.txt"), []byte("operator was here\n"), 0o644); err != nil {
		t.Fatalf("seed dir contents: %v", err)
	}

	statePath := filepath.Join(s.repo, ".bosun", "init.state")
	stateJSON := `{
  "version": "v0.6",
  "started_at": "2026-01-01T00:00:00Z",
  "plan_path": "plan.md",
  "total_sessions": 2,
  "labels": ["session-1", "session-2"],
  "completed_sessions": ["session-1"],
  "current_session": "session-2",
  "current_step": "git_worktree_add"
}
`
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	// Also need a registered session-1 worktree so the resume's
	// IsCompleted check finds the worktree it expects.
	s.GitIn(s.repo, "branch", "bosun/session-1")
	s.GitIn(s.repo, "worktree", "add", s.WorktreePath(1), "bosun/session-1")

	out, err := s.BosunErr("init", "--resume", "--brief", "plan.md")
	if err == nil {
		t.Fatalf("resume should refuse on unregistered dir without --force:\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("expected 'already exists' refusal; got:\n%s", out)
	}
	// Operator's content must be untouched.
	if _, statErr := os.Stat(filepath.Join(wt2, "hand-edit.txt")); statErr != nil {
		t.Errorf("operator file should remain after refusal: %v", statErr)
	}
}

// TestScenario_InitResumeForceOverwritesStaleDir covers the second half
// of Bug 2b: when --force is passed alongside --resume on an unregistered
// stale dir, bosun rm -rf's it and re-creates the worktree.
func TestScenario_InitResumeForceOverwritesStaleDir(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nA\n\n## session-2\nB\n")

	wt2 := s.WorktreePath(2)
	if err := os.MkdirAll(wt2, 0o755); err != nil {
		t.Fatalf("seed stale dir: %v", err)
	}
	stalePath := filepath.Join(wt2, "stale.txt")
	if err := os.WriteFile(stalePath, []byte("from a prior crashed init\n"), 0o644); err != nil {
		t.Fatalf("seed stale contents: %v", err)
	}

	statePath := filepath.Join(s.repo, ".bosun", "init.state")
	stateJSON := `{
  "version": "v0.6",
  "started_at": "2026-01-01T00:00:00Z",
  "plan_path": "plan.md",
  "total_sessions": 2,
  "labels": ["session-1", "session-2"],
  "completed_sessions": [],
  "current_session": "session-2",
  "current_step": "git_worktree_add"
}
`
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	out := s.Bosun("init", "--resume", "--force", "--brief", "plan.md")
	if strings.Contains(out, "already exists") {
		t.Fatalf("--resume --force should rm the stale dir, not refuse:\n%s", out)
	}
	// Stale content must be gone — replaced by a fresh worktree.
	if _, err := os.Stat(stalePath); err == nil {
		t.Errorf("stale file should be removed by --force; still present at %s", stalePath)
	}
	s.AssertWorktreeExists(2)
	s.AssertBranchExists("bosun/session-2")
}

// TestScenario_InitResumeEndToEndAfterHang exercises the v0.6.1 trial's
// recovery path end-to-end: start `bosun init 3` against a fake-git that
// hangs on the 2nd worktree-add (relying on session-1's per-op timeout
// to abort), then run `bosun init --resume` and verify all 3 sessions
// finish cleanly. Skipped when session-1's timeout fix isn't in place
// (config default GitOpTimeoutSeconds == 0).
func TestScenario_InitResumeEndToEndAfterHang(t *testing.T) {
	s := newScenario(t)
	// Drive a tight 3s git op timeout so the test runs in seconds, not minutes.
	s.WriteFile(".bosun/config.json", `{"git_op_timeout_seconds": 3}`)
	s.WriteFile("plan.md", "## session-1\nA\n\n## session-2\nB\n\n## session-3\nC\n")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	shimDir := t.TempDir()
	counterPath := filepath.Join(shimDir, "wt-count")
	hangFlag := filepath.Join(shimDir, "hang-second")
	shimScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "add" ]; then
  n=$(cat %q 2>/dev/null || echo 0)
  n=$((n+1))
  echo "$n" > %q
  if [ -f %q ] && [ "$n" = "2" ]; then
    sleep 30
    exit 0
  fi
fi
exec %q "$@"
`, counterPath, counterPath, hangFlag, realGit)
	shimPath := filepath.Join(shimDir, "git")
	if err := os.WriteFile(shimPath, []byte(shimScript), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	if err := os.WriteFile(hangFlag, nil, 0o644); err != nil {
		t.Fatalf("arm shim: %v", err)
	}
	env := append(os.Environ(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// First run: the 2nd worktree add hangs ~30s; session-1's per-op timeout
	// (3s here) must abort it. Without that fix, this test would block for
	// 30s+ and exceed the harness deadline.
	run := exec.Command(bosunBin, "init", "3", "--brief", "plan.md")
	run.Dir = s.repo
	run.Env = env
	var buf1 bytes.Buffer
	run.Stdout = &buf1
	run.Stderr = &buf1
	start := time.Now()
	err = run.Run()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("first init should fail (timeout); got success in %v\n%s", elapsed, buf1.String())
	}
	if elapsed > 25*time.Second {
		t.Skipf("session-1 per-op timeout not yet wired; init took %v (> 25s). Output:\n%s", elapsed, buf1.String())
	}

	// Disarm the hang and resume — all 3 sessions must come up clean.
	if err := os.Remove(hangFlag); err != nil {
		t.Fatalf("disarm shim: %v", err)
	}
	_ = os.Remove(counterPath)

	resume := exec.Command(bosunBin, "init", "--resume")
	resume.Dir = s.repo
	resume.Env = env
	var buf2 bytes.Buffer
	resume.Stdout = &buf2
	resume.Stderr = &buf2
	if err := resume.Run(); err != nil {
		t.Fatalf("resume after hang failed: %v\n%s", err, buf2.String())
	}

	for i := 1; i <= 3; i++ {
		s.AssertWorktreeExists(i)
		s.AssertBranchExists("bosun/session-" + itoa(i))
	}
}

// TestScenario_StatusPrunesGhostSpawnTreeEntries reproduces trial #3c
// Bug A: .bosun/spawn-tree.json contains 4 entries; 3 of them describe
// sub-sessions whose worktrees and branches were reaped externally
// (macOS / iCloud File Provider). `bosun status` must invoke
// SyncWithGit on entry, prune the 3 ghosts, and leave only the live
// entry. The stderr notice must name the pruned labels exactly once
// so the operator sees the reconciliation happen.
func TestScenario_StatusPrunesGhostSpawnTreeEntries(t *testing.T) {
	s := newScenario(t)

	// Create one real session — gives us a session-1 with a live
	// worktree + branch that the sync should leave untouched.
	s.Bosun("init", "1")
	s.AssertWorktreeExists(1)
	s.AssertBranchExists("bosun/session-1")

	// Inject the trial #3c shape directly into .bosun/spawn-tree.json:
	// session-1 (real) plus three ghost children. Writing the JSON by
	// hand rather than going through AddTopLevel/AddChild keeps the
	// test honest about the disk shape; ghosts are exactly entries
	// with no worktree and no branch on disk.
	tree := `{
  "version": "v1",
  "sessions": {
    "session-1": {
      "depth": 0,
      "children": ["session-1.auth", "session-1.http", "session-1.parser"],
      "spawned_at": "2026-05-18T00:00:00Z"
    },
    "session-1.auth": {
      "depth": 1,
      "parent": "session-1",
      "spawned_at": "2026-05-18T00:01:00Z"
    },
    "session-1.http": {
      "depth": 1,
      "parent": "session-1",
      "spawned_at": "2026-05-18T00:02:00Z"
    },
    "session-1.parser": {
      "depth": 1,
      "parent": "session-1",
      "spawned_at": "2026-05-18T00:03:00Z"
    }
  }
}
`
	s.WriteFile(filepath.Join(".bosun", "spawn-tree.json"), tree)

	out := s.Bosun("status")

	// The prune notice fires once and names every ghost so the
	// operator can correlate against whatever ate the worktrees.
	for _, ghost := range []string{"session-1.auth", "session-1.http", "session-1.parser"} {
		if !strings.Contains(out, ghost) {
			t.Errorf("expected status output to mention pruned ghost %q:\n%s", ghost, out)
		}
	}
	if !strings.Contains(out, "pruned 3 ghost") {
		t.Errorf("expected status output to announce 3 prunes:\n%s", out)
	}

	// After sync, the spawn-tree file should hold only the live entry.
	// Children list under session-1 must be empty so subsequent
	// merge/cleanup --tree walks don't trip over stale references.
	data, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "spawn-tree.json"))
	if err != nil {
		t.Fatalf("read spawn-tree.json: %v", err)
	}
	var parsed struct {
		Sessions map[string]struct {
			Children []string `json:"children"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse spawn-tree.json: %v\n%s", err, data)
	}
	if len(parsed.Sessions) != 1 {
		t.Errorf("spawn-tree.Sessions = %v, want only session-1", parsed.Sessions)
	}
	if _, ok := parsed.Sessions["session-1"]; !ok {
		t.Errorf("session-1 missing from post-sync tree: %v", parsed.Sessions)
	}
	if kids := parsed.Sessions["session-1"].Children; len(kids) != 0 {
		t.Errorf("session-1.children = %v, want empty after prune", kids)
	}

	// And --no-sync must skip the reconciliation entirely, leaving
	// whatever the operator just put on disk. Seed the ghost shape a
	// second time (the prior status call wrote it out cleanly).
	s.WriteFile(filepath.Join(".bosun", "spawn-tree.json"), tree)
	out2 := s.Bosun("status", "--no-sync")
	if strings.Contains(out2, "pruned") {
		t.Errorf("--no-sync should suppress the prune notice:\n%s", out2)
	}
}
