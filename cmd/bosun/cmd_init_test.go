package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindPhantomBranchRefs_DetectsFinderDuplicates(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".git", "refs", "heads", "bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Real ref + two Finder-style duplicates + an unrelated file that
	// happens to have a space (no trailing digit — should be ignored).
	mustWrite(t, filepath.Join(dir, "session-1"), "abc\n")
	mustWrite(t, filepath.Join(dir, "session-1 2"), "abc\n")
	mustWrite(t, filepath.Join(dir, "session-1 3"), "abc\n")
	mustWrite(t, filepath.Join(dir, "feature thing"), "abc\n")

	phantoms, err := findPhantomBranchRefs(repo, "bosun")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(phantoms) != 2 {
		t.Fatalf("want 2 phantoms, got %d: %v", len(phantoms), phantoms)
	}
}

func TestFindPhantomBranchRefs_MissingDirNotAnError(t *testing.T) {
	repo := t.TempDir()
	phantoms, err := findPhantomBranchRefs(repo, "bosun")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(phantoms) != 0 {
		t.Errorf("want 0 phantoms, got %d", len(phantoms))
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCheckStaleSessionBranch covers the v0.6.2 stale-branch pre-flight in
// isolation. Drives the helper directly against a real on-disk repo so the
// rev-parse / branch-exists paths run end-to-end rather than against
// mocked git output.
//
// Three cases:
//   - branch absent             → no error (proceed)
//   - branch at base HEAD       → no error (equal-SHA is a no-op semantically)
//   - branch diverges from base → userErr with recovery hints
func TestCheckStaleSessionBranch(t *testing.T) {
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@e")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("add", "f")
	runGit("commit", "-q", "-m", "init")

	rc, err := loadCtxAt(repo)
	if err != nil {
		t.Fatalf("loadCtx: %v", err)
	}

	// Case 1: branch absent.
	if err := checkStaleSessionBranch(rc, "bosun/session-99", "main"); err != nil {
		t.Fatalf("expected nil for absent branch, got %v", err)
	}

	// Case 2: branch at base HEAD.
	runGit("branch", "bosun/session-1")
	if err := checkStaleSessionBranch(rc, "bosun/session-1", "main"); err != nil {
		t.Fatalf("expected nil for branch == base HEAD, got %v", err)
	}

	// Case 3: branch diverges from base.
	runGit("checkout", "-q", "bosun/session-1")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("y\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("commit", "-q", "-am", "diverge")
	runGit("checkout", "-q", "main")

	err = checkStaleSessionBranch(rc, "bosun/session-1", "main")
	if err == nil {
		t.Fatal("expected refusal on divergent branch, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"already exists",
		"diverges from main",
		"git branch -D bosun/session-1",
		"--force",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}
}

// loadCtxAt wraps loadCtx for tests that need a runCtx rooted at a
// specific directory rather than the process CWD. Captures and restores
// the working directory around the call.
func loadCtxAt(dir string) (*runCtx, error) {
	prev, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	defer os.Chdir(prev)
	return loadCtx()
}

func TestSameLabels(t *testing.T) {
	// sameLabels gates the --resume "args contradict state" warning. The
	// fix downgraded the contradiction from a fatal error to a warning, so
	// the comparator's correctness is what controls whether the operator
	// sees the warning at all.
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"identical numbered", []string{"session-1", "session-2"}, []string{"session-1", "session-2"}, true},
		{"identical named", []string{"auth", "http"}, []string{"auth", "http"}, true},
		{"differ in count", []string{"session-1", "session-2", "session-3"}, []string{"session-1", "session-2"}, false},
		{"differ in order", []string{"auth", "http"}, []string{"http", "auth"}, false},
		{"differ in content", []string{"auth"}, []string{"storage"}, false},
		{"empty vs non-empty", nil, []string{"session-1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameLabels(tc.a, tc.b); got != tc.want {
				t.Errorf("sameLabels(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
