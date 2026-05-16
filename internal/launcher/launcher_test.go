package launcher

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrintStrategy(t *testing.T) {
	var buf bytes.Buffer
	got, err := Launch(Options{
		Strategy:     StrategyPrint,
		WorktreePath: "/path/with spaces",
		SessionName:  "session-1",
		Command:      "claude",
		Env:          map[string]string{"GOCACHE": "/tmp/cache"},
		Out:          &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != StrategyPrint {
		t.Fatalf("strategy = %s, want print", got)
	}
	out := buf.String()
	if !strings.Contains(out, "session-1") {
		t.Errorf("output missing session name: %s", out)
	}
	if !strings.Contains(out, "GOCACHE=") {
		t.Errorf("output missing GOCACHE env: %s", out)
	}
	if !strings.Contains(out, "'/path/with spaces'") {
		t.Errorf("worktree path not quoted: %s", out)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":             "'plain'",
		"with space":        "'with space'",
		"with'quote":        `'with'\''quote'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildShellEnvPrefix_Sorted(t *testing.T) {
	got := buildShellEnvPrefix(map[string]string{"B": "2", "A": "1"})
	want := "A='1' B='2' "
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildCmdEnvPrefix(t *testing.T) {
	got := buildCmdEnvPrefix(map[string]string{"A": "1", "B": "2"})
	if !strings.Contains(got, "set A=1") || !strings.Contains(got, "set B=2") {
		t.Fatalf("buildCmdEnvPrefix = %q", got)
	}
}

func TestIsolateCacheEnv(t *testing.T) {
	wt := filepath.Join(string(filepath.Separator)+"wt", "x")
	env := IsolateCacheEnv(wt)
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "npm_config_cache", "PYTHONPYCACHEPREFIX", "CARGO_TARGET_DIR"} {
		if env[key] == "" {
			t.Errorf("missing env key %s", key)
		}
	}
	if !strings.HasPrefix(env["GOCACHE"], wt) {
		t.Errorf("GOCACHE not under worktree: %s", env["GOCACHE"])
	}
}

func TestLaunch_MissingPath(t *testing.T) {
	_, err := Launch(Options{Strategy: StrategyPrint})
	if err == nil {
		t.Fatal("expected error for empty WorktreePath")
	}
}

func TestPrintStrategy_IncludesInitialPrompt(t *testing.T) {
	var buf bytes.Buffer
	_, err := Launch(Options{
		Strategy:      StrategyPrint,
		WorktreePath:  "/wt/session-1",
		SessionName:   "session-1",
		Command:       "claude",
		InitialPrompt: "Read BOSUN_BRIEF.md in this directory",
		Out:           &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "'Read BOSUN_BRIEF.md in this directory'") {
		t.Fatalf("print output missing quoted prompt:\n%s", out)
	}
}

func TestShellInvocation_QuotesPrompt(t *testing.T) {
	got := shellInvocation(Options{
		Command:       "claude",
		InitialPrompt: "hello world",
	})
	want := "claude 'hello world'"
	if got != want {
		t.Fatalf("shellInvocation = %q, want %q", got, want)
	}
}

func TestShellInvocation_EmptyPrompt(t *testing.T) {
	got := shellInvocation(Options{Command: "claude"})
	if got != "claude" {
		t.Fatalf("shellInvocation = %q, want claude", got)
	}
}

func TestShellInvocation_EscapesSingleQuotes(t *testing.T) {
	got := shellInvocation(Options{
		Command:       "claude",
		InitialPrompt: "it's a test",
	})
	want := `claude 'it'\''s a test'`
	if got != want {
		t.Fatalf("shellInvocation = %q, want %q", got, want)
	}
}

func TestGhosttyArgs_AlwaysNewWindow(t *testing.T) {
	// Ghostty's CLI doesn't support opening a tab in an existing window
	// (long-requested upstream feature). Until then, OpenAsTab is a no-op
	// and every session opens in its own window.
	for _, opts := range []Options{
		{WorktreePath: "/wt/session-1", Command: "claude"},
		{WorktreePath: "/wt/session-2", Command: "claude", OpenAsTab: true},
	} {
		args := ghosttyArgs(opts)
		for _, a := range args {
			if a == "+new-tab" {
				t.Fatalf("ghostty CLI doesn't support +new-tab; should never appear in argv, got: %v", args)
			}
		}
		if args[0] != "-e" {
			t.Fatalf("expected first arg to be -e, got %q", args[0])
		}
	}
}

func TestGhosttyArgs_CarriesPromptAndCWD(t *testing.T) {
	args := ghosttyArgs(Options{
		WorktreePath:  "/wt/session-2",
		Command:       "claude",
		InitialPrompt: "hello",
	})
	// Find the bash -lc payload — must include both the cd and the quoted prompt.
	var payload string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-lc" {
			payload = args[i+1]
			break
		}
	}
	if payload == "" {
		t.Fatalf("did not find bash -lc payload in args: %v", args)
	}
	if !strings.Contains(payload, "cd '/wt/session-2'") {
		t.Errorf("payload missing cd: %s", payload)
	}
	if !strings.Contains(payload, "claude 'hello'") {
		t.Errorf("payload missing prompted claude invocation: %s", payload)
	}
}

func TestITerm2Script_NewWindow(t *testing.T) {
	script := iTerm2Script(Options{
		WorktreePath:  "/wt/session-1",
		SessionName:   "session-1",
		Command:       "claude",
		InitialPrompt: "kick off",
		Env:           map[string]string{"FOO": "bar"},
	})
	if !strings.Contains(script, `create window with default profile`) {
		t.Errorf("new-window script missing create window:\n%s", script)
	}
	if strings.Contains(script, `create tab with default profile`) {
		t.Errorf("new-window script unexpectedly creates a tab:\n%s", script)
	}
	if !strings.Contains(script, `cd '/wt/session-1'`) {
		t.Errorf("script missing worktree cd:\n%s", script)
	}
	if !strings.Contains(script, `FOO='bar'`) {
		t.Errorf("script missing env injection:\n%s", script)
	}
	if !strings.Contains(script, `claude '"'"'kick off'"'"'`) && !strings.Contains(script, `kick off`) {
		t.Errorf("script missing initial-prompt invocation:\n%s", script)
	}
}

func TestITerm2Script_OpenAsTab(t *testing.T) {
	script := iTerm2Script(Options{
		WorktreePath: "/wt/session-2",
		SessionName:  "session-2",
		Command:      "claude",
		OpenAsTab:    true,
	})
	if !strings.Contains(script, `create tab with default profile`) {
		t.Errorf("tab script missing create-tab clause:\n%s", script)
	}
	if !strings.Contains(script, `if (count of windows) = 0`) {
		t.Errorf("tab script missing zero-window guard:\n%s", script)
	}
}

func TestITerm2Script_EscapesQuotes(t *testing.T) {
	// AppleScript strings use " as the delimiter; the inner shell command
	// includes embedded " (the prompt). They must arrive at osascript
	// escaped, otherwise the script terminates the string early.
	script := iTerm2Script(Options{
		WorktreePath:  "/wt",
		SessionName:   "s",
		Command:       "claude",
		InitialPrompt: `say "hi"`,
	})
	// The escaped form should appear; an unescaped quote would mean the
	// AppleScript string was terminated.
	if !strings.Contains(script, `\"hi\"`) {
		t.Errorf("expected escaped quotes in script:\n%s", script)
	}
}

func TestWindowsTerminalArgs_NewWindow(t *testing.T) {
	args := windowsTerminalArgs(Options{
		WorktreePath:  `C:\code\wt`,
		Command:       "claude",
		InitialPrompt: "kick off",
		Env:           map[string]string{"X": "1"},
	})
	if got := strings.Join(args, " "); !strings.Contains(got, `-d C:\code\wt`) {
		t.Errorf("expected -d <path> in args, got %v", args)
	}
	if got := strings.Join(args, " "); strings.HasPrefix(got, "-w") {
		t.Errorf("new-window form should not pass -w, got %v", args)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "set X=1") {
		t.Errorf("expected env prefix in command, got %v", args)
	}
}

func TestWindowsTerminalArgs_OpenAsTab(t *testing.T) {
	args := windowsTerminalArgs(Options{
		WorktreePath: `C:\code\wt`,
		Command:      "claude",
		OpenAsTab:    true,
	})
	got := strings.Join(args, " ")
	if !strings.HasPrefix(got, "-w 0 new-tab") {
		t.Errorf("tab form should start with `-w 0 new-tab`, got %v", args)
	}
	if !strings.Contains(got, `-d C:\code\wt`) {
		t.Errorf("expected -d <path> in args, got %v", args)
	}
}

func TestHasGhostty_OnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH stub uses POSIX shebang")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "ghostty")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bin, ok := hasGhostty()
	if !ok {
		t.Fatal("hasGhostty() = false, want true with stub on PATH")
	}
	// Resolve through symlinks (macOS /tmp → /private/tmp) before comparing.
	resolvedBin, _ := filepath.EvalSymlinks(bin)
	resolvedStub, _ := filepath.EvalSymlinks(stub)
	if resolvedBin != resolvedStub {
		t.Errorf("hasGhostty() = %q, want %q", resolvedBin, resolvedStub)
	}
}
