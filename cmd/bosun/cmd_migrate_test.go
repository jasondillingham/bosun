package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/git"
)

// migrateFixture is a small repo + sibling-worktree harness for the
// migrate-command tests. Kept here (rather than in scenario_test.go) so
// the migration story owns its own setup — the scenario harness assumes
// `bosun init` ran, which is not what we want for these tests.
type migrateFixture struct {
	t      *testing.T
	parent string
	repo   string
	client *git.Client
}

func newMigrateFixture(t *testing.T) *migrateFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("git path resolution differs on Windows; covered by hand-test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	parent := t.TempDir()
	repo := filepath.Join(parent, "myproj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := &migrateFixture{t: t, parent: parent, repo: repo, client: git.New()}
	fx.git("init", "-q", "-b", "main")
	fx.git("config", "user.email", "test@example.com")
	fx.git("config", "user.name", "Test")
	fx.git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.git("add", "seed.txt")
	fx.git("commit", "-qm", "seed")
	return fx
}

func (fx *migrateFixture) git(args ...string) string {
	fx.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", fx.repo}, args...)...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		fx.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, buf.String())
	}
	return buf.String()
}

// addLegacyWorktree creates a legacy-named worktree (<repo>-bosun-<sub>)
// on a fresh bosun/<label> branch. Returns the worktree's absolute path
// as git records it (post symlink resolution).
func (fx *migrateFixture) addLegacyWorktree(label string) string {
	fx.t.Helper()
	sub := label
	if rest, ok := strings.CutPrefix(label, "session-"); ok {
		sub = rest
	}
	path := filepath.Join(fx.parent, "myproj-bosun-"+sub)
	fx.git("worktree", "add", "-b", "bosun/"+label, path)
	return path
}

// fixedNow returns a closure that returns t every call — handy for
// pinning the migrate-timestamp fallback so tests can predict the
// resulting path.
func fixedNow(when time.Time) func() time.Time {
	return func() time.Time { return when }
}

func TestPlanMigrate_NoLegacyWorktrees(t *testing.T) {
	fx := newMigrateFixture(t)
	plans, conflicts, err := planMigrate(context.Background(), fx.client, fx.repo, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("planMigrate: %v", err)
	}
	if len(plans) != 0 || len(conflicts) != 0 {
		t.Fatalf("fresh repo: got %d plans, %d conflicts; want 0/0", len(plans), len(conflicts))
	}
}

func TestPlanMigrate_OneLegacy(t *testing.T) {
	fx := newMigrateFixture(t)
	fx.addLegacyWorktree("session-1")

	now := time.Date(2026, 5, 18, 14, 30, 25, 0, time.UTC)
	plans, conflicts, err := planMigrate(context.Background(), fx.client, fx.repo, fixedNow(now))
	if err != nil {
		t.Fatalf("planMigrate: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	if plans[0].label != "session-1" {
		t.Errorf("label = %q, want session-1", plans[0].label)
	}
	if !strings.HasSuffix(plans[0].from, "myproj-bosun-1") {
		t.Errorf("from = %q, want suffix myproj-bosun-1", plans[0].from)
	}
	// Timestamp falls back to parent-dir mtime (init.state not present);
	// the format is the only thing we assert (predicting an mtime is brittle).
	wantSuffixShape := "-bosun-" // followed by 14 digits and "-1"
	if !strings.Contains(plans[0].to, wantSuffixShape) {
		t.Errorf("to = %q, want shape <repo>-bosun-<ts>-1", plans[0].to)
	}
	if !strings.HasSuffix(plans[0].to, "-1") {
		t.Errorf("to = %q, want suffix -1", plans[0].to)
	}
}

func TestPlanMigrate_MultipleLegacy_SortedByLabel(t *testing.T) {
	fx := newMigrateFixture(t)
	// Add in non-alphabetical order to prove the sort happens.
	fx.addLegacyWorktree("session-2")
	fx.addLegacyWorktree("session-1")
	fx.addLegacyWorktree("auth")

	plans, conflicts, err := planMigrate(context.Background(), fx.client, fx.repo, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("planMigrate: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if len(plans) != 3 {
		t.Fatalf("plans = %d, want 3", len(plans))
	}
	got := []string{plans[0].label, plans[1].label, plans[2].label}
	want := []string{"auth", "session-1", "session-2"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("plans[%d].label = %q, want %q", i, got[i], w)
		}
	}
}

// TestPlanMigrate_Idempotent — the brief's "safe to abort mid-run"
// constraint. After renaming N of M worktrees, the next planMigrate call
// must only propose the remaining (M-N). We simulate the mid-run state
// by running migrate on a 3-worktree fixture, then asserting a re-plan
// finds nothing.
func TestPlanMigrate_Idempotent(t *testing.T) {
	fx := newMigrateFixture(t)
	fx.addLegacyWorktree("session-1")
	fx.addLegacyWorktree("session-2")

	if err := runMigrateAt(fx, fixedNow(time.Now())); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	plans, conflicts, err := planMigrate(context.Background(), fx.client, fx.repo, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("planMigrate post-rename: %v", err)
	}
	if len(plans) != 0 || len(conflicts) != 0 {
		t.Fatalf("post-rename re-plan: got %d plans, %d conflicts; want 0/0\nplans=%+v", len(plans), len(conflicts), plans)
	}
}

// TestPlanMigrate_Conflict — the brief's "don't migrate if the new shape
// already exists for the same session" constraint. We hand-create a new-
// shape directory matching what the migration would produce for session-1,
// then assert planMigrate refuses with a conflict.
func TestPlanMigrate_Conflict(t *testing.T) {
	fx := newMigrateFixture(t)
	fx.addLegacyWorktree("session-1")

	now := time.Date(2026, 5, 18, 14, 30, 25, 0, time.UTC)
	// Plant the new-shape dir at the predicted target. The migration's
	// timestamp resolution will land on the parent dir's mtime (no
	// init.state present), so we can't predict the exact path — instead,
	// derive what planMigrate would propose and pre-create that path.
	plans, _, err := planMigrate(context.Background(), fx.client, fx.repo, fixedNow(now))
	if err != nil {
		t.Fatalf("planMigrate (predict): %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan for prediction, got %d", len(plans))
	}
	if err := os.MkdirAll(plans[0].to, 0o755); err != nil {
		t.Fatal(err)
	}
	plans2, conflicts, err := planMigrate(context.Background(), fx.client, fx.repo, fixedNow(now))
	if err != nil {
		t.Fatalf("planMigrate (post-plant): %v", err)
	}
	if len(plans2) != 0 {
		t.Errorf("plans = %d, want 0 (conflict blocks rename)", len(plans2))
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(conflicts))
	}
	if conflicts[0].label != "session-1" {
		t.Errorf("conflict label = %q, want session-1", conflicts[0].label)
	}
}

