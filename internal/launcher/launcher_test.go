package launcher

import (
	"bytes"
	"path/filepath"
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
