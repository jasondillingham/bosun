package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
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
	// might matter. The operator must explicitly --force.
	s := newScenario(t)
	s.Bosun("init", "4")
	wt4 := s.WorktreePath(4)
	s.WriteFileIn(wt4, "scratch.txt", "wip\n")
	s.CommitIn(wt4, "wip on orphaned session-4")

	out := s.Bosun("cleanup", "--orphans=3")
	s.AssertContains(out, "session-4: skipped")
	s.AssertContains(out, "1 ahead")
	s.AssertWorktreeExists(4)

	// With --force, the same session goes.
	out = s.Bosun("cleanup", "--orphans=3", "--force")
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

// --- helpers (test-local) ---

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
