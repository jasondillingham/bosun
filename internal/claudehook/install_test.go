package claudehook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readJSON returns the parsed JSON map for an arbitrary file path.
// Centralized so the assertion helpers below stay readable.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return obj
}

func countBosunHooks(t *testing.T, obj map[string]any) int {
	t.Helper()
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		return 0
	}
	pre, _ := hooks["PreToolUse"].([]any)
	n := 0
	for _, e := range pre {
		em, _ := e.(map[string]any)
		if em == nil {
			continue
		}
		inner, _ := em["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.HasPrefix(cmd, HookCommand) {
				n++
			}
		}
	}
	return n
}

func TestInstall_EmptyRepo(t *testing.T) {
	repo := t.TempDir()
	res, err := Install(repo)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true on first install")
	}
	if res.SettingsPath != filepath.Join(repo, SettingsRelPath) {
		t.Fatalf("unexpected SettingsPath %q", res.SettingsPath)
	}
	obj := readJSON(t, res.SettingsPath)
	if got := countBosunHooks(t, obj); got != 1 {
		t.Fatalf("expected 1 bosun hook, got %d (settings: %v)", got, obj)
	}
}

func TestInstall_PreservesUnrelatedSettings(t *testing.T) {
	repo := t.TempDir()
	settings := filepath.Join(repo, SettingsRelPath)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := `{
  "model": "claude-opus-4-7",
  "permissions": {"allow": ["Read", "Edit"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "echo running bash"}]}
    ],
    "PostToolUse": [
      {"matcher": ".*", "hooks": [{"type": "command", "command": "echo done"}]}
    ]
  }
}`
	if err := os.WriteFile(settings, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(repo); err != nil {
		t.Fatalf("Install: %v", err)
	}

	obj := readJSON(t, settings)
	if model, _ := obj["model"].(string); model != "claude-opus-4-7" {
		t.Fatalf("model setting lost; got %q", model)
	}
	if _, ok := obj["permissions"]; !ok {
		t.Fatal("permissions setting lost")
	}
	hooks, _ := obj["hooks"].(map[string]any)
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Fatal("PostToolUse entry lost")
	}
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("expected 2 PreToolUse entries (1 existing + bosun), got %d", len(pre))
	}
	if got := countBosunHooks(t, obj); got != 1 {
		t.Fatalf("expected 1 bosun hook entry, got %d", got)
	}
}

func TestInstall_Idempotent(t *testing.T) {
	repo := t.TempDir()
	first, err := Install(repo)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !first.Changed {
		t.Fatal("first install should report Changed=true")
	}
	second, err := Install(repo)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second.Changed {
		t.Fatal("second install should report Changed=false")
	}
	obj := readJSON(t, first.SettingsPath)
	if got := countBosunHooks(t, obj); got != 1 {
		t.Fatalf("expected exactly 1 bosun hook after idempotent re-install, got %d", got)
	}
}

func TestInstall_ParsesExistingValidJSON(t *testing.T) {
	repo := t.TempDir()
	settings := filepath.Join(repo, SettingsRelPath)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	// Empty {} should parse cleanly without overwriting.
	if err := os.WriteFile(settings, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Install(repo)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true on first install over empty settings")
	}
	obj := readJSON(t, settings)
	if got := countBosunHooks(t, obj); got != 1 {
		t.Fatalf("expected 1 bosun hook, got %d", got)
	}
}

func TestInstall_RefusesUnparseableJSON(t *testing.T) {
	repo := t.TempDir()
	settings := filepath.Join(repo, SettingsRelPath)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(repo); err == nil {
		t.Fatal("Install should refuse to overwrite unparseable settings.json")
	}
}

func TestUninstall_RemovesOnlyBosunEntry(t *testing.T) {
	repo := t.TempDir()
	if _, err := Install(repo); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Add a sibling unrelated hook entry so we can verify it survives.
	settings := filepath.Join(repo, SettingsRelPath)
	obj := readJSON(t, settings)
	hooks := obj["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	pre = append(pre, map[string]any{
		"matcher": "Bash",
		"hooks": []any{
			map[string]any{"type": "command", "command": "echo bash"},
		},
	})
	hooks["PreToolUse"] = pre
	data, _ := json.MarshalIndent(obj, "", "  ")
	if err := os.WriteFile(settings, data, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Uninstall(repo)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Removed {
		t.Fatal("expected Removed=true")
	}
	after := readJSON(t, settings)
	if got := countBosunHooks(t, after); got != 0 {
		t.Fatalf("expected 0 bosun hooks after uninstall, got %d", got)
	}
	hooks2 := after["hooks"].(map[string]any)
	pre2 := hooks2["PreToolUse"].([]any)
	if len(pre2) != 1 {
		t.Fatalf("expected 1 unrelated PreToolUse entry left, got %d", len(pre2))
	}
}

func TestUninstall_CollapsesEmptyContainers(t *testing.T) {
	repo := t.TempDir()
	if _, err := Install(repo); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := Uninstall(repo); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	settings := filepath.Join(repo, SettingsRelPath)
	obj := readJSON(t, settings)
	if _, ok := obj["hooks"]; ok {
		t.Fatalf("expected 'hooks' key dropped after the only bosun entry was removed; got %v", obj)
	}
}

func TestUninstall_NoSettingsFile(t *testing.T) {
	repo := t.TempDir()
	res, err := Uninstall(repo)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if res.Removed {
		t.Fatal("Uninstall on missing settings should be a no-op")
	}
}
