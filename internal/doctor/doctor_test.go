package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRun_AggregatesResults verifies the orchestrator returns one
// Result per check in registration order. Order matters because the
// report renderer relies on it for stable output.
func TestRun_AggregatesResults(t *testing.T) {
	checks := []Check{
		func(context.Context, string) Result {
			return Result{Name: "a", Status: Pass, Message: "ok"}
		},
		func(context.Context, string) Result {
			return Result{Name: "b", Status: Warn, Message: "meh"}
		},
		func(context.Context, string) Result {
			return Result{Name: "c", Status: Fail, Message: "bad"}
		},
	}
	results := Run(context.Background(), "/tmp", checks)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, want := range []string{"a", "b", "c"} {
		if results[i].Name != want {
			t.Errorf("results[%d].Name = %q, want %q", i, results[i].Name, want)
		}
	}
}

func TestWorst_PicksHighestSeverity(t *testing.T) {
	cases := []struct {
		statuses []Status
		want     Status
	}{
		{[]Status{Pass, Pass, Pass}, Pass},
		{[]Status{Pass, Warn, Pass}, Warn},
		{[]Status{Warn, Fail, Pass}, Fail},
		{[]Status{Fail, Fail, Fail}, Fail},
		{nil, Pass},
	}
	for _, tc := range cases {
		var rs []Result
		for _, s := range tc.statuses {
			rs = append(rs, Result{Status: s})
		}
		if got := Worst(rs); got != tc.want {
			t.Errorf("Worst(%v) = %v, want %v", tc.statuses, got, tc.want)
		}
	}
}

func TestWriteReport_FormatsAllSections(t *testing.T) {
	results := []Result{
		{Name: "alpha", Status: Pass, Message: "all good"},
		{Name: "beta", Status: Warn, Message: "be careful", Fix: "do the thing"},
		{Name: "gamma", Status: Fail, Message: "very bad"},
	}
	var buf bytes.Buffer
	WriteReport(&buf, "/repo", results)
	out := buf.String()
	for _, want := range []string{
		"Bosun health check — /repo",
		"alpha: all good",
		"beta: be careful",
		"fix: do the thing",
		"gamma: very bad",
		"failure(s)", // summary line names failures
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestCheckRepoWriteable_HappyPath probes the writeable path; the
// failure path is harder to test portably (can't easily produce a
// read-only dir owned by the current user) and is covered by manual
// invocation when an operator hits the warn.
func TestCheckRepoWriteable_HappyPath(t *testing.T) {
	dir := t.TempDir()
	r := CheckRepoWriteable(context.Background(), dir)
	if r.Status != Pass {
		t.Errorf("CheckRepoWriteable on tempdir = %v, want Pass; message=%q", r.Status, r.Message)
	}
	// Probe file must not survive.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("CheckRepoWriteable left files behind: %+v", entries)
	}
}

func TestCheckStaleInitLock_FreshLockPasses(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bosun", "init.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := CheckStaleInitLock(context.Background(), dir)
	if r.Status != Pass {
		t.Errorf("fresh init.lock = %v, want Pass; message=%q", r.Status, r.Message)
	}
}

func TestCheckStaleInitLock_OldLockWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, ".bosun", "init.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate the lock file 2 hours.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	r := CheckStaleInitLock(context.Background(), dir)
	if r.Status != Warn {
		t.Errorf("2h-old init.lock = %v, want Warn; message=%q", r.Status, r.Message)
	}
	if r.Fix == "" {
		t.Errorf("warn result missing Fix hint")
	}
}

func TestCheckStaleInitLock_MissingLockPasses(t *testing.T) {
	dir := t.TempDir()
	r := CheckStaleInitLock(context.Background(), dir)
	if r.Status != Pass {
		t.Errorf("no lock file = %v, want Pass", r.Status)
	}
}

func TestCheckPhantomBranchRefs_Empty(t *testing.T) {
	dir := t.TempDir()
	r := CheckPhantomBranchRefs(context.Background(), dir)
	if r.Status != Pass {
		t.Errorf("empty bosun refs dir = %v, want Pass; %q", r.Status, r.Message)
	}
}

