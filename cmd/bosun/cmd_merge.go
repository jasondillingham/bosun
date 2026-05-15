package main

import (
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newMergeCmd() *cobra.Command {
	var (
		all      bool
		noSquash bool
		message  string
	)

	cmd := &cobra.Command{
		Use:   "merge [<session>...]",
		Short: "Squash-merge sessions back to the base branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMerge(cmd, args, mergeOpts{
				all:      all,
				noSquash: noSquash,
				message:  message,
			})
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "attempt every session, not just DONE")
	cmd.Flags().BoolVar(&noSquash, "no-squash", false, "use --no-ff merges instead of --squash")
	cmd.Flags().StringVarP(&message, "message", "m", "", "override the commit message")

	return cmd
}

type mergeOpts struct {
	all      bool
	noSquash bool
	message  string
}

func runMerge(cmd *cobra.Command, args []string, opts mergeOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	// Refuse if HEAD isn't on the base branch.
	currentBranch, err := rc.git.CurrentBranch(rc.ctx, rc.repoRoot)
	if err != nil {
		return gitErr("read current branch", err)
	}
	if currentBranch != rc.cfg.BaseBranch {
		return userErr("merge must run on base branch %q (HEAD is on %q)", rc.cfg.BaseBranch, currentBranch)
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	// Build the candidate list.
	var requested map[int]bool
	if len(args) > 0 {
		requested = map[int]bool{}
		for _, a := range args {
			n, err := session.ParseName(a)
			if err != nil {
				return userErr("%v", err)
			}
			requested[n] = true
		}
	}

	type result struct {
		name   string
		status string // "merged", "skipped", "conflict"
		reason string
	}
	var results []result
	conflictHit := false

	for _, s := range sessions {
		if requested != nil && !requested[s.Number] {
			continue
		}
		if !opts.all && requested == nil && s.State != session.StateDone {
			results = append(results, result{name: s.Name, status: "skipped", reason: "not marked DONE (use --all to override)"})
			continue
		}
		if s.Dirty > 0 {
			results = append(results, result{name: s.Name, status: "skipped", reason: fmt.Sprintf("%d uncommitted change(s)", s.Dirty)})
			continue
		}
		if s.Ahead == 0 {
			results = append(results, result{name: s.Name, status: "skipped", reason: "no commits ahead"})
			continue
		}

		commitMsg := opts.message
		if commitMsg == "" {
			commitMsg = fmt.Sprintf("merge: %s", s.Branch)
		}

		if opts.noSquash {
			if err := rc.git.MergeNoFF(rc.ctx, rc.repoRoot, s.Branch, commitMsg); err != nil {
				if isMergeConflict(err) {
					conflictHit = true
					results = append(results, result{name: s.Name, status: "conflict", reason: "merge conflict — resolve manually then commit"})
					break
				}
				return gitErr("merge --no-ff "+s.Branch, err)
			}
		} else {
			if err := rc.git.MergeSquash(rc.ctx, rc.repoRoot, s.Branch); err != nil {
				if isMergeConflict(err) {
					conflictHit = true
					results = append(results, result{name: s.Name, status: "conflict", reason: "merge conflict — resolve manually then commit"})
					break
				}
				return gitErr("merge --squash "+s.Branch, err)
			}
			if err := rc.git.Commit(rc.ctx, rc.repoRoot, commitMsg); err != nil {
				return gitErr("commit merged squash", err)
			}
		}

		// On clean merge, clear the session's state + claims.
		_ = rc.state.Clear(s.Name)
		_ = rc.claims.Clear(s.Name)

		results = append(results, result{name: s.Name, status: "merged", reason: fmt.Sprintf("%d commit(s) squashed", s.Ahead)})
	}

	// Print summary.
	for _, r := range results {
		mark := "✓"
		if r.status == "skipped" {
			mark = "⏭"
		} else if r.status == "conflict" {
			mark = "✗"
		}
		printf("  %s %s: %s — %s\n", mark, r.name, r.status, r.reason)
	}
	if conflictHit {
		println("\nbosun: stopped at first conflict. Resolve, commit, then re-run `bosun merge`.")
	}
	return nil
}

// isMergeConflict heuristically detects whether an err message indicates a merge conflict.
// git returns non-zero exit + a message containing "CONFLICT" or "Automatic merge failed".
func isMergeConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") || strings.Contains(msg, "automatic merge failed")
}

