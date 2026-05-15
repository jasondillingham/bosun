package main

import (
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

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
	n, err := session.ParseName(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}
	name := rc.cfg.SessionName(n)

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	var s *session.Session
	for i := range sessions {
		if sessions[i].Number == n {
			s = &sessions[i]
			break
		}
	}
	if s == nil {
		return userErr("%s not found", name)
	}

	if !force {
		if s.Dirty > 0 {
			return userErr("%s has %d uncommitted change(s); commit or stash, or pass --force", name, s.Dirty)
		}
		if s.Ahead > 0 {
			return userErr("%s has %d commit(s) ahead of %s that aren't merged; pass --force to discard", name, s.Ahead, rc.cfg.BaseBranch)
		}
	}

	if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, s.Path, force); err != nil {
		return gitErr("remove worktree "+s.Path, err)
	}
	if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, s.Branch, force); err != nil {
		return gitErr("delete branch "+s.Branch, err)
	}
	_ = rc.claims.Clear(name)
	_ = rc.state.Clear(name)

	printf("bosun: removed %s (worktree + branch + state)\n", name)
	return nil
}