func TestCheckPhantomBranchRefs_DetectsPhantoms(t *testing.T) {
	dir := t.TempDir()
	refsDir := filepath.Join(dir, ".git", "refs", "heads", "bosun")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real ref + Spotlight phantom + iCloud phantom.
	for _, name := range []string{"session-1", "session-1 2", "session-1 (1)"} {
		if err := os.WriteFile(filepath.Join(refsDir, name), []byte("deadbeef\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := CheckPhantomBranchRefs(context.Background(), dir)
	if r.Status != Warn {
		t.Errorf("two phantoms among three refs = %v, want Warn; %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "2 phantom") {
		t.Errorf("expected phantom count in message; got %q", r.Message)
	}
}

func TestHasDigitSuffix(t *testing.T) {
	cases := []struct {
		s, sep string
		want   bool
	}{
		{"session-1", " ", false},
		{"session-1 2", " ", true},
		{"session-1 99", " ", true},
		{"session-1 abc", " ", false},
		{"session-1 ", " ", false},
		{"", " ", false},
	}
	for _, tc := range cases {
		if got := hasDigitSuffix(tc.s, tc.sep); got != tc.want {
			t.Errorf("hasDigitSuffix(%q, %q) = %v, want %v", tc.s, tc.sep, got, tc.want)
		}
	}
}

func TestHasParenDigitSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"session-1", false},
		{"session-1 (1)", true},
		{"session-1 (12)", true},
		{"session-1 ()", false},
		{"session-1 (abc)", false},
		{"name(1)", true}, // no space; still has the shape
	}
	for _, tc := range cases {
		if got := hasParenDigitSuffix(tc.in); got != tc.want {
			t.Errorf("hasParenDigitSuffix(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestCheckGitVersion_RealGit uses the host's git binary. Skipped if
// git isn't on PATH — the check itself would fail, but the test
// shouldn't depend on test-env quality.
func TestCheckGitVersion_RealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := CheckGitVersion(context.Background(), "")
	// We can't assert Pass without knowing the host git version, but
	// we CAN assert the check produced a parseable result.
	if r.Name != "git-version" {
		t.Errorf("Name = %q, want git-version", r.Name)
	}
	if r.Status == Fail && !strings.Contains(r.Message, "git") {
		t.Errorf("Fail status but message doesn't mention git: %q", r.Message)
	}
}

func TestCheckOrphanWorktrees_NoneFound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Initialize a real git repo so listWorktrees succeeds.
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", repo, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	r := CheckOrphanWorktrees(context.Background(), repo)
	if r.Status != Pass {
		t.Errorf("fresh repo with no siblings = %v, want Pass; %q", r.Status, r.Message)
	}
}

// TestApplyFixes_DryRunDoesNotInvokeFn pins the dry-run contract: the
// preview path never touches FixFn. Operators rely on this to inspect
// what --fix WOULD do without any state mutation.
func TestApplyFixes_DryRunDoesNotInvokeFn(t *testing.T) {
	called := false
	results := []Result{
		{
			Name:    "fixable",
			Status:  Warn,
			Message: "needs fix",
			FixFn:   func(string) error { called = true; return nil },
		},
	}
	outcomes := ApplyFixes("/tmp", results, true)
	if called {
		t.Fatal("FixFn was called during --dry-run")
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(outcomes))
	}
	if !outcomes[0].DryRun {
		t.Error("outcome should be marked DryRun=true")
	}
	if outcomes[0].Applied {
		t.Error("Applied should be false for dry-run")
	}
}

// TestApplyFixes_AppliesAndRecordsResults pins the live-run contract.
func TestApplyFixes_AppliesAndRecordsResults(t *testing.T) {
	calls := []string{}
	wantErr := errors.New("boom")
	results := []Result{
		{Name: "pass", Status: Pass, FixFn: func(string) error { calls = append(calls, "pass"); return nil }},
		{Name: "ok", Status: Warn, FixDescription: "did stuff",
			FixFn: func(string) error { calls = append(calls, "ok"); return nil }},
		{Name: "broken", Status: Warn, FixDescription: "would do stuff",
			FixFn: func(string) error { calls = append(calls, "broken"); return wantErr }},
		{Name: "no-fixer", Status: Warn, Message: "fix me by hand"},
	}
	outcomes := ApplyFixes("/tmp", results, false)
	// Pass-status results are skipped even when they have a fixer (no
	// reason to fix something that's already clean).
	for _, c := range calls {
		if c == "pass" {
			t.Error("FixFn ran for a Pass-status result")
		}
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2 (ok + broken; no-fixer is silently skipped)", len(outcomes))
	}
	if !outcomes[0].Applied || outcomes[0].Err != nil {
		t.Errorf("first outcome: Applied=%v Err=%v, want true/nil", outcomes[0].Applied, outcomes[0].Err)
	}
	if outcomes[1].Applied {
		t.Error("second outcome should NOT be Applied=true (FixFn errored)")
	}
	if !errors.Is(outcomes[1].Err, wantErr) {
		t.Errorf("second outcome Err = %v, want %v", outcomes[1].Err, wantErr)
	}
}

// TestCheckStaleInitLock_FixRemoves verifies the init-lock fixer is
// idempotent and removes a real file when invoked. Belt-and-suspenders
// regression for the v0.8.x lane that introduced --fix.
func TestCheckStaleInitLock_FixRemoves(t *testing.T) {
	dir := t.TempDir()
	bosunDir := filepath.Join(dir, ".bosun")
	if err := os.MkdirAll(bosunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(bosunDir, "init.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	r := CheckStaleInitLock(context.Background(), dir)
	if r.FixFn == nil {
		t.Fatal("Warn result should have a FixFn")
	}
	if err := r.FixFn(dir); err != nil {
		t.Fatalf("FixFn: %v", err)
	}
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatal("init.lock still present after fix")
	}
	// Idempotent — second call on already-clean dir must not error.
	if err := r.FixFn(dir); err != nil {
		t.Fatalf("FixFn second call: %v (must be idempotent)", err)
	}
}

func TestCheckOrphanWorktrees_DetectsOrphan(t *testing.T) {
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
	// Sibling dir matching the bosun naming convention but not
	// registered with git.
	if err := os.MkdirAll(filepath.Join(dir, "myrepo-bosun-session-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := CheckOrphanWorktrees(context.Background(), repo)
	if r.Status != Warn {
		t.Errorf("orphan sibling = %v, want Warn; %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1 orphan") {
		t.Errorf("message missing orphan count; got %q", r.Message)
	}
}
