package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// makeExitError builds a real *exec.ExitError with the given exit code by
// running a shell that exits with that code. Tests need it because
// TreeEqualsBase distinguishes "diff returned 1" (clean signal) from
// "diff failed for another reason" (real error) via errors.As(*ExitError).
func makeExitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+itoaForTest(code)).Run()
	if err == nil {
		t.Fatalf("expected non-nil error from sh exit %d", code)
	}
	return err
}

func itoaForTest(n int) string {
	// Small integer to string — avoids pulling strconv into a tiny test helper.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

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

func TestUnmergedPatches(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   int
	}{
		{"all merged", "- abc Squash subject\n- def Another\n", 0},
		{"mixed", "+ abc Real ahead\n- def Squashed\n+ ghi Also ahead\n", 2},
		{"empty", "", 0},
		{"only unmerged", "+ aaa One\n+ bbb Two\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFakeClient(t, runMatcher{
				args:   []string{"cherry", "main", "bosun/session-1"},
				stdout: tc.stdout,
			})
			got, err := c.UnmergedPatches(context.Background(), "/repo", "main", "bosun/session-1")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("UnmergedPatches = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestChmodWritableTree mirrors the GOMODCACHE shape that --isolate-cache
// produces: read-only files (0o444) inside read-only dirs (0o555). Without
// the pre-pass, os.RemoveAll on this tree fails on the first unlink because
// the parent dir lacks write+execute for the owner. After the pre-pass,
// RemoveAll succeeds.
func TestChmodWritableTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows permission semantics are different; we still chmod, but
		// the test failure mode it guards against is the Unix one.
		t.Skip("permission model differs on Windows")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "go-mod", "cache", "download", "github.com", "x", "y@v1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(sub, "go.mod"),
		filepath.Join(sub, "y.go"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Lock down: files 0o444, dirs 0o555 (bottom-up).
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		return os.Chmod(p, 0o444)
	}); err != nil {
		t.Fatal(err)
	}
	// Walk again to chmod dirs after their contents so we can still descend.
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	// Apply in reverse so children are restricted before parents.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			t.Fatal(err)
		}
	}
	// Restore root so the test cleanup can succeed even if our helper bails.
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			} else {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
	})

	// Sanity: an unaided RemoveAll should fail.
	if err := os.RemoveAll(filepath.Join(root, "go-mod")); err == nil {
		t.Fatal("expected RemoveAll to fail on locked-down tree (test premise)")
	}

	// The helper should make the tree writable…
	if err := chmodWritableTree(root); err != nil {
		t.Fatalf("chmodWritableTree: %v", err)
	}
	// …and a subsequent RemoveAll should now succeed.
	if err := os.RemoveAll(filepath.Join(root, "go-mod")); err != nil {
		t.Fatalf("RemoveAll after chmod: %v", err)
	}
}

