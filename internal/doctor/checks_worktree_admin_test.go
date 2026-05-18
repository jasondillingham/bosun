package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkHealthyAdmin(t *testing.T, repo, name string) string {
	t.Helper()
	dir := filepath.Join(repo, ".git", "worktrees", name)
	for _, f := range []string{"HEAD", "commondir", "gitdir"} {
		writeFile(t, filepath.Join(dir, f), "x")
	}
	return dir
}

func mkBrokenAdmin(t *testing.T, repo, name string) string {
	t.Helper()
	dir := filepath.Join(repo, ".git", "worktrees", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	// Write only `index` and a `refs/` subdir — the partial-state
	// shape from issue #15.
	writeFile(t, filepath.Join(dir, "index"), "")
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	return dir
}

func TestCheckWorktreeAdminCorruption_NoAdminDirsIsClean(t *testing.T) {
	repo := t.TempDir()
	res := CheckWorktreeAdminCorruption(context.Background(), repo)
	if res.Status != Pass {
		t.Errorf("expected Pass on empty repo, got %v: %s", res.Status, res.Message)
	}
}

func TestCheckWorktreeAdminCorruption_HealthyDirsIsClean(t *testing.T) {
	repo := t.TempDir()
	mkHealthyAdmin(t, repo, "repo-bosun-1")
	mkHealthyAdmin(t, repo, "repo-bosun-2")

	res := CheckWorktreeAdminCorruption(context.Background(), repo)
	if res.Status != Pass {
		t.Errorf("expected Pass on healthy admin dirs, got %v: %s", res.Status, res.Message)
	}
}

func TestCheckWorktreeAdminCorruption_PhantomDirIsFail(t *testing.T) {
	repo := t.TempDir()
	mkHealthyAdmin(t, repo, "repo-bosun-1")
	mkBrokenAdmin(t, repo, "repo-bosun-1 2") // phantom

	res := CheckWorktreeAdminCorruption(context.Background(), repo)
	if res.Status != Fail {
		t.Errorf("expected Fail on phantom dir, got %v: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "phantom") {
		t.Errorf("message should mention phantom: %q", res.Message)
	}
	if res.FixFn == nil {
		t.Error("expected a FixFn to be present for autofix")
	}
}

func TestCheckWorktreeAdminCorruption_BrokenDirIsFail(t *testing.T) {
	repo := t.TempDir()
	mkBrokenAdmin(t, repo, "repo-bosun-1") // missing HEAD/commondir/gitdir

	res := CheckWorktreeAdminCorruption(context.Background(), repo)
	if res.Status != Fail {
		t.Errorf("expected Fail on broken admin dir, got %v: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "missing HEAD/commondir/gitdir") {
		t.Errorf("message should name the missing files: %q", res.Message)
	}
}

func TestCheckWorktreeAdminCorruption_FixReapsPhantoms(t *testing.T) {
	// Simulates the issue #15 corruption shape end-to-end: heal-after-fix.
	repo := t.TempDir()
	mkHealthyAdmin(t, repo, "repo-bosun-1")
	mkBrokenAdmin(t, repo, "repo-bosun-1 2")            // phantom
	brokenDir := mkBrokenAdmin(t, repo, "repo-bosun-2") // broken real

	res := CheckWorktreeAdminCorruption(context.Background(), repo)
	if res.FixFn == nil {
		t.Fatal("expected FixFn")
	}
	if err := res.FixFn(repo); err != nil {
		t.Fatalf("FixFn: %v", err)
	}

	// Both the phantom dir and the broken real dir should be gone.
	if _, err := os.Stat(filepath.Join(repo, ".git", "worktrees", "repo-bosun-1 2")); !os.IsNotExist(err) {
		t.Errorf("phantom dir survived fix: %v", err)
	}
	if _, err := os.Stat(brokenDir); !os.IsNotExist(err) {
		t.Errorf("broken admin dir survived fix: %v", err)
	}
	// The healthy admin dir must survive.
	if _, err := os.Stat(filepath.Join(repo, ".git", "worktrees", "repo-bosun-1")); err != nil {
		t.Errorf("healthy admin dir reaped by fix: %v", err)
	}

	// Rescan should now pass.
	res2 := CheckWorktreeAdminCorruption(context.Background(), repo)
	if res2.Status != Pass {
		t.Errorf("post-fix expected Pass, got %v: %s", res2.Status, res2.Message)
	}
}

func TestIsICloudManagedPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir, skipping iCloud path check")
	}
	cases := []struct {
		name    string
		path    string
		wantHit bool
	}{
		{"docs path", filepath.Join(home, "Documents", "project"), true},
		{"desktop path", filepath.Join(home, "Desktop", "project"), true},
		{"icloud drive root", filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs", "project"), true},
		{"tmp path", "/tmp/bosun-work", false},
		{"home root", home, false}, // home itself isn't iCloud-managed
		{"dev path", filepath.Join(home, "dev", "project"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := IsICloudManagedPath(c.path)
			if got != c.wantHit {
				t.Errorf("IsICloudManagedPath(%q) = %v (%q), want %v", c.path, got, reason, c.wantHit)
			}
			if c.wantHit && reason == "" {
				t.Error("hit but no reason returned")
			}
			if !c.wantHit && reason != "" {
				t.Errorf("miss but reason returned: %q", reason)
			}
		})
	}
}
