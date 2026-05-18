package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
)

// CheckLegacyWorktrees surfaces sibling worktree directories still on the
// pre-v0.11 `<repo>-bosun-<sub>` naming shape so the operator knows to run
// `bosun migrate`. Read-only — does NOT auto-rename via `--fix` because
// the rename is a real `git worktree move` and operators should opt in
// deliberately. Once they do, the new-shape `<repo>-bosun-<timestamp>-<sub>`
// dirs are what every fresh init produces.
//
// Detection scopes to worktrees git is actively tracking — orphan
// directories with the legacy shape are already handled by
// CheckOrphanWorktrees, and reporting them twice would be noise. The
// session.IsLegacyWorktreePath helper is the single source of truth for
// "what counts as legacy" so `bosun migrate` and this check can't drift.
func CheckLegacyWorktrees(ctx context.Context, repoRoot string) Result {
	registered, err := listWorktrees(ctx, repoRoot)
	if err != nil {
		return Result{
			Name:    "legacy-worktrees",
			Status:  Warn,
			Message: fmt.Sprintf("could not list registered worktrees: %v", err),
			Fix:     "fix the underlying git error then re-run doctor",
		}
	}

	var legacy []string
	for _, p := range registered {
		clean := filepath.Clean(p)
		if clean == filepath.Clean(repoRoot) {
			continue
		}
		if session.IsLegacyWorktreePath(repoRoot, clean) {
			legacy = append(legacy, clean)
		}
	}

	if len(legacy) == 0 {
		return Result{
			Name:    "legacy-worktrees",
			Status:  Pass,
			Message: "no legacy-named worktrees",
		}
	}

	// Sort for deterministic output — the operator may grep doctor logs
	// across runs and inconsistent ordering produces noise.
	sort.Strings(legacy)
	shown := legacy
	suffix := ""
	if len(shown) > 3 {
		shown = shown[:3]
		suffix = fmt.Sprintf(" (and %d more)", len(legacy)-3)
	}
	return Result{
		Name:    "legacy-worktrees",
		Status:  Warn,
		Message: fmt.Sprintf("%d legacy-named worktree(s): %s%s", len(legacy), strings.Join(shown, ", "), suffix),
		Fix:     "run `bosun migrate` to rename them to the new shape",
	}
}
