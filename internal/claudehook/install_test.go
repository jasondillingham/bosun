package claudehook

import (
	"encoding/json"
	"os"
	"os/exec"
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

// initGitRepo runs `git init` in dir and configures a minimal user so
// any test that needs `git check-ignore` to behave like a real repo
// can rely on it. Skips the test if git isn't on PATH so the package
// stays buildable on minimal CI images.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestInstall_EmptyRepo(t *testing.T) {
	repo := t.TempDir()
	res, err := Install(repo, InstallOptions{})
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

	if _, err := Install(repo, InstallOptions{}); err != nil {
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
	first, err := Install(repo, InstallOptions{})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !first.Changed {
		t.Fatal("first install should report Changed=true")
	}
	second, err := Install(repo, InstallOptions{})
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
	res, err := Install(repo, InstallOptions{})
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
	if _, err := Install(repo, InstallOptions{}); err == nil {
		t.Fatal("Install should refuse to overwrite unparseable settings.json")
	}
}

func TestUninstall_RemovesOnlyBosunEntry(t *testing.T) {
	repo := t.TempDir()
	if _, err := Install(repo, InstallOptions{}); err != nil {
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
	if _, err := Install(repo, InstallOptions{}); err != nil {
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

// TestInstall_GitignoreAbsent covers case 1 from the brief: a fresh
// repo with no .gitignore. ManageGitignore=true should create one
// holding the bosun block.
func TestInstall_GitignoreAbsent(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	res, err := Install(repo, InstallOptions{ManageGitignore: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.GitignoreChanged {
		t.Fatal("expected GitignoreChanged=true on fresh repo")
	}
	if res.GitignorePath != filepath.Join(repo, ".gitignore") {
		t.Fatalf("unexpected GitignorePath %q", res.GitignorePath)
	}
	if res.Gitignored {
		t.Fatal("settings.json should NOT be gitignored after install (the keep-tracked negation is in the block)")
	}

	body, err := os.ReadFile(res.GitignorePath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(body), gitignorePatternIgnore) {
		t.Errorf(".gitignore missing %q:\n%s", gitignorePatternIgnore, body)
	}
	if !strings.Contains(string(body), gitignorePatternKeep) {
		t.Errorf(".gitignore missing %q:\n%s", gitignorePatternKeep, body)
	}
	if !strings.Contains(string(body), "bosun") {
		t.Errorf(".gitignore comment should mention bosun so a future reader can grep back to the source:\n%s", body)
	}
}

// TestInstall_GitignoreAppendsToExisting confirms the bosun block
// lands AFTER pre-existing operator content and doesn't smash a
// missing-trailing-newline boundary.
func TestInstall_GitignoreAppendsToExisting(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	// Deliberately no trailing newline so we exercise the separator
	// logic in appendGitignoreBlock.
	existing := "node_modules/\nbuild/"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Install(repo, InstallOptions{ManageGitignore: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.GitignoreChanged {
		t.Fatal("expected GitignoreChanged=true when appending to a .gitignore without our pattern")
	}

	body, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.HasPrefix(string(body), existing) {
		t.Errorf("existing .gitignore content was clobbered:\n%s", body)
	}
	if !strings.Contains(string(body), gitignorePatternIgnore) {
		t.Errorf(".gitignore missing %q:\n%s", gitignorePatternIgnore, body)
	}
	if !strings.Contains(string(body), gitignorePatternKeep) {
		t.Errorf(".gitignore missing %q:\n%s", gitignorePatternKeep, body)
	}
}

// TestInstall_GitignoreAlreadyCorrect covers case 2: pattern already
// present, no-op. Running install twice must produce the same bytes.
func TestInstall_GitignoreAlreadyCorrect(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	preexisting := "# operator notes\nnode_modules/\n.claude/*\n!.claude/settings.json\n"
	gi := filepath.Join(repo, ".gitignore")
	if err := os.WriteFile(gi, []byte(preexisting), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Install(repo, InstallOptions{ManageGitignore: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.GitignoreChanged {
		t.Fatal("expected GitignoreChanged=false when the pattern is already present")
	}

	after, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(after) != preexisting {
		t.Errorf(".gitignore was modified when it shouldn't have been:\nbefore:\n%s\nafter:\n%s", preexisting, after)
	}
}

// TestInstall_GitignoreBlanketExclude covers case 3: an unconditional
// `.claude/` exclude that does gitignore settings.json. Install must
// leave .gitignore alone and surface Gitignored=true so the CLI's
// existing warning fires.
func TestInstall_GitignoreBlanketExclude(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	preexisting := "# blanket exclude on purpose\n.claude/\n"
	gi := filepath.Join(repo, ".gitignore")
	if err := os.WriteFile(gi, []byte(preexisting), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Install(repo, InstallOptions{ManageGitignore: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.GitignoreChanged {
		t.Fatal("Install must NOT rewrite a blanket .claude/ exclude — operator may have done it deliberately")
	}
	if !res.Gitignored {
		t.Fatal("expected Gitignored=true so the CLI's existing warning fires")
	}

	after, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(after) != preexisting {
		t.Errorf(".gitignore was modified despite blanket exclude:\nbefore:\n%s\nafter:\n%s", preexisting, after)
	}
}

// TestInstall_GitignoreIdempotent runs Install twice with
// ManageGitignore on and asserts the second call is a no-op
// byte-for-byte. Catches future regressions where the pattern detect
// disagrees with what we just wrote.
func TestInstall_GitignoreIdempotent(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	if _, err := Install(repo, InstallOptions{ManageGitignore: true}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	gi := filepath.Join(repo, ".gitignore")
	first, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("read .gitignore after first install: %v", err)
	}

	res, err := Install(repo, InstallOptions{ManageGitignore: true})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if res.GitignoreChanged {
		t.Fatal("expected GitignoreChanged=false on a second install over already-correct .gitignore")
	}

	second, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("read .gitignore after second install: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("idempotent install changed .gitignore bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestInstall_NoGitignoreLeavesFileAlone confirms the opt-out path:
// ManageGitignore=false must not create or modify .gitignore.
func TestInstall_NoGitignoreLeavesFileAlone(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	res, err := Install(repo, InstallOptions{ManageGitignore: false})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.GitignoreChanged {
		t.Fatal("Install with ManageGitignore=false must not change .gitignore")
	}
	if res.GitignorePath != "" {
		t.Fatalf("expected empty GitignorePath when management is off, got %q", res.GitignorePath)
	}
	if _, err := os.Stat(filepath.Join(repo, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("expected .gitignore to remain absent, stat err=%v", err)
	}
}
