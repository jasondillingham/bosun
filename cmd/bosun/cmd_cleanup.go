package main

import (
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newCleanupCmd() *cobra.Command {
	var (
		dryRun bool
		force  bool
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Batch-remove DONE or empty sessions",
		Long: `Remove every bosun-managed session that is either marked DONE or has no
work in it (no commits ahead of base, no uncommitted changes). Sessions with
in-flight work are skipped with a reason.

Pass --force to also remove dirty or unmerged sessions; pass --dry-run to
print what would happen without changing anything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCleanup(cmd, cleanupOpts{dryRun: dryRun, force: force})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen, don't act")
	cmd.Flags().BoolVar(&force, "force", false, "also remove dirty/unmerged sessions")

	return cmd
}

type cleanupOpts struct {
	dryRun bool
	force  bool
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

func planCleanup(sessions []session.Session, opts cleanupOpts) []cleanupPlan {
	plans := make([]cleanupPlan, 0, len(sessions))
	for i := range sessions {
		s := &sessions[i]
		switch {
		case s.State == session.StateDone:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "DONE"})
		case s.State == session.StateWorking && s.Ahead == 0 && s.Dirty == 0:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "empty (no commits, no changes)"})
		case opts.force:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "force-remove (" + describeWork(s) + ")"})
		default:
			plans = append(plans, cleanupPlan{s: s, action: cleanupSkip, reason: describeWork(s)})
		}
	}
	return plans
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

	plans := planCleanup(sessions, opts)

	removed, skipped := 0, 0
	for _, p := range plans {
		if p.action == cleanupSkip {
			skipped++
			printf("  ⏭ %s: skipped — %s\n", p.s.Name, p.reason)
			continue
		}
		if opts.dryRun {
			removed++
			printf("  ▸ %s: would remove — %s\n", p.s.Name, p.reason)
			continue
		}
		// Worktree force needed if it has uncommitted changes or the user passed --force.
		forceWT := opts.force || p.s.Dirty > 0
		// Branch force needed when commits aren't on base — DONE sessions and
		// any --force removal qualify. Squash-merged branches still appear
		// "unmerged" to git, so a plain -d would fail.
		forceBranch := opts.force || p.s.Ahead > 0 || p.s.State == session.StateDone
		if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, p.s.Path, forceWT); err != nil {
			return gitErr("remove worktree "+p.s.Path, err)
		}
		if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, p.s.Branch, forceBranch); err != nil {
			return gitErr("delete branch "+p.s.Branch, err)
		}
		_ = rc.claims.Clear(p.s.Name)
		_ = rc.state.Clear(p.s.Name)
		removed++
		printf("  ✓ %s: removed — %s\n", p.s.Name, p.reason)
	}

	if opts.dryRun {
		printf("\nbosun: dry-run — would remove %d, skip %d (no changes made)\n", removed, skipped)
	} else {
		printf("\nbosun: removed %d, skipped %d\n", removed, skipped)
	}
	return nil
}
