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

func TestRunningWith_MatchesViaCmdlineWhenNameIsVersion(t *testing.T) {
	// Regression for the v0.7+ kickoff observation: macOS Claude Code
	// installs at ~/.local/share/claude/versions/<X.Y.Z>/, so p.Name()
	// returns the version number ("2.1.143") rather than "claude". The
	// cmdline's first token is still "claude" and recovers the match.
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeLister{procs: []ProcInfo{
		{PID: 1, Name: "2.1.143", CWD: dir, Cmdline: "claude Read BOSUN_BRIEF.md..."},
	}}
	pid, ok, err := RunningWith(fake, IsAgent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected match via cmdline first token even when Name is a version number")
	}
	if pid != 1 {
		t.Fatalf("pid=%d, want 1", pid)
	}
}

func TestRunningWith_CmdlineFallbackIgnoredWhenNoMatch(t *testing.T) {
	// Cmdline starts with `bash` (not an agent); name is `2.1.143`
	// (also not an agent). Don't false-positive on the version-number
	// pattern alone.
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeLister{procs: []ProcInfo{
		{PID: 1, Name: "2.1.143", CWD: dir, Cmdline: "bash -c some-other-tool"},
	}}
	_, ok, err := RunningWith(fake, IsAgent, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no match when neither Name nor cmdline first token is an agent")
	}
}

func TestFirstCmdlineToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"claude", "claude"},
		{"claude Read BOSUN_BRIEF.md", "claude"},
		{"/usr/local/bin/claude --foo", "claude"},
		{"  leading whitespace", "leading"},
	}
	for _, tc := range cases {
		if got := firstCmdlineToken(tc.in); got != tc.want {
			t.Errorf("firstCmdlineToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
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

// TestIsAgentForCommand pins Phase 1 of the agent-command design: the
// predicate extends the default allowlist with the basename derived
// from a wrapper-script command, so non-Claude agents still register
// in `bosun status`. An empty command degrades to the IsAgent default.
func TestIsAgentForCommand(t *testing.T) {
	cases := []struct {
		name      string
		command   string
		probeName string
		want      bool
	}{
		// Default behavior preserved.
		{"empty command, claude still detected", "", "claude", true},
		{"empty command, bash still rejected", "", "bash", false},
		{"claude command equivalent to default", "claude", "claude", true},
		{"claude with args still default", "claude --model opus-4", "claude", true},

		// Wrapper scripts (bare basename and path forms).
		{"wrapper bare name registers", "ollama-claude.sh", "ollama-claude.sh", true},
		{"wrapper path registers via basename", "./scripts/ollama-claude.sh", "ollama-claude", true},
		{"wrapper path registers via full path probe", "./scripts/ollama-claude.sh", "/work/scripts/ollama-claude", true},
		{"wrapper command with args picks first token", "ollama-claude.sh --model llama3", "ollama-claude.sh", true},

		// Wrapper still admits the default basenames too — operators may
		// have multiple sessions, some wrapped, some not.
		{"wrapper command, claude still admitted", "ollama-claude.sh", "claude", true},
		{"wrapper command, code-cli still admitted", "ollama-claude.sh", "code-cli", true},

		// Unrelated processes stay rejected even with a wrapper command.
		{"wrapper command, bash rejected", "ollama-claude.sh", "bash", false},
		{"wrapper command, git rejected", "ollama-claude.sh", "git", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pred := IsAgentForCommand(tc.command)
			if got := pred(tc.probeName); got != tc.want {
				t.Errorf("IsAgentForCommand(%q)(%q) = %v, want %v", tc.command, tc.probeName, got, tc.want)
			}
		})
	}
}

// TestCommandBasename pins the basename-extraction rules so a future
// regression on the predicate's hot path surfaces here, not in a
// confusing scenario test.
func TestCommandBasename(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude", "claude"},
		{"./scripts/wrap.sh", "wrap"},
		{"/usr/local/bin/claude", "claude"},
		{"claude --model opus", "claude"},
		{"  claude  ", "claude"},
		{"Claude", "claude"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := commandBasename(tc.in); got != tc.want {
			t.Errorf("commandBasename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
