package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScenario_ListIgnoresPhantomStateFiles covers v0.7 ask #4: macOS
// Finder/Spotlight/iCloud occasionally duplicate state marker files
// inside `.bosun/state/` (`session-1 2.done`, etc.). Those phantoms
// must never surface as additional sessions in `bosun list`. We
// init a single session, drop a fake duplicate alongside its real
// state, and assert list only reports the real session.
func TestScenario_ListIgnoresPhantomStateFiles(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Mark session-1 done so a `.done` marker exists, then drop a
	// Finder-style duplicate of it inside the state dir.
	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "x.txt", "work\n")
	s.GitIn(wt, "add", ".")
	s.GitIn(wt, "commit", "-q", "-m", "work")
	s.Bosun("done", "session-1", "-m", "real")

	stateDir := filepath.Join(s.repo, ".bosun", "state")
	if err := os.WriteFile(filepath.Join(stateDir, "session-1 2.done"), []byte("phantom\n"), 0o644); err != nil {
		t.Fatalf("write phantom: %v", err)
	}

	out := s.Bosun("list")
	if !strings.Contains(out, "session-1") {
		t.Fatalf("list missing session-1:\n%s", out)
	}
	// The phantom must not surface as its own line.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, " ") || strings.Contains(line, "(1)") {
			t.Errorf("list surfaced phantom session: %q\nfull output:\n%s", line, out)
		}
	}
}

// TestScenario_InitDropsSpotlightMarker covers the prevention half of
// ask #4: `bosun init` writes `.bosun/.metadata_never_index` so macOS
// stops indexing the directory and the duplicate files don't get
// created in the first place. Cross-platform: the file is just an
// empty marker on every OS; only macOS treats it specially.
func TestScenario_InitDropsSpotlightMarker(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	marker := filepath.Join(s.repo, ".bosun", ".metadata_never_index")
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("marker size = %d, want 0", info.Size())
	}
}

// TestScenario_RescueRefusesCorruptedGitdir + RemoveForceSalvages covers
// v0.7 ask #5: the v0.6.2 trial reproduced an agent crash that reduced
// `<repo>/.git/worktrees/<name>/` to just `index`. In that state every
// `git -C <worktree>` call fails. `bosun rescue` must refuse with an
// actionable recovery hint instead of failing mid-snapshot, and
// `bosun remove --force` must salvage anything still on disk before
// destroying the worktree.
func TestScenario_RescueRefusesCorruptedGitdir(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "in-flight.txt", "agent was here\n")

	// Simulate the v0.6.2 crash footprint: gitdir admin dir lost its HEAD.
	// Git names the admin dir after the worktree's basename, not the
	// bosun label — so derive the path from the worktree dir.
	headPath := filepath.Join(s.repo, ".git", "worktrees", filepath.Base(wt), "HEAD")
	if err := os.Remove(headPath); err != nil {
		t.Fatalf("remove HEAD: %v", err)
	}

	out, err := s.BosunErr("rescue", "1")
	if err == nil {
		t.Fatalf("rescue on corrupted gitdir should fail, got success:\n%s", out)
	}
	for _, want := range []string{"corrupted", "bosun remove", "--force"} {
		if !strings.Contains(out, want) {
			t.Errorf("rescue refusal missing %q:\n%s", want, out)
		}
	}
}

// TestScenario_RemoveForceSalvagesCorruptedWorktree pins the salvage
// half of ask #5: with a corrupted gitdir, `bosun remove --force`
// copies the worktree's files into `.bosun/rescues/session-N-<ts>/`
// before destroying the worktree. The branch + worktree dir must
// also be gone afterward.
func TestScenario_RemoveForceSalvagesCorruptedWorktree(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "in-flight.txt", "agent was here\n")
	s.WriteFileIn(wt, "nested/data.txt", "more in-flight work\n")

	headPath := filepath.Join(s.repo, ".git", "worktrees", filepath.Base(wt), "HEAD")
	if err := os.Remove(headPath); err != nil {
		t.Fatalf("remove HEAD: %v", err)
	}

	out := s.Bosun("remove", "1", "--force")
	if !strings.Contains(out, "salvaged") {
		t.Errorf("remove --force output missing 'salvaged' confirmation:\n%s", out)
	}

	rescues := filepath.Join(s.repo, ".bosun", "rescues")
	entries, err := os.ReadDir(rescues)
	if err != nil {
		t.Fatalf("read rescues dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("rescue dir entries = %d, want 1: %+v", len(entries), entries)
	}
	if !strings.HasPrefix(entries[0].Name(), "session-1-") {
		t.Errorf("rescue dir name %q should start with session-1-", entries[0].Name())
	}
	snap := filepath.Join(rescues, entries[0].Name())
	for _, rel := range []string{"in-flight.txt", "nested/data.txt"} {
		if _, err := os.Stat(filepath.Join(snap, rel)); err != nil {
			t.Errorf("salvage missing %s: %v", rel, err)
		}
	}
	// `.git` pointer (or whatever stub remains) must not have been copied.
	if _, err := os.Stat(filepath.Join(snap, ".git")); err == nil {
		t.Errorf(".git pointer should not have been copied into the salvage")
	}

	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
}
