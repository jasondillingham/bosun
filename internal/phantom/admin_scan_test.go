package phantom

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// makeAdminDir scaffolds one admin dir under <repoRoot>/.git/worktrees/<name>.
// When complete is true, writes the three canonical metadata files
// (HEAD, commondir, gitdir) plus a couple of subdirs git typically
// creates. When complete is false, omits one or more of them so the
// dir looks like the issue #15 corruption shape.
func makeAdminDir(t *testing.T, repoRoot, name string, complete bool) string {
	t.Helper()
	dir := filepath.Join(repoRoot, ".git", "worktrees", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if complete {
		for _, f := range []string{"HEAD", "commondir", "gitdir"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("placeholder"), 0o644); err != nil {
				t.Fatalf("write %s: %v", f, err)
			}
		}
	}
	// Always write index + refs/ subdir so the dir looks plausible
	// — the issue #15 corruption keeps these around even when the
	// top-level metadata files are gone.
	if err := os.WriteFile(filepath.Join(dir, "index"), []byte{}, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	return dir
}

func TestScanWorktreeAdmin_CleanRepoReturnsEmpty(t *testing.T) {
	repo := t.TempDir()
	// .git/worktrees/ doesn't exist yet — fresh repo before init.
	res, err := ScanWorktreeAdmin(repo)
	if err != nil {
		t.Fatalf("ScanWorktreeAdmin: %v", err)
	}
	if len(res.PhantomDirs) != 0 || len(res.BrokenDirs) != 0 {
		t.Errorf("expected empty result on fresh repo, got %+v", res)
	}
}

func TestScanWorktreeAdmin_HealthyAdminDirsAreNotFlagged(t *testing.T) {
	repo := t.TempDir()
	makeAdminDir(t, repo, "repo-bosun-1", true)
	makeAdminDir(t, repo, "repo-bosun-2", true)

	res, err := ScanWorktreeAdmin(repo)
	if err != nil {
		t.Fatalf("ScanWorktreeAdmin: %v", err)
	}
	if len(res.PhantomDirs) != 0 {
		t.Errorf("healthy dirs flagged as phantom: %v", res.PhantomDirs)
	}
	if len(res.BrokenDirs) != 0 {
		t.Errorf("healthy dirs flagged as broken: %v", res.BrokenDirs)
	}
}

func TestScanWorktreeAdmin_SpotlightPhantomShape(t *testing.T) {
	repo := t.TempDir()
	// The shape from issue #15: real admin dirs + iCloud " 2"-suffix
	// phantom dirs containing duplicated index files.
	makeAdminDir(t, repo, "repo-bosun-1", true)
	makeAdminDir(t, repo, "repo-bosun-1 2", false) // phantom, missing metadata
	makeAdminDir(t, repo, "repo-bosun-2", true)
	makeAdminDir(t, repo, "repo-bosun-3 2", false) // phantom

	res, err := ScanWorktreeAdmin(repo)
	if err != nil {
		t.Fatalf("ScanWorktreeAdmin: %v", err)
	}
	sort.Strings(res.PhantomDirs)
	if len(res.PhantomDirs) != 2 {
		t.Fatalf("expected 2 phantom dirs, got %d: %v", len(res.PhantomDirs), res.PhantomDirs)
	}
	for _, p := range res.PhantomDirs {
		if !strings.HasSuffix(p, " 2") {
			t.Errorf("expected phantom path to end in ' 2', got %q", p)
		}
	}
}

func TestScanWorktreeAdmin_ICloudParenPhantomShape(t *testing.T) {
	repo := t.TempDir()
	makeAdminDir(t, repo, "repo-bosun-1", true)
	makeAdminDir(t, repo, "repo-bosun-1 (1)", false) // iCloud-style phantom

	res, err := ScanWorktreeAdmin(repo)
	if err != nil {
		t.Fatalf("ScanWorktreeAdmin: %v", err)
	}
	if len(res.PhantomDirs) != 1 {
		t.Fatalf("expected 1 phantom dir, got %d: %v", len(res.PhantomDirs), res.PhantomDirs)
	}
	if !strings.HasSuffix(res.PhantomDirs[0], "(1)") {
		t.Errorf("expected phantom path to end in '(1)', got %q", res.PhantomDirs[0])
	}
}

func TestScanWorktreeAdmin_BrokenAdminDir(t *testing.T) {
	repo := t.TempDir()
	// Real-named dir missing HEAD/commondir/gitdir — the issue #15
	// corruption shape that hits bosun's view via `git worktree list`.
	makeAdminDir(t, repo, "repo-bosun-1", true)
	makeAdminDir(t, repo, "repo-bosun-2", false) // broken

	res, err := ScanWorktreeAdmin(repo)
	if err != nil {
		t.Fatalf("ScanWorktreeAdmin: %v", err)
	}
	if len(res.BrokenDirs) != 1 {
		t.Fatalf("expected 1 broken dir, got %d: %v", len(res.BrokenDirs), res.BrokenDirs)
	}
	if !strings.HasSuffix(res.BrokenDirs[0], "repo-bosun-2") {
		t.Errorf("expected broken path to end in 'repo-bosun-2', got %q", res.BrokenDirs[0])
	}
}

func TestScanWorktreeAdmin_FullIssue15Repro(t *testing.T) {
	// The exact shape captured in
	// docs/macos-worktree-corruption-forensics.md: four "real" admin
	// dirs all missing their HEAD/commondir/gitdir top-level files,
	// plus phantom " 2" suffixed dirs for three of them, plus a
	// zombie "bosun-bosun-5" from a prior round.
	repo := t.TempDir()
	for _, n := range []string{"repo-bosun-1", "repo-bosun-2", "repo-bosun-3", "repo-bosun-4"} {
		makeAdminDir(t, repo, n, false) // broken
	}
	for _, n := range []string{"repo-bosun-1 2", "repo-bosun-2 2", "repo-bosun-3 2"} {
		makeAdminDir(t, repo, n, false) // phantom
	}
	makeAdminDir(t, repo, "repo-bosun-5", false) // zombie (also broken)

	res, err := ScanWorktreeAdmin(repo)
	if err != nil {
		t.Fatalf("ScanWorktreeAdmin: %v", err)
	}
	if len(res.PhantomDirs) != 3 {
		t.Errorf("expected 3 phantom dirs, got %d: %v", len(res.PhantomDirs), res.PhantomDirs)
	}
	if len(res.BrokenDirs) != 5 {
		t.Errorf("expected 5 broken dirs (4 real + 1 zombie), got %d: %v", len(res.BrokenDirs), res.BrokenDirs)
	}
}

func TestPhantomAdminBaseName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"repo-bosun-1", "repo-bosun-1"},       // not phantom
		{"repo-bosun-1 2", "repo-bosun-1"},     // spotlight shape
		{"repo-bosun-1 (1)", "repo-bosun-1"},   // icloud shape
		{"a b c 42", "a b c"},                  // embedded spaces ok
		{"plain", "plain"},                     // no suffix
		{"trailing space ", "trailing space "}, // single trailing space, no digits — not phantom
	}
	for _, c := range cases {
		got := PhantomAdminBaseName(c.in)
		if got != c.want {
			t.Errorf("PhantomAdminBaseName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
