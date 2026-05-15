package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestScale_TenSessions runs the workflow at N=10 with disjoint per-session
// edits, asserting that a full init → claim → done → merge cycle completes
// cleanly and within a sane wall-clock budget.
//
// The point isn't to benchmark — it's to surface anything that breaks down
// at scale (lock contention, claim/overlap O(N²) blowup, merge ordering
// surprises) that the N=2 happy-path test can't catch.
func TestScale_TenSessions(t *testing.T) {
	const N = 10
	s := newScenario(t)

	// Pre-seed the repo with per-session target files. Each session edits
	// only "its own" file, so disjoint by construction. We're testing the
	// orchestration, not git's conflict resolution.
	for i := 1; i <= N; i++ {
		s.WriteFile(fmt.Sprintf("internal/mod%d/file.go", i), fmt.Sprintf("package mod%d\n", i))
	}
	s.GitIn(s.repo, "add", ".")
	s.GitIn(s.repo, "commit", "-q", "-m", "seed N modules")

	start := time.Now()

	initOut := s.Bosun("init", fmt.Sprintf("%d", N))
	if strings.Count(initOut, "→") != N {
		t.Fatalf("init output didn't create %d sessions:\n%s", N, initOut)
	}

	// Each session edits its own module file and claims its path. Half the
	// sessions also claim a shared helper file to exercise overlap detection
	// at scale.
	for i := 1; i <= N; i++ {
		wt := s.WorktreePath(i)
		s.WriteFileIn(wt, fmt.Sprintf("internal/mod%d/file.go", i), fmt.Sprintf("package mod%d\n// edited by session-%d\n", i, i))
		s.CommitIn(wt, fmt.Sprintf("mod%d: edit", i))

		s.Bosun("claim", fmt.Sprintf("session-%d", i), fmt.Sprintf("internal/mod%d/file.go", i))
		if i%2 == 0 {
			s.Bosun("claim", fmt.Sprintf("session-%d", i), "internal/shared/helper.go")
		}
	}

	// Overlaps: every even session claimed internal/shared/helper.go → that's
	// 5 sessions on one path.
	overlapOut := s.Bosun("status", "--with-overlaps")
	s.AssertContainsAll(overlapOut, "Overlapping claims", "internal/shared/helper.go")

	// Mark every session DONE.
	for i := 1; i <= N; i++ {
		s.Bosun("done", fmt.Sprintf("session-%d", i))
	}

	// Verify all 10 are DONE via the JSON schema.
	p := s.StatusJSON()
	if len(p.Sessions) != N {
		t.Fatalf("status shows %d sessions, want %d", len(p.Sessions), N)
	}
	for _, sess := range p.Sessions {
		if sess.State != "DONE" {
			t.Errorf("%s state = %s, want DONE", sess.Name, sess.State)
		}
	}

	mergeOut := s.Bosun("merge")
	for i := 1; i <= N; i++ {
		want := fmt.Sprintf("session-%d: merged", i)
		if !strings.Contains(mergeOut, want) {
			t.Errorf("merge output missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("full merge output:\n%s", mergeOut)
	}

	// Every per-session file should be on main with the session's edit.
	for i := 1; i <= N; i++ {
		s.AssertFileOnMain(fmt.Sprintf("internal/mod%d/file.go", i))
	}

	elapsed := time.Since(start)
	t.Logf("N=%d sessions: init+claim+done+merge cycle in %v (≈%.0fms/session)",
		N, elapsed, float64(elapsed.Milliseconds())/float64(N))

	// Soft budget: we expect well under 30s for N=10 on a modern machine.
	// Treat as a regression alarm, not a hard contract.
	if elapsed > 30*time.Second {
		t.Errorf("N=%d cycle took %v — possible scale regression", N, elapsed)
	}
}
