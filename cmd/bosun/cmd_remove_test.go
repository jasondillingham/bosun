package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyWorktreeBestEffort_SkipsDotGit pins the fallback used when
// `git status` can't tell us what's dirty (e.g. the v0.6.2 corrupted-
// gitdir crash). copyWorktreeBestEffort copies every regular file and
// symlink under the worktree except `.git` itself — without that skip,
// the snapshot would carry a stale gitdir pointer that makes the
// snapshot dir itself look like a broken worktree.
func TestCopyWorktreeBestEffort_SkipsDotGit(t *testing.T) {
	wt := t.TempDir()
	// .git pointer file (linked-worktree shape) — must be skipped.
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /tmp/bogus\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	// Regular files at the root and nested.
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "sub", "b.txt"), []byte("bravo\n"), 0o644); err != nil {
		t.Fatalf("write sub/b.txt: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "salvage")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	n, skipped, err := copyWorktreeBestEffort(wt, dest)
	if err != nil {
		t.Fatalf("copyWorktreeBestEffort: %v", err)
	}
	if n != 2 {
		t.Errorf("copied = %d, want 2 (a.txt + sub/b.txt)", n)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %+v, want empty for clean salvage", skipped)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		t.Errorf(".git was copied; should have been skipped")
	}
	for _, want := range []string{"a.txt", "sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("salvage missing %s: %v", want, err)
		}
	}
}

// TestCopyWorktreeBestEffort_RecordsSkipped pins the v0.7+ fix: when a
// file can't be salvaged (unreadable, symlink target missing, copy error)
// it now lands in the skipped slice with a reason so the rescue caller
// can surface it to the operator. Pre-fix, the function silently
// `return nil`'d on each error and the caller saw a copied-count that
// pretended the snapshot was complete.
func TestCopyWorktreeBestEffort_RecordsSkipped(t *testing.T) {
	wt := t.TempDir()
	// Two readable files (should copy cleanly).
	if err := os.WriteFile(filepath.Join(wt, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "also-ok.txt"), []byte("ok2"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One unreadable file (mode 0 + chmod after write) to force a copy
	// failure on the read side. Skipped on non-Unix where we can't easily
	// produce the failure.
	if os.Getuid() != 0 {
		unreadable := filepath.Join(wt, "unreadable.txt")
		if err := os.WriteFile(unreadable, []byte("nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unreadable, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	}

	dest := filepath.Join(t.TempDir(), "salvage")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	n, skipped, err := copyWorktreeBestEffort(wt, dest)
	if err != nil {
		t.Fatalf("copyWorktreeBestEffort: %v", err)
	}

	// The two readable files should always salvage.
	if n < 2 {
		t.Errorf("copied = %d, want at least 2", n)
	}
	// On non-root unix, the chmod 0 file should appear in skipped with
	// a copy-error reason.
	if os.Getuid() != 0 {
		if len(skipped) == 0 {
			t.Fatalf("expected unreadable.txt in skipped list; got empty")
		}
		var found bool
		for _, s := range skipped {
			if strings.Contains(s.rel, "unreadable") && strings.Contains(s.reason, "copy") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("skipped list missing unreadable.txt with copy reason: %+v", skipped)
		}
	}
}

// TestLiveAgentRemoveMessage pins the v0.6 liveness-gate refusal message:
// label, pid, recovery hint, and --ignore-running escape hatch must all
// appear so the operator never has to grep our docs to recover. Brittle
// on purpose — operator muscle memory anchors on this exact shape.
func TestLiveAgentRemoveMessage(t *testing.T) {
	got := liveAgentRemoveMessage("session-2", 12345)
	for _, want := range []string{
		"session-2",
		"pid 12345",
		"live agent",
		"uncommitted changes",
		"refusing remove",
		"let the agent finish",
		"--ignore-running",
		"discards uncommitted work",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("liveAgentRemoveMessage missing %q\n--- got ---\n%s", want, got)
		}
	}
}
