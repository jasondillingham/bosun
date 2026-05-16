package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newCleanupCmd() *cobra.Command {
	var (
		dryRun     bool
		force      bool
		orphansArg int
		orphanDirs bool
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Batch-remove DONE or empty sessions",
		Long: `Remove every bosun-managed session that is either marked DONE or has no
work in it (no commits ahead of base, no uncommitted changes). Sessions with
in-flight work are skipped with a reason.

Pass --force to also remove dirty or unmerged sessions; pass --dry-run to
print what would happen without changing anything.

Pass --orphans=N to instead clean up sessions whose number is greater than
N — typical after ` + "`bosun init --force`" + ` shrinks the session count and leaves
the trailing worktrees behind. ` + "`--orphans`" + ` without a value defaults to the
configured default_session_count.

Pass --orphan-dirs to also scan the repo's parent directory for sibling
worktree directories whose git admin metadata is already gone — the shape
the v0.3 corruption left behind. Independent of --orphans (which filters by
session number).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := cleanupOpts{dryRun: dryRun, force: force, orphanDirs: orphanDirs}
			if cmd.Flags().Changed("orphans") {
				opts.orphansMode = true
				opts.orphansKeep = orphansArg
			}
			return runCleanup(cmd, opts)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen, don't act")
	cmd.Flags().BoolVar(&force, "force", false, "also remove dirty/unmerged sessions")
	cmd.Flags().IntVar(&orphansArg, "orphans", 0, "only act on sessions whose number is greater than this (0 means use config.default_session_count)")
	cmd.Flags().BoolVar(&orphanDirs, "orphan-dirs", false, "also scan parent dir for worktree directories git no longer tracks and remove them")
	// NoOptDefVal lets `--orphans` work without a value (cobra parses it as
	// `--orphans=0`), at which point runCleanup falls back to the config
	// default. The string form is what cobra requires here.
	cmd.Flags().Lookup("orphans").NoOptDefVal = "0"

	return cmd
}

type cleanupOpts struct {
	dryRun bool
	force  bool
	// orphansMode flips planCleanup into "act on sessions with number >
	// orphansKeep only" — for cleaning up the trailing worktrees that
	// linger when a later `bosun init` shrinks the session count.
	orphansMode bool
	// orphansKeep is the highest session number to keep when orphansMode
	// is set. Zero means "fall back to cfg.DefaultSessionCount in
	// runCleanup." A negative value would remove every session, which we
	// reject up front.
	orphansKeep int
	// orphanDirs additionally scans the repo's parent directory for
	// worktree-shaped directories that git's worktree list doesn't track
	// (the v0.3 corruption case: data on disk, admin metadata pruned).
	// Acts in addition to the normal session sweep.
	orphanDirs bool
}

type cleanupAction int

const (
	cleanupSkip cleanupAction = iota
	cleanupRemove
)

type cleanupPlan struct {
	s      *session.Session
	action cleanupAction
	reason string
}

// squashCheck reports whether all of branch's commits ahead of base are
// already on base by patch-id (i.e. squash-merged). Returning (true, nil)
// means the branch's content is on main even though git still shows it ahead.
type squashCheck func(branch string) (bool, error)

func planCleanup(sessions []session.Session, opts cleanupOpts, isSquashed squashCheck) ([]cleanupPlan, error) {
	plans := make([]cleanupPlan, 0, len(sessions))
	for i := range sessions {
		s := &sessions[i]
		switch {
		case s.State == session.StateDone:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "DONE"})
		case s.State == session.StateWorking && s.Ahead == 0 && s.Dirty == 0:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "empty"})
		case s.State == session.StateWorking && s.Ahead > 0 && s.Dirty == 0:
			// After `bosun merge` squashes the session, the branch tip still
			// reports `ahead=1` even though its content is on main. Treat
			// patch-equivalent branches as removable without --force.
			squashed, err := isSquashed(s.Branch)
			if err != nil {
				return nil, fmt.Errorf("check unmerged patches for %s: %w", s.Branch, err)
			}
			if squashed {
				plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "squash-merged"})
				continue
			}
			if opts.force {
				plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "force-remove, " + describeWork(s)})
				continue
			}
			plans = append(plans, cleanupPlan{s: s, action: cleanupSkip, reason: describeWork(s)})
		case opts.force:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "force-remove, " + describeWork(s)})
		default:
			plans = append(plans, cleanupPlan{s: s, action: cleanupSkip, reason: describeWork(s)})
		}
	}
	return plans, nil
}

// filterOrphans returns only those sessions whose number is greater than
// keep — the sessions that should not exist after the operator's most
// recent `bosun init N`. Named sessions (Number == 0) are passed through
// untouched: --orphans is a numeric-mode concept ("trim the trailing
// numbered sessions"), so when a named session co-exists with numbered
// ones we leave it alone unless the operator removes it explicitly.
// Caller-supplied keep < 0 is treated as a usage error upstream; here we
// trust it's non-negative.
func filterOrphans(sessions []session.Session, keep int) []session.Session {
	out := make([]session.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.Number == 0 {
			continue
		}
		if s.Number > keep {
			out = append(out, s)
		}
	}
	return out
}

func describeWork(s *session.Session) string {
	parts := make([]string, 0, 3)
	if s.Dirty > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted", s.Dirty))
	}
	if s.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d ahead", s.Ahead))
	}
	if s.State == session.StateStuck {
		parts = append(parts, "STUCK")
	}
	if len(parts) == 0 {
		return "in-progress"
	}
	return strings.Join(parts, ", ")
}

// executeCleanupOne removes one session's worktree and branch and clears
// its bosun state/claims. The caller is responsible for having decided the
// session is safe to remove (DONE / empty / squash-merged / --force) — this
// helper bypasses git's own safety gates so untracked bosun metadata
// (BOSUN_BRIEF.md, .claude/CLAUDE.md) and patch-id-equivalent branches
// don't block the removal.
func executeCleanupOne(rc *runCtx, p cleanupPlan) error {
	forceWT := true
	forceBranch := true
	if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, p.s.Path, forceWT); err != nil {
		return gitErr("remove worktree "+p.s.Path, err)
	}
	if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, p.s.Branch, forceBranch); err != nil {
		return gitErr("delete branch "+p.s.Branch, err)
	}
	_ = rc.claims.Clear(p.s.Name)
	_ = rc.state.Clear(p.s.Name)
	return nil
}

// cleanupOne plans and (unless dry-run) executes cleanup for one session.
// Used by the TUI control center's `c` keybind so it doesn't reimplement
// the policy from runCleanup. Returns the action taken and a short reason.
// err is non-nil only for unexpected git failures.
func cleanupOne(rc *runCtx, s *session.Session, opts cleanupOpts) (cleanupAction, string, error) {
	isSquashed := func(branch string) (bool, error) {
		unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, branch)
		if err != nil {
			return false, err
		}
		return unmerged == 0, nil
	}
	plans, err := planCleanup([]session.Session{*s}, opts, isSquashed)
	if err != nil {
		return cleanupSkip, "", gitErr("plan cleanup", err)
	}
	p := plans[0]
	if p.action == cleanupSkip {
		return cleanupSkip, p.reason, nil
	}
	if opts.dryRun {
		return cleanupRemove, p.reason, nil
	}
	if err := executeCleanupOne(rc, p); err != nil {
		return cleanupSkip, "", err
	}
	return cleanupRemove, p.reason, nil
}

func runCleanup(cmd *cobra.Command, opts cleanupOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	// Drop git metadata for worktrees whose directory has been manually
	// removed (e.g. `rm -rf` on a session dir). Without this, Derive
	// would skip the stale entries (good for status) but the on-disk
	// admin files would linger until the user notices. Doing it here
	// keeps cleanup's promise of "nothing left behind."
	if err := rc.git.PruneWorktrees(rc.ctx, rc.repoRoot); err != nil {
		return gitErr("prune worktrees", err)
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	if len(sessions) == 0 && !opts.orphanDirs {
		println("bosun: no sessions to clean up")
		return nil
	}

	if opts.orphansMode {
		keep := opts.orphansKeep
		if keep == 0 {
			keep = rc.cfg.DefaultSessionCount
		}
		if keep < 0 {
			return userErr("--orphans must be >= 0, got %d", keep)
		}
		sessions = filterOrphans(sessions, keep)
		if len(sessions) == 0 && !opts.orphanDirs {
			printf("bosun: no sessions beyond session-%d to clean up\n", keep)
			return nil
		}
		// Orphan candidates with no work are removed cleanly; ones with
		// ahead/dirty/STUCK state stay subject to the same --force gate the
		// regular cleanup uses. The operator can re-run with --force to
		// nuke them if they really want.
	}

	plans, err := planCleanup(sessions, opts, func(branch string) (bool, error) {
		unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, branch)
		if err != nil {
			return false, err
		}
		return unmerged == 0, nil
	})
	if err != nil {
		return gitErr("plan cleanup", err)
	}

	removed, skipped := 0, 0
	for _, p := range plans {
		if p.action == cleanupSkip {
			skipped++
			printf("  ⏭ %s: skipped — %s\n", p.s.Name, p.reason)
			continue
		}
		if opts.dryRun {
			removed++
			printf("  ▸ %s: would remove (%s)\n", p.s.Name, p.reason)
			continue
		}
		if err := executeCleanupOne(rc, p); err != nil {
			return err
		}
		removed++
		printf("  ✓ %s: removed (%s)\n", p.s.Name, p.reason)
	}

	if opts.orphanDirs {
		r, s, err := sweepOrphanDirs(rc, opts.dryRun)
		if err != nil {
			return err
		}
		removed += r
		skipped += s
	}

	if opts.dryRun {
		printf("\nbosun: dry-run — would remove %d, skip %d (no changes made)\n", removed, skipped)
	} else {
		printf("\nbosun: removed %d, skipped %d\n", removed, skipped)
	}
	return nil
}

// sweepOrphanDirs implements the --orphan-dirs path: scan the repo's
// parent directory for sibling dirs matching cfg.WorktreeSuffixPattern,
// drop any that git's worktree list still tracks, then for each
// remaining candidate either remove it (chmod + RemoveAll) or skip it
// with a "looks like a live worktree" notice when the dir still
// carries a `.git` file pointing at the main repo. Returns the
// removed/skipped counts so runCleanup can fold them into the summary.
func sweepOrphanDirs(rc *runCtx, dryRun bool) (removed, skipped int, err error) {
	candidates, err := git.ScanOrphanDirs(rc.repoRoot, rc.cfg.WorktreeSuffixPattern)
	if err != nil {
		return 0, 0, gitErr("scan orphan dirs", err)
	}
	if len(candidates) == 0 {
		return 0, 0, nil
	}

	worktrees, err := rc.git.ListWorktrees(rc.ctx, rc.repoRoot)
	if err != nil {
		return 0, 0, gitErr("list worktrees", err)
	}
	tracked := make(map[string]bool, len(worktrees))
	for _, w := range worktrees {
		tracked[w.Path] = true
	}

	for _, dir := range candidates {
		if tracked[dir] {
			// git still considers it a real worktree — handled by the
			// normal session sweep, not here.
			continue
		}
		display := filepath.Base(dir)
		if looksLikeLiveWorktree(dir) {
			skipped++
			printf("  ⏭ %s: skipped (looks like a live worktree; run `git worktree prune` first)\n", display)
			continue
		}
		if dryRun {
			removed++
			printf("  ▸ %s: would remove (orphan dir)\n", display)
			continue
		}
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			_ = git.ChmodWritableTree(dir)
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return removed, skipped, fmt.Errorf("remove orphan dir %s: %w", dir, rmErr)
		}
		removed++
		printf("  ✓ %s: removed (orphan dir)\n", display)
	}
	return removed, skipped, nil
}

// looksLikeLiveWorktree reports whether dir has a `.git` *file* (not
// directory) whose content begins with `gitdir:` — the on-disk shape of
// a linked worktree's pointer back to the main repo's .git admin tree.
// True means we should refuse to remove it: the safer course is to ask
// the operator to run `git worktree prune` (or `repair`) first so git's
// view of the tree matches the disk.
func looksLikeLiveWorktree(dir string) bool {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir:")
}
