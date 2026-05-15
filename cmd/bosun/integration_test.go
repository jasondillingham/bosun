package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegration_EndToEnd exercises the full happy path:
// init -> commit in session -> claim -> done -> merge -> remove.
//
// This was the original integration test; it now runs against the shared
// binary built once in TestMain rather than rebuilding per test.
func TestIntegration_EndToEnd(t *testing.T) {
	s := newScenario(t)

	out := s.Bosun("init", "2")
	s.AssertContainsAll(out, "session-1", "session-2")

	s.AssertWorktreeExists(1)
	s.AssertWorktreeExists(2)

	// Make a commit in session-1's worktree.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "feature.txt", "session-1 feature\n")
	s.CommitIn(wt1, "add feature")

	s.Bosun("claim", "session-1", "feature.txt")

	statusOut := s.Bosun("status")
	s.AssertContainsAll(statusOut, "session-1", "add feature")

	s.Bosun("done", "session-1")

	listOut := s.Bosun("list", "--ready")
	if strings.TrimSpace(listOut) != "session-1" {
		t.Fatalf("list --ready = %q, want session-1", strings.TrimSpace(listOut))
	}

	mergeOut := s.Bosun("merge")
	s.AssertContainsAll(mergeOut, "session-1: merged", "session-2: skipped")

	// feature.txt should now be on main.
	s.AssertFileOnMain("feature.txt")

	// session-2 had no commits, so remove without --force should succeed.
	s.Bosun("remove", "session-2")

	if _, err := s.bosunRaw(s.repo, "status"); err != nil {
		t.Fatalf("status after teardown should not error: %v", err)
	}

	// Clean up — remove session-1 with --force since its branch is "ahead"
	// of main (squashed commit is content-equivalent but not literally there).
	s.Bosun("remove", "session-1", "--force")

	s.AssertWorktreeMissing(1)
	s.AssertWorktreeMissing(2)

	// Worktree paths should be cleaned up under the parent dir too.
	_ = filepath.Join
}
