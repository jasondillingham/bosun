package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScenario_DoctorCatchesIssue15Corruption is the end-to-end
// regression test for the macOS worktree admin corruption from issue
// #15 / docs/macos-worktree-corruption-forensics.md. We hand-craft
// the broken state in the scenario's .git/worktrees/ — phantom
// " 2"-suffix dirs and admin dirs missing HEAD/commondir/gitdir —
// then assert `bosun doctor` reports FAIL with a message that names
// the corruption shape.
func TestScenario_DoctorCatchesIssue15Corruption(t *testing.T) {
	s := newScenario(t)

	adminBase := filepath.Join(s.repo, ".git", "worktrees")
	// Real worktree admin dir, but stripped of HEAD/commondir/gitdir.
	brokenDir := filepath.Join(adminBase, "repo-bosun-1")
	if err := os.MkdirAll(filepath.Join(brokenDir, "refs"), 0o755); err != nil {
		t.Fatalf("mkdir broken admin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "index"), []byte(""), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	// Phantom dir with iCloud " 2" suffix.
	phantomDir := filepath.Join(adminBase, "repo-bosun-1 2")
	if err := os.MkdirAll(phantomDir, 0o755); err != nil {
		t.Fatalf("mkdir phantom: %v", err)
	}
	if err := os.WriteFile(filepath.Join(phantomDir, "index 3"), []byte(""), 0o644); err != nil {
		t.Fatalf("write phantom index: %v", err)
	}

	out, err := s.BosunErr("doctor")
	if err == nil {
		t.Fatalf("doctor should FAIL on corrupt worktree state:\n%s", out)
	}
	if !strings.Contains(out, "worktree-admin-integrity") {
		t.Errorf("doctor output should name the worktree-admin-integrity check: %s", out)
	}
	if !strings.Contains(out, "phantom") {
		t.Errorf("doctor output should mention phantom dirs: %s", out)
	}
	if !strings.Contains(out, "missing HEAD/commondir/gitdir") {
		t.Errorf("doctor output should name the missing files: %s", out)
	}
}

// TestScenario_DoctorFixReapsCorruption verifies that
// `bosun doctor --fix` removes the corrupt admin state cleanly and
// a subsequent doctor run returns clean. This is the recovery
// path operators land on after issue #15 strikes.
func TestScenario_DoctorFixReapsCorruption(t *testing.T) {
	s := newScenario(t)

	adminBase := filepath.Join(s.repo, ".git", "worktrees")
	brokenDir := filepath.Join(adminBase, "repo-bosun-1")
	phantomDir := filepath.Join(adminBase, "repo-bosun-1 2")
	for _, d := range []string{brokenDir, phantomDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// First, --fix should run. Expect non-zero exit because the
	// underlying issue was FAIL; but the fix should still apply.
	out, _ := s.BosunErr("doctor", "--fix")
	if !strings.Contains(out, "Auto-fixes:") {
		t.Errorf("doctor --fix output should mention auto-fixes: %s", out)
	}
	if _, err := os.Stat(brokenDir); !os.IsNotExist(err) {
		t.Errorf("broken admin dir should be removed by --fix: %v", err)
	}
	if _, err := os.Stat(phantomDir); !os.IsNotExist(err) {
		t.Errorf("phantom admin dir should be removed by --fix: %v", err)
	}

	// Second run with no remaining corruption should pass clean.
	out2 := s.Bosun("doctor")
	if !strings.Contains(out2, "worktree-admin-integrity") {
		t.Errorf("doctor should still list worktree-admin-integrity even when clean: %s", out2)
	}
	if !strings.Contains(out2, "All checks passed") {
		t.Errorf("post-fix doctor should be all-clean: %s", out2)
	}
}
