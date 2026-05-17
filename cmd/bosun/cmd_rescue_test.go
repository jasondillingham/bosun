package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/git"
)

// TestCopyRescueFiles_CopiesContent walks copyRescueFiles end-to-end on a
// fixture worktree: two modified files and one untracked file. All three
// must land under dest with bytes preserved and the relative-path layout
// intact (so the operator can run `diff -r` against the original worktree
// later).
func TestCopyRescueFiles_CopiesContent(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "a.go"), "package a\n// modified\n")
	writeFile(t, filepath.Join(wt, "sub", "b.go"), "package b\n// modified\n")
	writeFile(t, filepath.Join(wt, "new.txt"), "fresh untracked\n")

	dest := filepath.Join(t.TempDir(), "snapshot")
	lines := []git.PorcelainStatusLine{
		{XY: " M", Path: "a.go"},
		{XY: " M", Path: "sub/b.go"},
		{XY: "??", Path: "new.txt"},
	}
	n, err := copyRescueFiles(wt, dest, lines)
	if err != nil {
		t.Fatalf("copyRescueFiles: %v", err)
	}
	if n != 3 {
		t.Errorf("copied = %d, want 3", n)
	}
	for path, want := range map[string]string{
		"a.go":     "package a\n// modified\n",
		"sub/b.go": "package b\n// modified\n",
		"new.txt":  "fresh untracked\n",
	} {
		body, err := os.ReadFile(filepath.Join(dest, path))
		if err != nil {
			t.Errorf("missing %s: %v", path, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s body = %q, want %q", path, body, want)
		}
	}
}

// TestCopyRescueFiles_SkipsDeletions: a "D " porcelain entry has no on-
// disk content to save — copyRescueFiles should skip it silently rather
// than failing the whole snapshot. The remaining files still copy.
func TestCopyRescueFiles_SkipsDeletions(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "kept.go"), "still here\n")
	// "deleted.go" intentionally not created — simulating a D entry.

	dest := filepath.Join(t.TempDir(), "snapshot")
	lines := []git.PorcelainStatusLine{
		{XY: " M", Path: "kept.go"},
		{XY: " D", Path: "deleted.go"},
	}
	n, err := copyRescueFiles(wt, dest, lines)
	if err != nil {
		t.Fatalf("copyRescueFiles: %v", err)
	}
	if n != 1 {
		t.Errorf("copied = %d, want 1 (deletion skipped)", n)
	}
	if _, err := os.Stat(filepath.Join(dest, "kept.go")); err != nil {
		t.Errorf("kept.go missing from snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "deleted.go")); err == nil {
		t.Errorf("deleted.go should not have been created in snapshot")
	}
}

// TestCopyRescueFiles_UntrackedDirectory: when an agent crashes mid-build
// it can leave a whole untracked subdirectory behind. Git porcelain
// reports such directories as one entry — copyRescueFiles walks the tree
// underneath instead of trying to copy the directory itself.
func TestCopyRescueFiles_UntrackedDirectory(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "build", "out", "x.bin"), "binary\n")
	writeFile(t, filepath.Join(wt, "build", "log.txt"), "log\n")

	dest := filepath.Join(t.TempDir(), "snapshot")
	lines := []git.PorcelainStatusLine{
		{XY: "??", Path: "build/"},
	}
	n, err := copyRescueFiles(wt, dest, lines)
	if err != nil {
		t.Fatalf("copyRescueFiles: %v", err)
	}
	if n != 2 {
		t.Errorf("copied = %d, want 2 (both nested files)", n)
	}
	got := snapshotFiles(t, dest)
	want := []string{"build/log.txt", "build/out/x.bin"}
	if !equalSortedStrings(got, want) {
		t.Errorf("snapshot files = %v, want %v", got, want)
	}
}

// writeFile is a tiny helper that creates parent dirs as needed.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// snapshotFiles returns every regular file under root in repo-relative form,
// sorted for deterministic comparison.
func snapshotFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		// Normalize to forward-slashes for cross-OS comparison clarity.
		out = append(out, strings.ReplaceAll(rel, string(filepath.Separator), "/"))
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func equalSortedStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
