// Package claudehook implements bosun's integration with Claude Code's
// PreToolUse hook system. When the operator runs `bosun hook install`,
// bosun appends a hook entry to `<repo>/.claude/settings.json` so that
// Claude Code intercepts every Edit / Write / MultiEdit / NotebookEdit
// call and runs `bosun hook claim`. The handler resolves the calling
// session from the worktree path and records the edited path in the
// session's claim file — so `bosun status --with-overlaps` becomes a
// reliable view of what every session is touching instead of relying
// on agents to remember the bosun_claim convention.
//
// Hard requirement: the hook NEVER blocks the agent. Every failure
// mode (parse error, missing path, write failure, etc.) degrades to
// "log to stderr, exit 0" so a misconfigured bosun install can't
// break tool calls that would otherwise succeed.
//
// Sub-session inheritance is a v0.9.2 follow-up: when bosun_spawn
// creates a child worktree, the parent repo's `.claude/settings.json`
// does not automatically travel into the new sibling directory.
// Operators who want auto-claim inside spawned sub-sessions today
// must `bosun hook install` from each spawned worktree by hand, or
// commit `.claude/settings.json` so the sub-worktree inherits it
// through git. See docs/v0.9.1-hook-spec.md "Open questions" for the
// design discussion.
package claudehook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
)

