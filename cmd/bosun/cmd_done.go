package main

import (
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newDoneCmd() *cobra.Command {
	var (
		message string
		force   bool
		stuck   bool
	)

	cmd := &cobra.Command{
		Use:   "done <session>",
		Short: "Mark a session ready to merge",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDone(cmd, args[0], doneOpts{message: message, force: force, stuck: stuck})
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "", "optional message stored with the marker")
	cmd.Flags().BoolVar(&force, "force", false, "mark done even if dirty or no commits ahead")
	cmd.Flags().BoolVar(&stuck, "stuck", false, "mark STUCK instead of DONE")

	return cmd
}

type doneOpts struct {
	message string
	force   bool
	stuck   bool
}

func runDone(cmd *cobra.Command, sessionArg string, opts doneOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	n, err := session.ParseName(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}
	name := rc.cfg.SessionName(n)

	// Locate the session to validate dirty/ahead.
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

	if opts.stuck {
		if err := rc.state.MarkStuck(name, opts.message); err != nil {
			return internalErr("mark stuck", err)
		}
		printf("bosun: %s marked STUCK\n", name)
		return nil
	}

	if !opts.force {
		if s.Dirty > 0 {
			return userErr("%s has %d uncommitted change(s); commit them first or pass --force", name, s.Dirty)
		}
		if s.Ahead == 0 {
			return userErr("%s has no commits ahead of %s; use `bosun remove` instead, or pass --force", name, rc.cfg.BaseBranch)
		}
	}

	if err := rc.state.MarkDone(name, opts.message); err != nil {
		return internalErr("mark done", err)
	}
	printf("bosun: %s marked DONE (%d commits ready)\n", name, s.Ahead)
	return nil
}
