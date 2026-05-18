package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestAttach_WritesAttachedPID_FromInsideWorktree pins the implicit-PID
// path: running `bosun attach session-1` from inside the worktree
// writes that worker process's PID into
// .bosun/state/session-1.attached-pid.
func TestAttach_WritesAttachedPID_FromInsideWorktree(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt := s.WorktreePath(1)
	out := s.BosunIn(wt, "attach", "session-1")
	s.AssertContains(out, "attached pid=")

	body, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "state", "session-1.attached-pid"))
	if err != nil {
		t.Fatalf("read attached-pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		t.Fatalf("attached-pid body = %q, want positive integer", body)
	}
}

// TestAttach_RefusesImplicitOutsideWorktree: without --pid, attach run
// from a directory other than the worktree refuses — the operator's
// shell PID is not a meaningful liveness referent for that session.
func TestAttach_RefusesImplicitOutsideWorktree(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Running from the main repo (not the worktree) should fail.
	out, err := s.BosunErr("attach", "session-1")
	if err == nil {
		t.Fatalf("attach from outside worktree should fail; output:\n%s", out)
	}
	s.AssertContains(out, "not inside the session-1 worktree")
	s.AssertContains(out, "--pid")

	// No file should have been written.
	if _, err := os.Stat(filepath.Join(s.repo, ".bosun", "state", "session-1.attached-pid")); err == nil {
		t.Fatal("attach failure left a stale attached-pid file")
	}
}

// TestAttach_ExplicitPID writes the supplied PID verbatim and works
// from any cwd (the operator is asserting trust by typing the number).
func TestAttach_ExplicitPID(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	out := s.Bosun("attach", "session-1", "--pid", "12345")
	s.AssertContains(out, "attached pid=12345")

	body, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "state", "session-1.attached-pid"))
	if err != nil {
		t.Fatalf("read attached-pid: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != "12345" {
		t.Errorf("attached-pid body = %q, want 12345", got)
	}
}

// TestAttach_Clear removes the file. Idempotent — a second --clear
// against an already-absent file is a no-op (the messaging differs but
// neither call errors).
func TestAttach_Clear(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	s.Bosun("attach", "session-1", "--pid", "12345")
	out := s.Bosun("attach", "session-1", "--clear")
	s.AssertContains(out, "attached-pid cleared")
	if _, err := os.Stat(filepath.Join(s.repo, ".bosun", "state", "session-1.attached-pid")); err == nil {
		t.Fatal("attached-pid file still present after --clear")
	}

	// Second clear is fine.
	if _, err := s.BosunErr("attach", "session-1", "--clear"); err != nil {
		t.Fatalf("second --clear should be no-op, got error: %v", err)
	}
}

// TestAttach_StatusReportsRunningWhenAttachedPIDIsAlive: the
// integration of cmd_attach + session.Derive — after attach, even
// without a `claude` in the proc tree, `bosun status --json` reports
// running=true with the registered PID.
func TestAttach_StatusReportsRunningWhenAttachedPIDIsAlive(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	// Use the test process's own PID — definitely alive.
	myPID := os.Getpid()
	s.Bosun("attach", "session-1", "--pid", strconv.Itoa(myPID))

	out := s.Bosun("status", "--json")
	if !strings.Contains(out, `"running": true`) {
		t.Errorf("status --json should report running=true after attach to live PID; got:\n%s", out)
	}
	want := `"running_pid": ` + strconv.Itoa(myPID)
	if !strings.Contains(out, want) {
		t.Errorf("status --json missing %q; got:\n%s", want, out)
	}
}

// TestAttach_ExternalLivenessGate_NeverCrashes: when
// liveness_gate=external, even a dirty worktree with no agent stays
// WORKING and the RUNNING column says "external". Mirrors the brief's
// done-criterion exactly.
func TestAttach_ExternalLivenessGate_NeverCrashes(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")
	s.Bosun("config", "set", "liveness_gate", "external")

	// Make the worktree dirty so the auto path would normally flip CRASHED.
	s.WriteFileIn(s.WorktreePath(1), "scratch.txt", "in-progress\n")

	out := s.Bosun("status")
	if strings.Contains(out, "CRASHED") {
		t.Errorf("status reports CRASHED under liveness_gate=external; got:\n%s", out)
	}
	if !strings.Contains(out, "external") {
		t.Errorf("status missing 'external' in RUNNING column; got:\n%s", out)
	}
}

// TestAttach_UnknownSessionErrors: typo against a non-existent session
// must not silently create an orphan attached-pid file under
// .bosun/state/.
func TestAttach_UnknownSessionErrors(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	out, err := s.BosunErr("attach", "session-9", "--pid", "1")
	if err == nil {
		t.Fatalf("attach against missing session should fail; output:\n%s", out)
	}
	s.AssertContains(out, "session-9 not found")

	stateDir := filepath.Join(s.repo, ".bosun", "state")
	entries, _ := os.ReadDir(stateDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "session-9.") {
			t.Errorf("attach error left orphan state file %s", e.Name())
		}
	}
}
