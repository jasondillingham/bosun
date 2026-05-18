package claudehook

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
)

// LabelFromWorktreePath derives the bosun session label that owns
// worktreePath, given the main worktree's repoRoot and the configured
// worktree-suffix pattern.
//
// Returns ("", nil) when worktreePath is the main worktree itself or
// when the basename doesn't match the configured `<repo>+<pattern>`
// shape — those are legitimate "this edit doesn't belong to a bosun
// session" cases and the hook should silently no-op rather than claim
// a wrong label.
//
// Numeric session worktrees (`<repo>-bosun-3`) resolve to the
// canonical "session-3" label; named worktrees (`<repo>-bosun-auth`)
// resolve to "auth". Errors are reserved for invalid configuration
// (pattern without `{N}`); callers should fall back to silent exit-0
// in that case too.
func LabelFromWorktreePath(repoRoot, worktreePath string, cfg config.Config) (string, error) {
	if filepath.Clean(worktreePath) == filepath.Clean(repoRoot) {
		return "", nil
	}
	repoBase := filepath.Base(repoRoot)
	wtBase := filepath.Base(worktreePath)
	if !strings.HasPrefix(wtBase, repoBase) {
		return "", nil
	}
	rest := wtBase[len(repoBase):]

	pattern := cfg.WorktreeSuffixPattern
	idx := strings.Index(pattern, "{N}")
	if idx < 0 {
		return "", fmt.Errorf("worktree_suffix_pattern missing {N}: %q", pattern)
	}
	prefix := pattern[:idx]
	suffix := pattern[idx+len("{N}"):]
	if !strings.HasPrefix(rest, prefix) || !strings.HasSuffix(rest, suffix) {
		return "", nil
	}
	sub := rest[len(prefix) : len(rest)-len(suffix)]
	if sub == "" {
		return "", nil
	}
	// Bare-integer substitution is the canonical numeric form: a
	// "-bosun-3" suffix means session-3, matching cfg.SessionName(N).
	if n, err := strconv.Atoi(sub); err == nil {
		if n < 1 {
			return "", nil
		}
		return cfg.SessionName(n), nil
	}
	if err := session.ValidateLabel(sub); err != nil {
		return "", nil
	}
	return sub, nil
}
