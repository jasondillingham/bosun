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

// gitignoreRelPath is the per-repo .gitignore Install touches when
// ManageGitignore is on. Always at the repo root — bosun never
// rewrites nested .gitignore files.
const gitignoreRelPath = ".gitignore"

// gitignorePatternIgnore + gitignorePatternKeep are the two non-comment
// lines bosun appends so Claude Code's per-developer
// `.claude/settings.local.json` (and any other state Claude Code drops
// under `.claude/`) stays out of the repo while the tracked
// `.claude/settings.json` — which is where bosun's PreToolUse hook
// lives and where `git clone` carries it — remains visible to git.
const (
	gitignorePatternIgnore = ".claude/*"
	gitignorePatternKeep   = "!.claude/settings.json"
	gitignoreCommentLine   = "# Managed by `bosun hook install` — keep .claude/settings.json tracked."
)

// InstallOptions controls Install's opt-in behaviors. Today the only
// knob is the .gitignore management step; new options should land
// here so callers don't have to take a signature break.
type InstallOptions struct {
	// ManageGitignore turns on the per-repo .gitignore management
	// step. When true Install ensures the repo's `.gitignore`
	// excludes `.claude/*` but keeps `.claude/settings.json` tracked
	// (so the hook contract travels with `git clone`). When false
	// Install touches settings.json only and leaves .gitignore
	// alone — the CLI flips this off when the operator passes
	// `--no-gitignore`.
	ManageGitignore bool
}

// InstallResult is what Install returns so the CLI layer can render
// the right human-facing message (and exit code) without re-checking
// disk state.
type InstallResult struct {
	// SettingsPath is the absolute path bosun wrote (or would write).
	SettingsPath string
	// Changed is true when settings.json was modified; false on
	// idempotent no-op reinstalls.
	Changed bool
	// Gitignored is true when `.claude/settings.json` matches a
	// gitignore entry in the repo *after* Install finishes.
	// Operators care because a gitignored hook config does not
	// travel with `git clone`. With ManageGitignore on this is
	// usually false (the appended pattern includes the keep-tracked
	// negation); it stays true only when an existing blanket
	// `.claude/` exclude is in effect and we refused to rewrite it.
	Gitignored bool
	// GitignorePath is the absolute path to the .gitignore the
	// gitignore-management step targeted. Empty when
	// ManageGitignore was false.
	GitignorePath string
	// GitignoreChanged is true when bosun actually appended its
	// pattern to .gitignore. False on idempotent reinstalls and on
	// the warn-and-skip path where an existing blanket exclude is
	// already in place.
	GitignoreChanged bool
}

// Install adds (or no-ops) the bosun PreToolUse hook entry in
// `<repoRoot>/.claude/settings.json`. The write is atomic (temp +
// rename) so a concurrent reader never sees a torn file. Existing
// unrelated hook entries are preserved. When opts.ManageGitignore is
// true Install also ensures the repo's .gitignore excludes the
// per-developer `.claude/` state while keeping settings.json tracked.
func Install(repoRoot string, opts InstallOptions) (InstallResult, error) {
	settingsPath := filepath.Join(repoRoot, SettingsRelPath)
	res := InstallResult{SettingsPath: settingsPath}

	raw, err := readSettings(settingsPath)
	if err != nil {
		return res, err
	}

	if hookAlreadyInstalled(raw) {
		// settings.json already carries our hook; we still run the
		// gitignore step so an operator can re-run `hook install` to
		// pick up the v0.9.2 gitignore management even when the hook
		// itself is already in place.
		if opts.ManageGitignore {
			if err := applyGitignore(repoRoot, &res); err != nil {
				return res, err
			}
		}
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
	if opts.ManageGitignore {
		if err := applyGitignore(repoRoot, &res); err != nil {
			return res, err
		}
	}
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}

// writeFileAtomic writes data to path via a temp file in the same
// directory followed by os.Rename. Same contract as writeSettings:
// concurrent readers either see the old file or the new one, never a
// half-written intermediate.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("temp %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// applyGitignore runs the gitignore-management step and records the
// outcome on res. It's split out so Install's hot path stays focused
// on settings.json. See ensureGitignore for the three-case logic.
func applyGitignore(repoRoot string, res *InstallResult) error {
	path, changed, err := ensureGitignore(repoRoot)
	if err != nil {
		return err
	}
	res.GitignorePath = path
	res.GitignoreChanged = changed
	return nil
}

// ensureGitignore implements the v0.9.2 gitignore contract:
//
//  1. `.gitignore` absent or has no relevant `.claude/` pattern —
//     append the recommended two-line pattern with a leading comment.
//  2. `.gitignore` already has `.claude/*` + `!.claude/settings.json`
//     — no-op.
//  3. `.gitignore` has an unconditional `.claude/` (or equivalent)
//     exclude that gitignores settings.json — no-op. The CLI surfaces
//     the existing "settings.json appears gitignored" warning so the
//     operator can decide whether they meant to do that.
//
// Returns (path, changed, err). path is always the absolute
// `<repoRoot>/.gitignore` so callers can name it in CLI output.
func ensureGitignore(repoRoot string) (string, bool, error) {
	path := filepath.Join(repoRoot, gitignoreRelPath)

	// Case 3 first: if settings.json is already gitignored, an
	// existing blanket `.claude/` rule (or some other exclude) is in
	// place. Don't touch the file — the operator may have done it
	// deliberately, and overwriting it would be surprising.
	if isGitignored(repoRoot, SettingsRelPath) {
		return path, false, nil
	}

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return path, false, fmt.Errorf("read %s: %w", path, err)
	}

	// Case 2: pattern already present verbatim — nothing to do.
	if hasRecommendedGitignorePattern(existing) {
		return path, false, nil
	}

	// Case 1: append the pattern. Build the new content with a
	// well-separated block so it doesn't run into the previous
	// trailing line.
	updated := appendGitignoreBlock(existing)
	if err := writeFileAtomic(path, updated, 0o644); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// hasRecommendedGitignorePattern returns true when both
// `.claude/*` and `!.claude/settings.json` appear as non-comment
// lines in content. Order doesn't matter; bosun won't re-append even
// if the operator put extra whitespace or comments between them.
func hasRecommendedGitignorePattern(content []byte) bool {
	var hasIgnore, hasKeep bool
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch line {
		case gitignorePatternIgnore:
			hasIgnore = true
		case gitignorePatternKeep:
			hasKeep = true
		}
	}
	return hasIgnore && hasKeep
}

// appendGitignoreBlock returns existing + a properly-separated bosun
// block. Ensures a trailing newline on existing content and inserts a
// blank line separator before the block so the comment isn't glued to
// whatever the operator wrote last.
func appendGitignoreBlock(existing []byte) []byte {
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 {
		if !bytes.HasSuffix(existing, []byte("\n")) {
			buf.WriteByte('\n')
		}
		if !bytes.HasSuffix(buf.Bytes(), []byte("\n\n")) {
			buf.WriteByte('\n')
		}
	}
	buf.WriteString(gitignoreCommentLine)
	buf.WriteByte('\n')
	buf.WriteString(gitignorePatternIgnore)
	buf.WriteByte('\n')
	buf.WriteString(gitignorePatternKeep)
	buf.WriteByte('\n')
	return buf.Bytes()
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
