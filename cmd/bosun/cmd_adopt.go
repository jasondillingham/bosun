package main

import (
	"fmt"
	"os"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/spf13/cobra"
)

func newAdoptCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adopt <sub-session>",
		Short: "Promote a sub-session to top-level in the spawn tree",
		Long: `Adopt clears a sub-session's parent reference in .bosun/spawn-tree.json
so it stands on its own. Useful after the operator decides:

  - "this sub is the only thing worth continuing; the parent's done"
  - "the parent crashed and won't recover; let this sub keep going"

Mechanics: the sub's worktree and branch are unchanged. Only the
spawn-tree metadata is rewritten — Parent goes empty, Depth goes 0,
the previous parent's children list drops the entry. Subsequent
` + "`bosun merge`" + ` against the sub is no longer gated by "merge --tree
the parent first" since the sub has no parent anymore.

The sub's branch keeps its current base (which was the parent's
HEAD at spawn time). If the operator wants the sub re-based on
main, that's a separate ` + "`git rebase`" + ` step.

This is a read-modify-write on .bosun/spawn-tree.json; safe under
concurrent cleanup via the lockfile.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := loadCtx()
			if err != nil {
				return err
			}
			label, err := session.ParseLabel(args[0])
			if err != nil {
				return err
			}
			tree := spawntree.NewStore(rc.repoRoot)
			parent, err := tree.ParentOf(label)
			if err != nil {
				return internalErr("read spawn tree", err)
			}
			if parent == "" {
				return userErr("%s is already top-level (no parent recorded in spawn tree)", label)
			}
			if err := tree.Adopt(label); err != nil {
				return internalErr("adopt session", err)
			}
			fmt.Fprintf(os.Stdout, "bosun: adopted %s (was a sub-session of %s; now top-level)\n", label, parent)
			return nil
		},
	}
	return cmd
}
