package main

import (
	"strconv"

	"github.com/jasondillingham/bosun/internal/hooks"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

// Register `pre-remove` here rather than in internal/hooks so each
// lifecycle owner declares its own event names alongside the call-site
// that fires them — keeps the hooks package free of churn while v0.5
// rounds out per-command events in parallel branches.
func init() {
	hooks.KnownEvents = append(hooks.KnownEvents, "pre-remove")
}

func newRemoveCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "remove <session>",
		Short: "Tear down a session's worktree + branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemove(cmd, args[0], force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "remove even if dirty or unmerged")

	return cmd
}

func runRemove(cmd *cobra.Command, sessionArg string, force bool) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	label, err := session.ParseLabel(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}

	// Drop admin metadata for worktrees whose directory was manually
	// removed before deriving sessions — otherwise Derive (which now
	// skips prunable entries) would report this session as missing
	// and the leftover branch + state would linger.
	if err := rc.git.PruneWorktrees(rc.ctx, rc.repoRoot); err != nil {
		return gitErr("prune worktrees", err)
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	s := findSessionByLabel(sessions, label)
	if s == nil {
		// Session has no worktree, but its branch may still exist
		// (e.g. operator rm -rf'd the dir, prune cleaned admin files,
		// branch is the only thing left). Honor the remove request by
		// deleting that branch and any state/claims for the label.
		branch := rc.cfg.BranchForLabel(label)
		exists, berr := rc.git.BranchExists(rc.ctx, rc.repoRoot, branch)
		if berr != nil {
			return gitErr("check branch "+branch, berr)
		}
		if !exists {
			return userErr("%s not found", label)
		}
		if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, branch, true); err != nil {
			return gitErr("delete branch "+branch, err)
		}
		_ = rc.claims.Clear(label)
		_ = rc.state.Clear(label)
		printf("bosun: removed %s (worktree dir was already gone; cleaned up branch + state)\n", label)
		return nil
	}

	// destructive controls whether we let git's own safety checks (`branch -d`,
	// `worktree remove` without --force) gate the operation. We bypass them when
	// the user passed --force, OR when patch-id analysis says the branch's
	// content is already on base (e.g. after `bosun merge` squashed it).
	destructive := force
	if !force {
		if s.Dirty > 0 {
			return userErr("%s has %d uncommitted change(s); commit or stash, or pass --force", label, s.Dirty)
		}
		if s.Ahead > 0 {
			unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, s.Branch)
			if err != nil {
				return gitErr("check unmerged patches for "+s.Branch, err)
			}
			if unmerged > 0 {
				return userErr("%s has %d commit(s) ahead of %s that aren't merged; pass --force to discard", label, unmerged, rc.cfg.BaseBranch)
			}
			// All ahead-commits are patch-equivalent to base — squash-merged.
			// git itself won't accept `branch -d` here because the tip SHA isn't
			// reachable from base, so escalate to force for the git calls only.
			destructive = true
		}
	}

	// Fire pre-remove BEFORE any destructive op so a hook with
	// `fail_open: false` can snapshot the worktree (or veto entirely)
	// while it still exists on disk. The orphan-branch fast-path above
	// skips this hook by design: there's no worktree to snapshot and
	// the safety-check env vars (ahead/dirty) aren't meaningful.
	hookEnv := map[string]string{
		"BOSUN_REPO_ROOT":     rc.repoRoot,
		"BOSUN_SESSION":       label,
		"BOSUN_BRANCH":        s.Branch,
		"BOSUN_WORKTREE_PATH": s.Path,
		"BOSUN_AHEAD":         strconv.Itoa(s.Ahead),
		"BOSUN_DIRTY":         strconv.Itoa(s.Dirty),
	}
	if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "pre-remove", hookEnv); err != nil {
		return userErr("%v", err)
	}

	if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, s.Path, destructive); err != nil {
		return gitErr("remove worktree "+s.Path, err)
	}
	if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, s.Branch, destructive); err != nil {
		return gitErr("delete branch "+s.Branch, err)
	}
	_ = rc.claims.Clear(label)
	_ = rc.state.Clear(label)

	printf("bosun: removed %s (worktree + branch + state)\n", label)
	return nil
}
