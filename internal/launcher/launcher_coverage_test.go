package launcher

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBuildEnvPairs covers the env-var formatter used by the tmux
// strategy. Pure function so the assertions are precise.
func TestBuildEnvPairs(t *testing.T) {
	if got := buildEnvPairs(nil); got != nil {
		t.Errorf("buildEnvPairs(nil) = %v, want nil", got)
	}
	got := buildEnvPairs(map[string]string{"B": "2", "A": "1"})
	want := []string{"A=1", "B=2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestHasTmux_NoEnv asserts the TMUX-env gate. Even with `tmux` on
// PATH, hasTmux must return false when TMUX is unset — bosun only
// uses tmux when the operator is already in a tmux session.
func TestHasTmux_NoEnv(t *testing.T) {
	t.Setenv("TMUX", "")
	if hasTmux() {
		t.Error("hasTmux() = true with TMUX unset; expected false")
	}
}

// TestHasTmux_EnvSetButNoBinary asserts that TMUX set without tmux
// on PATH still returns false (e.g., env was inherited from an
// outer tmux but the binary is gone).
func TestHasTmux_EnvSetButNoBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on Windows")
	}
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("PATH", t.TempDir())
	if hasTmux() {
		t.Error("hasTmux() = true with TMUX set but no tmux binary on PATH; expected false")
	}
}

// TestHasTmux_BothPresent rounds out the truth table.
func TestHasTmux_BothPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "tmux")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !hasTmux() {
		t.Error("hasTmux() = false with TMUX set and tmux on PATH; expected true")
	}
}

// TestHasITerm2 covers the macOS-only iTerm2 detection. On non-darwin
// the function returns false unconditionally.
func TestHasITerm2(t *testing.T) {
	got := hasITerm2()
	if runtime.GOOS != "darwin" && got {
		t.Errorf("hasITerm2() = true on %s; expected false", runtime.GOOS)
	}
}

// TestHasWindowsTerminal covers the wt.exe detection. On non-windows
// it returns ("", false) without touching PATH.
func TestHasWindowsTerminal(t *testing.T) {
	bin, ok := hasWindowsTerminal()
	if runtime.GOOS != "windows" {
		if ok || bin != "" {
			t.Errorf("hasWindowsTerminal() = (%q, %v) on %s; expected (\"\", false)", bin, ok, runtime.GOOS)
		}
	}
}

// TestHasTerminal_GhosttyPresent covers the cross-OS short-circuit
// where Ghostty on PATH counts as a terminal regardless of the
// OS-specific fallbacks.
func TestHasTerminal_GhosttyPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang")
	}
	stubGhosttyOnPath(t)
	if !hasTerminal() {
		t.Error("hasTerminal() = false with ghostty on PATH; expected true")
	}
}

// TestHasTerminal_None tests the negative case on Linux — clean PATH
// + no Ghostty + no linuxTerminals. macOS always has osascript built
// in, so we skip there. Windows hardcodes true via cmd /c.
func TestHasTerminal_None(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux can have all terminals absent")
	}
	t.Setenv("PATH", t.TempDir())
	if hasTerminal() {
		t.Error("hasTerminal() = true with empty PATH on Linux; expected false")
	}
}

// TestPickAuto_PrintFallback verifies the cascade reaches print when
// neither tmux nor a terminal is available. Linux-only because the
// other OSes have built-in terminals that can't be hidden.
func TestPickAuto_PrintFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux can have no built-in terminal")
	}
	t.Setenv("TMUX", "")
	t.Setenv("PATH", t.TempDir())
	if got := pickAuto(); got != StrategyPrint {
		t.Errorf("pickAuto() = %v, want %v", got, StrategyPrint)
	}
}

// TestPickAuto_PrefersTmux verifies tmux wins when both tmux and a
// terminal are available — operators already inside tmux expect new
// windows to land as tmux panes, not in a separate GUI app.
func TestPickAuto_PrefersTmux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang")
	}
	dir := t.TempDir()
	for _, name := range []string{"tmux", "ghostty"} {
		stub := filepath.Join(dir, name)
		if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := pickAuto(); got != StrategyTmux {
		t.Errorf("pickAuto() = %v with TMUX set; want %v", got, StrategyTmux)
	}
}

