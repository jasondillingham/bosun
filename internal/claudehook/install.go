package claudehook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SettingsRelPath is the in-repo location bosun installs its
// PreToolUse hook into. Per docs/v0.9.1-hook-spec.md the per-repo
// `.claude/settings.json` is the right scope: it travels with the
// repo so cloners get the integration, unlike `settings.local.json`
// (per-developer) or `~/.claude/settings.json` (per-user, fires on
// every claude session globally).
const SettingsRelPath = ".claude/settings.json"

// HookCommand is the literal command string bosun installs into the
// PreToolUse list. The "starts with" match in install/uninstall keys
// off this constant so we can spot bosun's entries regardless of any
// extra args operators add later.
const HookCommand = "bosun hook claim"

// HookMatcher is the regex run against `tool_name` to decide whether
// the PreToolUse hook fires. v0.9.1 covers the four mutating tools;
// Bash and the read-only tools deliberately stay out (see "What's
// NOT in v0.9.1" in the spec for the rationale).
const HookMatcher = "Edit|Write|MultiEdit|NotebookEdit"

// HookTimeoutMillis caps how long Claude Code waits for the hook
// before killing it. 5 s gives the direct-claim and MCP-socket paths
// plenty of headroom while still bounding pathological hangs. The
// hook always exits 0 so a timeout just means "Claude Code moves on
// without a claim recorded."
const HookTimeoutMillis = 5000

// InstallResult is what Install returns so the CLI layer can render
// the right human-facing message (and exit code) without re-checking
// disk state.
type InstallResult struct {
	// SettingsPath is the absolute path bosun wrote (or would write).
	SettingsPath string
	// Changed is true when the file was modified; false on idempotent
	// no-op reinstalls.
	Changed bool
	// Gitignored is true when `.claude/settings.json` matches a
	// gitignore entry in the repo. Operators care because a
	// gitignored hook config does not travel with `git clone`.
	Gitignored bool
}

// Install adds (or no-ops) the bosun PreToolUse hook entry in
// `<repoRoot>/.claude/settings.json`. The write is atomic (temp +
// rename) so a concurrent reader never sees a torn file. Existing
// unrelated hook entries are preserved.
func Install(repoRoot string) (InstallResult, error) {
	settingsPath := filepath.Join(repoRoot, SettingsRelPath)
	res := InstallResult{SettingsPath: settingsPath}

	raw, err := readSettings(settingsPath)
	if err != nil {
		return res, err
	}

	if hookAlreadyInstalled(raw) {
		res.Gitignored = isGitignored(repoRoot, SettingsRelPath)
		return res, nil
	}

	modified, err := appendHook(raw)
	if err != nil {
		return res, err
	}
	if err := writeSettings(settingsPath, modified); err != nil {
		return res, err
	}
	res.Changed = true
	res.Gitignored = isGitignored(repoRoot, SettingsRelPath)
	return res, nil
}

// readSettings returns the parsed JSON object (as a generic
// map[string]any so unknown keys round-trip) for path, or an empty
// map when the file is absent. Parse failures surface as errors —
// install must not silently overwrite a hand-edited settings file it
// couldn't read.
func readSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if obj == nil {
		return map[string]any{}, nil
	}
	return obj, nil
}

// hookAlreadyInstalled walks the parsed settings tree looking for a
// PreToolUse entry whose nested hooks list contains a command that
// starts with HookCommand. Tolerant of partial / hand-edited shapes:
// missing keys, type mismatches, etc. all degrade to "no bosun hook
// found" rather than panicking.
func hookAlreadyInstalled(obj map[string]any) bool {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	pre, _ := hooks["PreToolUse"].([]any)
	for _, entry := range pre {
		e, _ := entry.(map[string]any)
		if e == nil {
			continue
		}
		inner, _ := e["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.HasPrefix(strings.TrimSpace(cmd), HookCommand) {
				return true
			}
		}
	}
	return false
}

// appendHook returns the obj with bosun's PreToolUse entry appended.
// Builds the intermediate `hooks` and `PreToolUse` containers when
// absent. Preserves unrelated PreToolUse entries (we append; never
// replace).
func appendHook(obj map[string]any) (map[string]any, error) {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		obj["hooks"] = hooks
	}
	pre, _ := hooks["PreToolUse"].([]any)
	entry := map[string]any{
		"matcher": HookMatcher,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": HookCommand,
				"timeout": HookTimeoutMillis,
			},
		},
	}
	pre = append(pre, entry)
	hooks["PreToolUse"] = pre
	return obj, nil
}

// writeSettings serializes obj to path atomically (temp + rename).
// Mirrors the pattern claims.Store uses so concurrent readers (the
// editor that wrote the settings; `claude` reading them) never see a
// half-written file.
func writeSettings(path string, obj map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("temp settings: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp settings: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename settings: %w", err)
	}
	return nil
}

// isGitignored shells out to `git check-ignore` to discover whether
// the operator's .gitignore would exclude `relPath`. Best-effort:
// any failure (no git binary, not a git repo, etc.) reports false so
// install never breaks because of a probing failure.
func isGitignored(repoRoot, relPath string) bool {
	cmd := exec.Command("git", "-C", repoRoot, "check-ignore", "-q", relPath)
	err := cmd.Run()
	if err == nil {
		return true
	}
	// git check-ignore exits 1 when the path is NOT ignored, and >1
	// for actual errors. We only treat exit 0 as "ignored"; everything
	// else (including missing git, missing repo) is "not ignored from
	// our standpoint."
	return false
}