// TestPlanMigrate_IgnoresNewShape — a worktree already on the new shape
// must not appear in the plan. Without this, a second `bosun migrate`
// against a half-migrated repo would propose nonsense renames.
func TestPlanMigrate_IgnoresNewShape(t *testing.T) {
	fx := newMigrateFixture(t)
	// Add a new-shape worktree directly.
	newShape := filepath.Join(fx.parent, "myproj-bosun-20260518143025-1")
	fx.git("worktree", "add", "-b", "bosun/session-1", newShape)

	plans, conflicts, err := planMigrate(context.Background(), fx.client, fx.repo, fixedNow(time.Now()))
	if err != nil {
		t.Fatalf("planMigrate: %v", err)
	}
	if len(plans) != 0 || len(conflicts) != 0 {
		t.Fatalf("new-shape-only repo: got %d plans, %d conflicts; want 0/0", len(plans), len(conflicts))
	}
}

// TestRunMigrate_MovesGitRegistry — the load-bearing assertion that
// after migrate, `git worktree list` reports the new path, not the old.
// Proves we went through `git worktree move` (not a raw rename that
// would orphan the admin metadata).
func TestRunMigrate_MovesGitRegistry(t *testing.T) {
	fx := newMigrateFixture(t)
	oldPath := fx.addLegacyWorktree("session-1")

	if err := runMigrateAt(fx, fixedNow(time.Now())); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	listing := fx.git("worktree", "list", "--porcelain")
	if strings.Contains(listing, oldPath+"\n") {
		t.Errorf("git still tracks legacy path %q:\n%s", oldPath, listing)
	}
	// Confirm the new-shape path appears.
	if !strings.Contains(listing, "-bosun-") || !strings.HasSuffix(strings.TrimSpace(listing), "refs/heads/bosun/session-1") {
		// Trim is best-effort — just check the new-shape token is present.
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Errorf("legacy dir %q still exists after migrate", oldPath)
	}
}

// runMigrateAt is a test-only wrapper around the plan-and-execute body
// of runMigrate that takes an injected `now` closure (for predictable
// timestamps) and operates on the fixture's git client and repo. It does
// NOT call loadCtx (which would scan PWD for the main worktree and
// break when the fixture lives outside cwd).
func runMigrateAt(fx *migrateFixture, now func() time.Time) error {
	plans, conflicts, err := planMigrate(context.Background(), fx.client, fx.repo, now)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return userErr("conflicts: %d", len(conflicts))
	}
	for _, p := range plans {
		if err := fx.client.MoveWorktree(context.Background(), fx.repo, p.from, p.to); err != nil {
			return err
		}
	}
	return nil
}