// TestPickAuto_TerminalWhenNoTmux covers the second tier of the
// cascade.
func TestPickAuto_TerminalWhenNoTmux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang")
	}
	stubGhosttyOnPath(t)
	t.Setenv("TMUX", "")
	if got := pickAuto(); got != StrategyTerminal {
		t.Errorf("pickAuto() = %v with no TMUX but ghostty present; want %v", got, StrategyTerminal)
	}
}

// TestLaunchTmux_BuildsArgv exercises the tmux dispatch path via the
// execRunFn seam. No real tmux invocation; we just capture argv and
// assert the new-window form and env passing.
func TestLaunchTmux_BuildsArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "tmux")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prevRun := execRunFn
	t.Cleanup(func() { execRunFn = prevRun })

	var captured []string
	execRunFn = func(cmd *exec.Cmd) error {
		captured = append([]string(nil), cmd.Args...)
		return nil
	}

	got, err := Launch(Options{
		Strategy:      StrategyAuto, // resolves to tmux via TMUX env + binary
		WorktreePath:  "/wt/session-1",
		SessionName:   "session-1",
		Command:       "claude",
		InitialPrompt: "hi",
		Env:           map[string]string{"GOCACHE": "/tmp/x"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got != StrategyTmux {
		t.Errorf("Launch resolved to %v, want %v", got, StrategyTmux)
	}

	joined := strings.Join(captured, " ")
	for _, want := range []string{"new-window", "-c", "/wt/session-1", "-n", "session-1", "GOCACHE=/tmp/x", "claude", "hi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tmux argv missing %q; got %v", want, captured)
		}
	}
}

// TestLaunchTerminalLinux_DispatchesViaPATH stubs a fake
// gnome-terminal binary on PATH and asserts that the Linux dispatch
// reaches the spawnFn seam with the expected argv. macOS skips this
// because launchTerminal prefers Ghostty when present and falls
// through to launchTerminalDarwin otherwise.
func TestLaunchTerminalLinux_DispatchesViaPATH(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only dispatch path")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "gnome-terminal")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Reset PATH so the only emulator visible is our stub.
	t.Setenv("PATH", dir)

	prev := spawnFn
	t.Cleanup(func() { spawnFn = prev })
	resetMacOSStaggerForTest()

	var argv []string
	spawnFn = func(cmd *exec.Cmd, sessionName string, outWriter io.Writer) error {
		argv = append([]string(nil), cmd.Args...)
		return nil
	}

	_, err := Launch(Options{
		Strategy:     StrategyTerminal,
		WorktreePath: "/wt/s",
		SessionName:  "s",
		Command:      "claude",
		Out:          io.Discard,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "gnome-terminal") {
		t.Errorf("expected gnome-terminal in argv; got %v", argv)
	}
	if !strings.Contains(joined, "/wt/s") {
		t.Errorf("expected worktree path in argv; got %v", argv)
	}
}

// TestLaunchTerminalLinux_NoEmulator covers the negative path: no
// terminal emulator on PATH must return a real error rather than
// silently succeeding.
func TestLaunchTerminalLinux_NoEmulator(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only dispatch path")
	}
	t.Setenv("PATH", t.TempDir())
	err := launchTerminalLinux(Options{WorktreePath: "/wt", Command: "claude"})
	if err == nil {
		t.Fatal("launchTerminalLinux with empty PATH returned nil; want error")
	}
}

// TestLaunchTerminalDarwin_DispatchesViaSpawnFn covers the macOS
// osascript path. Called directly rather than through launchTerminal
// because the latter prefers Ghostty when present (Ghostty's app
// bundle location is auto-detected, so we can't reliably hide it via
// PATH alone). The function itself is darwin-only at the dispatch
// level — running the test on Linux would invoke osascript via the
// fake spawnFn anyway, but skipping there keeps the assertion
// platform-honest.
func TestLaunchTerminalDarwin_DispatchesViaSpawnFn(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only dispatch path")
	}

	prev := spawnFn
	t.Cleanup(func() { spawnFn = prev })
	resetMacOSStaggerForTest()

	var argv []string
	spawnFn = func(cmd *exec.Cmd, sessionName string, outWriter io.Writer) error {
		argv = append([]string(nil), cmd.Args...)
		return nil
	}

	err := launchTerminalDarwin(Options{
		WorktreePath: "/wt/s",
		SessionName:  "s",
		Command:      "claude",
		Out:          io.Discard,
	})
	if err != nil {
		t.Fatalf("launchTerminalDarwin: %v", err)
	}
	if len(argv) == 0 || argv[0] != "osascript" {
		t.Errorf("expected osascript in argv; got %v", argv)
	}
}
