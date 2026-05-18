package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/state"
)

// runCtx bundles the repo-derived state that almost every command needs.
type runCtx struct {
	ctx      context.Context
	git      *git.Client
	cfg      config.Config
	repoRoot string
	claims   *claims.Store
	state    *state.Store
}

// loadCtx finds the main worktree root, reads optional config, and returns a runCtx.
// The runCtx's repoRoot is always the *main* worktree, even when bosun is invoked
// from inside a linked worktree — claims and state must live in one canonical
// place that every session can reach.
func loadCtx() (*runCtx, error) {
	ctx := context.Background()
	c := git.New()
	cwd, err := os.Getwd()
	if err != nil {
		return nil, internalErr("getwd", err)
	}
	root, err := c.MainWorktreePath(ctx, cwd)
	if err != nil {
		return nil, userErr("not inside a git repository (cwd=%s)", cwd)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, userErr("load config: %v", err)
	}
	if cfg.GitOpTimeoutSeconds > 0 {
		c.SetTimeout(time.Duration(cfg.GitOpTimeoutSeconds) * time.Second)
	}
	return &runCtx{
		ctx:      ctx,
		git:      c,
		cfg:      cfg,
		repoRoot: root,
		claims:   claims.NewStore(root),
		state:    state.NewStore(root),
	}, nil
}

// printf is a convenience that writes to stdout via fmt.Fprintf.
func printf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format, args...)
}

// println writes to stdout.
func println(s string) {
	fmt.Fprintln(os.Stdout, s)
}

// lookupWorktreePathByLabel asks git for the on-disk path of the worktree
// backing the given session label. Authoritative for both legacy
// `<repo>-bosun-N` dirs and the v0.10 UID-per-worktree
// `<repo>-bosun-<ts>-N` form because it reads `git worktree list` rather
// than reconstructing the path from the suffix pattern. Returns
// ("", false) when neither a branch-match nor a basename-pattern fallback
// produces a hit.
//
// The basename fallback is what catches the v0.6.2 corruption shape: a
// worktree whose `.git/worktrees/<name>/HEAD` was removed reports
// `branch detached` in `git worktree list`, but the dir is still on
// disk under its bosun-stamped name. Matching by basename keeps the
// upstream corruption check able to refuse on the right path instead
// of silently falling through to a generic "session not found."
func lookupWorktreePathByLabel(rc *runCtx, label string) (string, bool) {
	worktrees, err := rc.git.ListWorktrees(rc.ctx, rc.repoRoot)
	if err != nil {
		return "", false
	}
	wantBranchRef := "refs/heads/" + rc.cfg.BranchForLabel(label)
	for _, wt := range worktrees {
		if wt.Branch == wantBranchRef {
			return wt.Path, true
		}
	}
	// Branch-detached fallback. The legacy basename is built via the
	// existing suffix helper with an empty round timestamp; the
	// timestamped variants are detected by parsing the substituted-{N}
	// segment of the basename.
	repoBase := filepath.Base(rc.repoRoot)
	legacyBase := repoBase + rc.cfg.WorktreeSuffixForLabel(label, "")
	sub := label
	if rest, ok := strings.CutPrefix(label, "session-"); ok {
		sub = rest
	}
	idx := strings.Index(rc.cfg.WorktreeSuffixPattern, "{N}")
	if idx < 0 {
		return "", false
	}
	patternPrefix := repoBase + rc.cfg.WorktreeSuffixPattern[:idx]
	patternSuffix := rc.cfg.WorktreeSuffixPattern[idx+len("{N}"):]
	for _, wt := range worktrees {
		base := filepath.Base(wt.Path)
		if base == legacyBase {
			return wt.Path, true
		}
		// Scheme-C UID-per-worktree form: `<patternPrefix><ts>-<sub><patternSuffix>`.
		if !strings.HasPrefix(base, patternPrefix) || !strings.HasSuffix(base, patternSuffix) {
			continue
		}
		mid := base[len(patternPrefix) : len(base)-len(patternSuffix)]
		if len(mid) >= 17 && mid[8] == '-' && mid[15] == '-' && allDigitsExceptDash(mid[:15]) && mid[16:] == sub {
			return wt.Path, true
		}
	}
	return "", false
}

// allDigitsExceptDash reports whether s consists of ASCII digits except
// for the single dash at index 8 (the YYYYMMDD-HHMMSS shape).
func allDigitsExceptDash(s string) bool {
	if len(s) != 15 || s[8] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 {
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