// TestChmodWritableTree_DeepRealisticGoModCacheShape stresses the helper
// against a directory layout that more faithfully reflects what
// --isolate-cache leaves behind: many modules, each several path segments
// deep, with read-only files (0o444) inside read-only directories (0o555).
// The original TestChmodWritableTree only exercises one module path; we
// observed during the v0.3.1 bug-hunt cleanup that `bosun cleanup --force`
// still hit Permission denied on real-world GOMODCACHE trees even after the
// pre-chmod ran. This test asserts the helper's invariant holds at scale.
func TestChmodWritableTree_DeepRealisticGoModCacheShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission model differs on Windows")
	}
	root := t.TempDir()

	// 6 different module-like subtrees, each with the kind of nesting
	// gopath sees: <root>/.cache/go-mod/<host>/<org>/<pkg>@<ver>/<sub>/<file>
	modules := []struct {
		host, org, pkg, ver string
	}{
		{"github.com", "google", "jsonschema-go", "v0.4.3"},
		{"github.com", "spf13", "cobra", "v1.10.2"},
		{"github.com", "modelcontextprotocol", "go-sdk", "v1.6.0"},
		{"golang.org", "x", "term", "v0.27.0"},
		{"golang.org", "x", "sys", "v0.41.0"},
		{"github.com", "shirou", "gopsutil", "v3.24.5"},
	}
	for _, m := range modules {
		base := filepath.Join(root, ".cache", "go-mod", m.host, m.org, m.pkg+"@"+m.ver)
		// Two sub-packages per module + several files each, to mimic real density.
		for _, sub := range []string{".", "internal", "internal/util", "cmd/tool"} {
			dir := filepath.Join(base, sub)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			for _, f := range []string{"go.mod", "go.sum", "LICENSE", "README.md", "doc.go", "util.go", "util_test.go"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatalf("write %s: %v", f, err)
				}
			}
		}
	}

	// Lock down (bottom-up): files 0o444, then dirs 0o555.
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chmod(p, 0o444)
	}); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			t.Fatalf("chmod %s: %v", dirs[i], err)
		}
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			} else {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
	})

	// Premise: untouched RemoveAll fails on this shape.
	if err := os.RemoveAll(filepath.Join(root, ".cache")); err == nil {
		t.Fatal("expected unaided RemoveAll to fail on locked-down GOMODCACHE")
	}

	// Helper should make every entry writable…
	if err := chmodWritableTree(root); err != nil {
		t.Fatalf("chmodWritableTree: %v", err)
	}

	// …and the post-condition is binary: nothing under root is read-only.
	// Use the same invariant as the production code (u+w bit must be set).
	var leftReadOnly []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Mode().Perm()&0o200 == 0 {
			leftReadOnly = append(leftReadOnly, p)
		}
		return nil
	})
	if len(leftReadOnly) > 0 {
		t.Fatalf("%d entries still missing u+w after chmodWritableTree; sample: %v",
			len(leftReadOnly), leftReadOnly[:minInt(5, len(leftReadOnly))])
	}

	// Belt-and-suspenders: RemoveAll must now succeed.
	if err := os.RemoveAll(filepath.Join(root, ".cache")); err != nil {
		t.Fatalf("RemoveAll after chmod: %v", err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestChmodWritableTree_MissingRoot just confirms the walk doesn't panic on
// a path that doesn't exist — RemoveWorktree calls Stat first, but the
// helper itself should still degrade cleanly.
func TestChmodWritableTree_MissingRoot(t *testing.T) {
	err := chmodWritableTree(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		// filepath.WalkDir returns an error when the root doesn't exist;
		// that's expected and fine — callers ignore it.
		t.Skip("walk returned nil on missing root; platform-dependent")
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

func TestParseWorktreeList_Prunable(t *testing.T) {
	in := "worktree /repo\nHEAD abc\nbranch refs/heads/main\n\nworktree /repo-bosun-1\nHEAD def\nbranch refs/heads/bosun/session-1\nprunable gitdir file points to non-existent location\n"
	got := parseWorktreeList(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Prunable {
		t.Fatalf("main entry marked prunable")
	}
	if !got[1].Prunable {
		t.Fatalf("session entry not marked prunable")
	}
}

func TestTreeEqualsBase_Equal(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"diff", "--quiet", "main..bosun/session-1", "--"},
		stdout: "",
	})
	got, err := c.TreeEqualsBase(context.Background(), "/repo", "main", "bosun/session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatalf("TreeEqualsBase = false, want true (empty diff)")
	}
}

func TestTreeEqualsBase_Different(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args: []string{"diff", "--quiet", "main..bosun/session-1", "--"},
		err:  makeExitError(t, 1),
	})
	got, err := c.TreeEqualsBase(context.Background(), "/repo", "main", "bosun/session-1")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatalf("TreeEqualsBase = true, want false (exit 1)")
	}
}

func TestTreeEqualsBase_OtherError(t *testing.T) {
	// Exit 128 is git's "bad ref" — surface this as a real error, not "false".
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"diff", "--quiet", "main..bosun/missing", "--"},
		stderr: "fatal: bad revision 'main..bosun/missing'\n",
		err:    makeExitError(t, 128),
	})
	if _, err := c.TreeEqualsBase(context.Background(), "/repo", "main", "bosun/missing"); err == nil {
		t.Fatalf("TreeEqualsBase exit 128 returned nil error")
	}
}

func TestPruneWorktrees(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{args: []string{"worktree", "prune"}})
	if err := c.PruneWorktrees(context.Background(), "/repo"); err != nil {
		t.Fatal(err)
	}
}
