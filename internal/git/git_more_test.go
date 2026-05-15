package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRepoRoot(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"rev-parse", "--show-toplevel"},
		stdout: "/some/repo\n",
	})
	got, err := c.RepoRoot(context.Background(), "/anywhere")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/some/repo" {
		t.Fatalf("RepoRoot = %q, want /some/repo", got)
	}
}

func TestMainWorktreePath(t *testing.T) {
	// The function does filepath.Dir on git's --git-common-dir output, so
	// the expected value is OS-native (filepath.Dir uses OS separators).
	commonDir := filepath.Join(string(filepath.Separator)+"repo", ".git")
	want := filepath.Dir(commonDir)

	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"rev-parse", "--path-format=absolute", "--git-common-dir"},
		stdout: commonDir + "\n",
	})
	got, err := c.MainWorktreePath(context.Background(), "/anywhere")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("MainWorktreePath = %q, want %q", got, want)
	}
}

func TestCreateBranch(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{args: []string{"branch", "bosun/session-1", "main"}})
	if err := c.CreateBranch(context.Background(), "/repo", "bosun/session-1", "main"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteBranch_SafeAndForce(t *testing.T) {
	c, r := newFakeClient(t,
		runMatcher{args: []string{"branch", "-d", "bosun/session-1"}},
		runMatcher{args: []string{"branch", "-D", "bosun/session-2"}},
	)
	if err := c.DeleteBranch(context.Background(), "/repo", "bosun/session-1", false); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBranch(context.Background(), "/repo", "bosun/session-2", true); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(r.calls))
	}
}

func TestAddRemoveWorktree(t *testing.T) {
	c, _ := newFakeClient(t,
		runMatcher{args: []string{"worktree", "add", "/repo-bosun-1", "bosun/session-1"}},
		runMatcher{args: []string{"worktree", "remove", "/repo-bosun-1"}},
		runMatcher{args: []string{"worktree", "remove", "/repo-bosun-1", "--force"}},
	)
	if err := c.AddWorktree(context.Background(), "/repo", "/repo-bosun-1", "bosun/session-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveWorktree(context.Background(), "/repo", "/repo-bosun-1", false); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveWorktree(context.Background(), "/repo", "/repo-bosun-1", true); err != nil {
		t.Fatal(err)
	}
}

func TestMergeAndCommit(t *testing.T) {
	c, _ := newFakeClient(t,
		runMatcher{args: []string{"merge", "--squash", "bosun/session-1"}},
		runMatcher{args: []string{"commit", "-m", "merge: bosun/session-1"}},
		runMatcher{args: []string{"merge", "--no-ff", "bosun/session-2", "-m", "msg"}},
		runMatcher{args: []string{"merge", "--abort"}},
	)
	ctx := context.Background()
	if err := c.MergeSquash(ctx, "/repo", "bosun/session-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(ctx, "/repo", "merge: bosun/session-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.MergeNoFF(ctx, "/repo", "bosun/session-2", "msg"); err != nil {
		t.Fatal(err)
	}
	if err := c.MergeAbort(ctx, "/repo"); err != nil {
		t.Fatal(err)
	}
}

func TestLogN(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"log", "-10", "--oneline", "--decorate"},
		stdout: "abc One\ndef Two\n",
	})
	got, err := c.LogN(context.Background(), "/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc One\ndef Two\n" {
		t.Fatalf("LogN = %q", got)
	}
}

func TestAppendWorktreeExclude(t *testing.T) {
	dir := t.TempDir()
	infoPath := filepath.Join(dir, "info", "exclude")

	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"rev-parse", "--git-path", "info/exclude"},
		stdout: infoPath + "\n",
	})
	if err := c.AppendWorktreeExclude(context.Background(), dir, "BOSUN_BRIEF.md"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatalf("exclude not written: %v", err)
	}
	if string(data) != "BOSUN_BRIEF.md\n" {
		t.Fatalf("exclude content = %q, want %q", string(data), "BOSUN_BRIEF.md\n")
	}
}

func TestListWorktrees_Roundtrip(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"worktree", "list", "--porcelain"},
		stdout: "worktree /repo\nHEAD x\nbranch refs/heads/main\n\n",
	})
	got, err := c.ListWorktrees(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/repo" {
		t.Fatalf("ListWorktrees = %+v", got)
	}
}
