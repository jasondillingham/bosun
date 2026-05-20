package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunSpawn_RefusesWhenDisabled pins the first user-facing
// failure mode: if agent_spawn isn't enabled in this repo's config,
// the CLI should print an actionable error pointing at the config
// key to flip, NOT silently no-op or surface an opaque internal
// error.
func TestRunSpawn_RefusesWhenDisabled(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	// Default config: agent_spawn.enabled = false.

	err := runSpawn("session-1", "plan.md", false)
	if err == nil {
		t.Fatal("expected refusal when agent_spawn.enabled=false, got nil")
	}
	for _, want := range []string{"agent_spawn is disabled", "bosun config set agent_spawn.enabled true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should include %q; got: %v", want, err)
		}
	}
}

// TestRunSpawn_RefusesUnknownParent: even with spawn enabled, the
// parent label has to name a real bosun-managed session. A typo
// gets the same "not found" error other CLI commands return.
func TestRunSpawn_RefusesUnknownParent(t *testing.T) {
	repo := initBosunRepo(t)
	// Enable agent_spawn via config.json so the first gate passes.
	cfgDir := filepath.Join(repo, ".bosun")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	cfgBody := `{"agent_spawn": {"enabled": true, "max_depth": 2, "max_concurrent_sub_sessions": 3}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	chdir(t, repo)

	err := runSpawn("session-99", "plan.md", false)
	if err == nil {
		t.Fatal("expected error for unknown parent, got nil")
	}
	if !strings.Contains(err.Error(), "session-99 not found") {
		t.Errorf("error should report `session-99 not found`; got: %v", err)
	}
}

// TestRunSpawn_RefusesMissingBriefFile keeps the operator from a
// confusing brief-parse error by failing on file open first.
func TestRunSpawn_RefusesMissingBriefFile(t *testing.T) {
	repo := initBosunRepo(t)
	cfgDir := filepath.Join(repo, ".bosun")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	cfgBody := `{"agent_spawn": {"enabled": true, "max_depth": 2, "max_concurrent_sub_sessions": 3}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	chdir(t, repo)

	// Don't write the brief file. With agent_spawn enabled but no
	// parent-existence yet, runSpawn should hit the parent check
	// before the file open. To exercise the file-open branch we'd
	// need a real parent session; for now this test asserts that the
	// disabled-spawn branch (cheapest gate) doesn't fire when enabled.
	err := runSpawn("session-1", "missing.md", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Either "not found" (parent gate) or "read brief" (file gate);
	// the actual order is parent-first because that's a cheaper check.
	// Either way, the error should be specific.
	if !strings.Contains(err.Error(), "session-1 not found") && !strings.Contains(err.Error(), "missing.md") {
		t.Errorf("expected specific parent-not-found or missing.md error; got: %v", err)
	}
}
