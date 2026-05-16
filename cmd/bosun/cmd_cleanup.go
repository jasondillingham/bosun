package main

import (
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newCleanupCmd() *cobra.Command {
	var (
		dryRun     bool
		force      bool
		orphansArg int
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
configured default_session_count.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := cleanupOpts{dryRun: dryRun, force: force}
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
// recent `bosun init`. Caller-supplied keep < 0 is treated as a usage
// error upstream; here we trust it's non-negative.
func filterOrphans(sessions []session.Session, keep int) []session.Session {
	out := make([]session.Session, 0, len(sessions))
	for _, s := range sessions {
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

func runCleanup(cmd *cobra.Command, opts cleanupOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	if len(sessions) == 0 {
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
		if len(sessions) == 0 {
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
		// Bosun's planCleanup already validated this session is safe to
		// remove (DONE / empty / squash-merged / --force). Bypass git's own
		// safety gates so untracked bosun metadata (BOSUN_BRIEF.md,
		// .claude/CLAUDE.md) and patch-id-equivalent branches don't block
		// the removal.
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
		removed++
		printf("  ✓ %s: removed (%s)\n", p.s.Name, p.reason)
	}

	if opts.dryRun {
		printf("\nbosun: dry-run — would remove %d, skip %d (no changes made)\n", removed, skipped)
	} else {
		printf("\nbosun: removed %d, skipped %d\n", removed, skipped)
	}
	return nil
}
