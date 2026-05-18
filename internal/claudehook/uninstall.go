package claudehook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// UninstallResult tells the CLI how to phrase the post-uninstall
// summary: whether the file existed at all, whether bosun's entry
// was actually present, and whether the resulting file collapsed
// back to an empty object (which we still keep on disk — operators
// frequently store other Claude Code settings there too).
type UninstallResult struct {
	SettingsPath string
	Existed      bool
	Removed      bool
}

// Uninstall removes every PreToolUse entry whose nested hooks list
// contains a `bosun hook claim` command. If PreToolUse becomes empty
// the key itself is dropped; if `hooks` becomes empty it's dropped
// too. Other settings are preserved untouched.
func Uninstall(repoRoot string) (UninstallResult, error) {
	settingsPath := filepath.Join(repoRoot, SettingsRelPath)
	res := UninstallResult{SettingsPath: settingsPath}

	raw, err := readSettings(settingsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	if _, err := os.Stat(settingsPath); err == nil {
		res.Existed = true
	}

	changed := stripBosunHook(raw)
	if !changed {
		return res, nil
	}
	res.Removed = true
	if err := writeSettings(settingsPath, raw); err != nil {
		return res, err
	}
	return res, nil
}

// stripBosunHook mutates obj in place, removing every PreToolUse
// entry whose inner hooks contain HookCommand. Empty containers
// collapse upward: an emptied PreToolUse drops the key, an emptied
// hooks map drops the whole `hooks` key. Returns true when anything
// changed.
func stripBosunHook(obj map[string]any) bool {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) == 0 {
		return false
	}
	kept := pre[:0]
	removed := false
	for _, entry := range pre {
		e, _ := entry.(map[string]any)
		if e == nil {
			kept = append(kept, entry)
			continue
		}
		inner, _ := e["hooks"].([]any)
		filteredInner := inner[:0]
		entryChanged := false
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if hm == nil {
				filteredInner = append(filteredInner, h)
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.HasPrefix(strings.TrimSpace(cmd), HookCommand) {
				removed = true
				entryChanged = true
				continue
			}
			filteredInner = append(filteredInner, h)
		}
		if entryChanged {
			if len(filteredInner) == 0 {
				continue
			}
			e["hooks"] = filteredInner
		}
		kept = append(kept, entry)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, "PreToolUse")
	} else {
		hooks["PreToolUse"] = kept
	}
	if len(hooks) == 0 {
		delete(obj, "hooks")
	}
	return true
}