// preToolUseInput is the subset of Claude Code's PreToolUse JSON
// payload bosun needs. Unknown fields are ignored on parse so a
// future Claude Code schema bump that adds keys won't crash the hook.
type preToolUseInput struct {
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

// HandleOptions lets tests inject the worktree resolver, the claim
// writer, and the config loader. Production callers leave it nil to
// get the defaults (git-backed resolve + on-disk claims store +
// `config.Load`).
type HandleOptions struct {
	// Resolve returns the (mainRepoRoot, worktreeRoot) pair for the
	// directory the agent ran the tool from. Default uses a `git`
	// subprocess via gitRevParse below.
	Resolve func(cwd string) (mainRoot, worktreeRoot string, err error)
	// Claim writes the per-session claim. Default writes through
	// internal/claims atomically. Tests stub this to assert the call
	// without touching disk.
	Claim func(mainRepoRoot, sessionLabel string, paths []string) error
	// LoadConfig returns the resolved bosun config for the main repo
	// root. Default reads `<root>/.bosun/config.json` via
	// internal/config and falls back to documented defaults when the
	// file is absent.
	LoadConfig func(mainRepoRoot string) (config.Config, error)
}

// HandlePreToolUse is the hook entry point. Always returns nil — the
// surface area exits 0 unconditionally so Claude Code never blocks a
// tool call because of a bosun-side problem. Errors are surfaced via
// stderr (`.bosun/hook.log` is a v0.9.x follow-up; see
// docs/v0.9.1-hook-spec.md "Open questions").
func HandlePreToolUse(r io.Reader, stderr io.Writer, opts HandleOptions) error {
	if stderr == nil {
		stderr = os.Stderr
	}
	if opts.Resolve == nil {
		opts.Resolve = defaultResolve
	}
	if opts.Claim == nil {
		opts.Claim = defaultClaim
	}
	if opts.LoadConfig == nil {
		opts.LoadConfig = config.Load
	}

	var in preToolUseInput
	dec := json.NewDecoder(r)
	if err := dec.Decode(&in); err != nil {
		_, _ = fmt.Fprintf(stderr, "bosun: hook: parse error: %v\n", err)
		return nil
	}

	path := extractPath(in.ToolName, in.ToolInput)
	if path == "" {
		// Either an unknown tool name or a payload missing the path
		// field bosun expected. Silent exit-0 — the agent's call
		// proceeds; we just don't record a claim.
		return nil
	}

	cwd := strings.TrimSpace(in.CWD)
	if cwd == "" {
		// Without a cwd we can't resolve which worktree owns the
		// edit. Quiet no-op rather than guessing.
		return nil
	}

	// Fast-path: if no ancestor of cwd contains a `.bosun/`
	// directory, the agent is editing outside any bosun-managed tree.
	// Skip the git invocation entirely — sub-millisecond stat-only
	// rejection for the common "not a bosun repo" case.
	if !hasBosunAncestor(cwd) {
		return nil
	}

	mainRoot, worktreeRoot, err := opts.Resolve(cwd)
	if err != nil {
		// Best-effort: log but don't block. The most common cause is
		// "cwd was removed between the agent reading it and the hook
		// running" — out of our control.
		_, _ = fmt.Fprintf(stderr, "bosun: hook: resolve worktree: %v\n", err)
		return nil
	}
	if mainRoot == "" {
		return nil
	}

	cfg, err := opts.LoadConfig(mainRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bosun: hook: load config: %v\n", err)
		return nil
	}

	label, err := LabelFromWorktreePath(mainRoot, worktreeRoot, cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bosun: hook: couldn't resolve session label: %v\n", err)
		return nil
	}
	if label == "" {
		// Editing in the main worktree or some other sibling that
		// isn't a bosun session. Nothing to claim.
		return nil
	}

	relPath, err := relativizePath(worktreeRoot, path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bosun: hook: relativize path: %v\n", err)
		return nil
	}
	if relPath == "" {
		// Edited path lives outside the worktree (e.g. agent editing
		// a config file in the user's home dir). Not bosun's concern.
		return nil
	}

	if err := opts.Claim(mainRoot, label, []string{relPath}); err != nil {
		_, _ = fmt.Fprintf(stderr, "bosun: hook: claim write failed: %v\n", err)
	}
	return nil
}

// extractPath pulls the edited file path out of `tool_input` for the
// supported tool names. NotebookEdit uses `notebook_path`; everything
// else uses `file_path`. Returns "" for unknown tool names or when
// the expected field is missing — both signal silent exit-0 upstream.
func extractPath(toolName string, raw json.RawMessage) string {
	var key string
	switch toolName {
	case "Edit", "Write", "MultiEdit":
		key = "file_path"
	case "NotebookEdit":
		key = "notebook_path"
	default:
		return ""
	}
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	rawVal, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(rawVal, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// hasBosunAncestor walks up from cwd to decide whether the resolver
// is even worth invoking. Two structural markers count:
//
//   - `.bosun/` directly here  → bosun main worktree.
//   - `.git` as a regular file → some linked worktree; could be a
//     bosun session worktree, could be an unrelated linked worktree.
//     We can't tell from on-disk shape alone (bosun's session
//     worktrees don't carry their own `.bosun/`), so we defer to the
//     git-backed resolver, which costs ~10–30 ms but resolves
//     authoritatively.
//
// A `.git` directory without an adjacent `.bosun/` is a non-bosun
// main worktree — stop and return false rather than burn a `git`
// invocation per Edit on every non-bosun repo a developer touches.
// Bounded to maxAncestors levels so a runaway CWD never costs more
// than the bounded stat count.
func hasBosunAncestor(cwd string) bool {
	const maxAncestors = 32
	dir := cwd
	for i := 0; i < maxAncestors; i++ {
		if info, err := os.Stat(filepath.Join(dir, ".bosun")); err == nil && info.IsDir() {
			return true
		}
		if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return !info.IsDir()
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
	return false
}

// relativizePath returns path expressed relative to worktreeRoot.
// Absolute paths outside the worktree return "" — we don't claim
// across worktree boundaries. Relative inputs are joined onto the
// worktree before the rel-check so the agent's "edit
// internal/foo.go"-style relative paths still resolve.
//
// macOS routinely surfaces the same directory under both `/tmp/...`
// and `/private/tmp/...` (and `/var/...` vs `/private/var/...`)
// because of the well-known root-level symlinks. The agent's
// `cwd`/`file_path` may use one form while `git rev-parse` returns
// the other, so we resolve both sides through filepath.EvalSymlinks
// before comparing. Without that, the rel-check spuriously decides
// the file is "outside the worktree" and we silently drop the claim.
func relativizePath(worktreeRoot, path string) (string, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(worktreeRoot, abs)
	}
	abs = filepath.Clean(abs)
	root := filepath.Clean(worktreeRoot)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else {
		// The file may not exist yet (Write creates new files). Walk
		// up to the first ancestor that does exist, resolve that,
		// and re-glue the unresolved tail. This keeps the symlink
		// canonicalization meaningful for not-yet-created files.
		abs = resolveExistingAncestor(abs)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}

// resolveExistingAncestor walks up p until it finds a directory that
// exists, EvalSymlinks-resolves it, and re-joins the previously
// missing tail. Returns p unchanged when no ancestor exists (the
// caller will see a "outside worktree" result, which matches our
// silent-no-op contract).
func resolveExistingAncestor(p string) string {
	tail := ""
	cur := p
	for {
		if _, err := os.Stat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return p
			}
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// defaultResolve resolves (mainRepoRoot, worktreeRoot) from cwd by
// asking git twice: `rev-parse --show-toplevel` for the current
// worktree, then `rev-parse --git-common-dir` for the main repo's
// .git location. Two ~10 ms invocations beat the alternative
// (parsing `.git` files by hand) for correctness, which matters more
// than the perf delta inside the 5 s hook budget.
func defaultResolve(cwd string) (mainRoot, worktreeRoot string, err error) {
	worktreeRoot, err = gitRevParse(cwd, "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	commonDir, err := gitRevParse(cwd, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	mainRoot = filepath.Dir(commonDir)
	return mainRoot, worktreeRoot, nil
}

// defaultClaim writes the claim to disk through internal/claims. The
// store is cross-process safe via flock so concurrent hook
// invocations don't tear the JSON file.
func defaultClaim(mainRepoRoot, sessionLabel string, paths []string) error {
	store := claims.NewStore(mainRepoRoot)
	return store.Add(sessionLabel, paths)
}
