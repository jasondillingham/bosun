package proc

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type fakeLister struct {
	procs []ProcInfo
	err   error
}

func (f fakeLister) List() ([]ProcInfo, error) { return f.procs, f.err }

func TestRunningWith_MatchesByNameAndCWD(t *testing.T) {
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeLister{procs: []ProcInfo{
		{PID: 1, Name: "bash", CWD: dir},          // right cwd, wrong name
		{PID: 2, Name: "claude", CWD: "/nowhere"}, // right name, wrong cwd
		{PID: 42, Name: "claude", CWD: dir},       // match
	}}
	pid, ok, err := RunningWith(fake, IsAgent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected match")
	}
	if pid != 42 {
		t.Fatalf("pid=%d, want 42", pid)
	}
}

func TestRunningWith_NoMatchReturnsFalse(t *testing.T) {
	fake := fakeLister{procs: []ProcInfo{
		{PID: 1, Name: "bash", CWD: "/tmp"},
	}}
	pid, ok, err := RunningWith(fake, IsAgent, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("expected no match, got pid=%d", pid)
	}
}

func TestRunningWith_ListerErrorPropagates(t *testing.T) {
	boom := errors.New("permission denied")
	pid, ok, err := RunningWith(fakeLister{err: boom}, IsAgent, "/tmp")
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
	if ok {
		t.Fatal("ok should be false on lister error")
	}
	if pid != 0 {
		t.Fatalf("pid=%d, want 0", pid)
	}
}

func TestRunningWith_PerProcessPermissionErrorsAreSilent(t *testing.T) {
	// The real GopsutilLister swallows per-process errors silently — a
	// process the current user can't introspect is simply omitted from
	// the list. We simulate that by handing RunningWith a list that
	// already excludes the unreadable process: detection should report
	// the worktree as not-running rather than surfacing an error.
	dir := t.TempDir()
	fake := fakeLister{procs: []ProcInfo{
		// All visible procs are unrelated. (The "unreadable claude" in
		// the worktree, if it existed, would have been dropped by the
		// lister before reaching us.)
		{PID: 1, Name: "bash", CWD: dir},
		{PID: 2, Name: "git", CWD: dir},
	}}
	_, ok, err := RunningWith(fake, IsAgent, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when no agent process is visible")
	}
}

func TestRunningWith_DetectsRealSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep not portable to windows")
	}
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Permissive matcher so "sleep" counts as an agent for this test —
	// we're exercising the real lister + path matching, not the name
	// filter (covered by TestIsAgent).
	anyName := func(string) bool { return true }

	var (
		pid int
		ok  bool
		err error
	)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pid, ok, err = RunningWith(GopsutilLister{}, anyName, dir)
		if ok || err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("RunningWith: %v", err)
	}
	if !ok {
		t.Fatal("expected to detect the sleep subprocess")
	}
	if pid != cmd.Process.Pid {
		t.Errorf("pid=%d, want %d", pid, cmd.Process.Pid)
	}

	// Kill it and verify the detection flips back to ok=false within a
	// reasonable window. (The OS may take a beat to reap and gopsutil
	// may take a beat to refresh.)
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, ok, _ = RunningWith(GopsutilLister{}, anyName, dir)
		if !ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ok {
		t.Fatal("expected ok=false after killing the subprocess")
	}
}

func TestIsAgent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"claude", true},
		{"Claude", true},
		{"claude.exe", true},
		{"claude-code", true},
		{"code-cli", true},
		{"/usr/local/bin/claude", true},
		{"bash", false},
		{"git", false},
		{"node", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsAgent(tc.in); got != tc.want {
			t.Errorf("IsAgent(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
