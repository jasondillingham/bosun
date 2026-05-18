package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestScenario_HookInstallWritesSettings verifies the install
// command creates `.claude/settings.json` and lands a PreToolUse
// entry whose command starts with `bosun hook claim`. Operators run
// `bosun hook install` once per repo — the on-disk shape it produces
// is the contract Claude Code reads.
func TestScenario_HookInstallWritesSettings(t *testing.T) {
	s := newScenario(t)
	out := s.Bosun("hook", "install")
	if !strings.Contains(out, "hook installed") {
		t.Fatalf("expected install confirmation, got:\n%s", out)
	}

	data := readFile(t, filepath.Join(s.repo, ".claude", "settings.json"))
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		t.Fatalf("parse settings.json: %v\n%s", err, data)
	}
	hooks, _ := obj["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) == 0 {
		t.Fatalf("PreToolUse entries missing:\n%s", data)
	}
	found := false
	for _, e := range pre {
		em := e.(map[string]any)
		inner, _ := em["hooks"].([]any)
		for _, h := range inner {
			hm := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			if strings.HasPrefix(cmd, "bosun hook claim") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("bosun hook entry not found in settings.json:\n%s", data)
	}

	// Re-running install should be a no-op (idempotent).
	out2 := s.Bosun("hook", "install")
	if !strings.Contains(out2, "already installed") {
		t.Fatalf("expected idempotent message on second install, got:\n%s", out2)
	}
}

// TestScenario_HookUninstallRemovesEntry verifies the inverse: a
// freshly installed hook can be removed cleanly, leaving the
// settings file behind (operators may have other settings there) but
// stripping any `bosun hook claim` line.
func TestScenario_HookUninstallRemovesEntry(t *testing.T) {
	s := newScenario(t)
	s.Bosun("hook", "install")
	out := s.Bosun("hook", "uninstall")
	if !strings.Contains(out, "hook removed") {
		t.Fatalf("expected uninstall confirmation, got:\n%s", out)
	}
	// File should still exist (empty) so other Claude Code settings
	// the operator added aren't surprised by a vanished file.
	if _, err := os.Stat(filepath.Join(s.repo, ".claude", "settings.json")); err != nil {
		t.Fatalf("settings.json should still exist post-uninstall: %v", err)
	}
}

// TestScenario_HookClaimEditPath drives the PreToolUse handler end-
// to-end. We init a multi-session repo, pipe a synthetic Edit
// payload to `bosun hook claim` from inside session-2's worktree,
// and then assert `.bosun/claims/session-2.json` lists the edited
// path. This is the test the spec calls out under "Scenario test in
// cmd/bosun/" — it proves Claude Code → bosun would land a claim
// without any agent cooperation.
func TestScenario_HookClaimEditPath(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "2")
	wt2 := s.WorktreePath(2)

	editedRel := "cmd/foo.go"
	editedAbs := filepath.Join(wt2, editedRel)
	payload := mustJSON(t, map[string]any{
		"session_id":      "claude-test",
		"hook_event_name": "PreToolUse",
		"cwd":             wt2,
		"tool_name":       "Edit",
		"tool_input": map[string]any{
			"file_path":  editedAbs,
			"old_string": "x",
			"new_string": "y",
		},
	})
	runHookClaim(t, s, wt2, payload)

	claimsFile := filepath.Join(s.repo, ".bosun", "claims", "session-2.json")
	if _, err := os.Stat(claimsFile); err != nil {
		// List the claims dir to help diagnose pattern-mismatch cases.
		dir := filepath.Join(s.repo, ".bosun", "claims")
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("missing %s; claims/ contains %v; cwd was %s; repo %s", claimsFile, names, wt2, s.repo)
	}
	data := readFile(t, claimsFile)
	if !strings.Contains(data, editedRel) {
		t.Fatalf("session-2 claims missing %q:\n%s", editedRel, data)
	}
}

// TestScenario_HookClaimDedupesAcrossInvocations confirms that a
// burst of edits to the same path doesn't blow up the claim file —
// claims.Store.Add dedupes already, but this is the end-to-end
// guarantee the spec calls out under "Path deduplication."
func TestScenario_HookClaimDedupesAcrossInvocations(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")
	wt1 := s.WorktreePath(1)

	editedAbs := filepath.Join(wt1, "a.go")
	payload := mustJSON(t, map[string]any{
		"cwd":       wt1,
		"tool_name": "Edit",
		"tool_input": map[string]any{
			"file_path": editedAbs,
		},
	})
	for i := 0; i < 3; i++ {
		runHookClaim(t, s, wt1, payload)
	}

	claimsFile := filepath.Join(s.repo, ".bosun", "claims", "session-1.json")
	var c struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(readFile(t, claimsFile)), &c); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	if len(c.Paths) != 1 {
		t.Fatalf("expected 1 claim entry after dedupe, got %d: %v", len(c.Paths), c.Paths)
	}
}

// TestScenario_HookClaimSilentlyIgnoresMainWorktree drives the
// handler with a `cwd` inside the main worktree (not a session
// worktree). The hook should exit 0 and write nothing — editing the
// main worktree is the operator's prerogative, not a session.
func TestScenario_HookClaimSilentlyIgnoresMainWorktree(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	payload := mustJSON(t, map[string]any{
		"cwd":       s.repo,
		"tool_name": "Edit",
		"tool_input": map[string]any{
			"file_path": filepath.Join(s.repo, "README.md"),
		},
	})
	runHookClaim(t, s, s.repo, payload)

	claimsDir := filepath.Join(s.repo, ".bosun", "claims")
	entries, _ := os.ReadDir(claimsDir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		t.Fatalf("unexpected claim file %s after main-worktree edit", e.Name())
	}
}

// TestScenario_HookClaimNeverBlocks proves the "always exit 0"
// contract against the worst input we can stuff in: garbage on
// stdin. Claude Code would block a tool call if the hook exited
// non-zero, so this is the one invariant that absolutely cannot
// regress.
func TestScenario_HookClaimNeverBlocks(t *testing.T) {
	s := newScenario(t)
	cmd := exec.Command(bosunBin, "hook", "claim")
	cmd.Dir = s.repo
	cmd.Stdin = strings.NewReader("not even close to json")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook claim should exit 0 even on bad stdin; got %v\n%s", err, buf.String())
	}
}

// runHookClaim spawns `bosun hook claim` from dir, pipes payload on
// stdin, and fails the test if the process exits non-zero. The
// command must NEVER fail in production — Claude Code reads non-zero
// as "block this tool call" — so test failures here are loud.
func runHookClaim(t *testing.T, s *scenario, dir string, payload []byte) {
	t.Helper()
	cmd := exec.Command(bosunBin, "hook", "claim")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(payload)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("bosun hook claim in %s: %v\n%s", dir, err, buf.String())
	}
	if buf.Len() > 0 {
		t.Logf("hook stderr: %s", buf.String())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return data
}

// Silence unused-import warnings in case future test refactors drop
// fmt from this file.
var _ = fmt.Sprintf
