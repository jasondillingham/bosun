package main

import (
	"strconv"

	"github.com/jasondillingham/bosun/internal/hooks"
	"github.com/jasondillingham/bosun/internal/webhooks"
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

	cmd.GroupID = "during"
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
	label, err := session.ParseLabel(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}

	// Locate the session to validate dirty/ahead.
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	s := findSessionByLabel(sessions, label)
	if s == nil {
		return userErr("%s not found", label)
	}

	if opts.stuck {
		if err := rc.state.MarkStuck(label, opts.message); err != nil {
			return internalErr("mark stuck", err)
		}
		printf("bosun: %s marked STUCK\n", label)
		runPostDoneHook(rc, label, "stuck", s.Ahead, opts.message)
		return nil
	}

	if !opts.force {
		if err := session.ValidateDoneable(*s, rc.cfg.BaseBranch); err != nil {
			return userErr("%v", err)
		}
	}

	if err := rc.state.MarkDone(label, opts.message); err != nil {
		return internalErr("mark done", err)
	}
	printf("bosun: %s marked DONE (%d commits ready)\n", label, s.Ahead)
	runPostDoneHook(rc, label, "done", s.Ahead, opts.message)
	return nil
}

// runPostDoneHook fires post-done after the state file has been written.
// A hard hook failure surfaces as a warning rather than a non-zero exit
// because the state has already changed; aborting now would leave the
// operator with an unclear "did the mark land?" question. Hook authors
// who want to gate on done should use pre-merge once that event lands
// in v0.2.
func runPostDoneHook(rc *runCtx, label, status string, ahead int, message string) {
	env := map[string]string{
		"BOSUN_REPO_ROOT":     rc.repoRoot,
		"BOSUN_SESSION_LABEL": label,
		"BOSUN_DONE_STATUS":   status,
		"BOSUN_AHEAD_COUNT":   strconv.Itoa(ahead),
		"BOSUN_DONE_MESSAGE":  message,
	}
	if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "post-done", env); err != nil {
		printf("bosun: warning: post-done hook: %v\n", err)
	}
	// Phase 5 #64: fire-and-forget HTTP delivery alongside hooks.
	// Discarding the WaitGroup is intentional — `bosun done` must
	// return promptly, not block on a Slack p99.
	_ = webhooks.Fire(rc.ctx, rc.cfg.Webhooks, "post-done", env)
}

// findSessionByLabel returns the session whose Label matches, or nil.
// Shared by claim/done/show/merge/remove/launch for CLI lookups so they all
// agree on the canonical-label match semantics.
func findSessionByLabel(sessions []session.Session, label string) *session.Session {
	for i := range sessions {
		if sessions[i].Label == label {
			return &sessions[i]
		}
	}
	return nil
}
