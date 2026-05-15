package main

import (
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bosun",
		Short: "Coordinate parallel coding-agent sessions on isolated git worktrees",
		Long: `Bosun runs a fleet of parallel coding-agent sessions, each on its own git
worktree, with one place to see what every session is doing.`,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(
		newInitCmd(),
		newStatusCmd(),
		newShowCmd(),
		newClaimCmd(),
		newDoneCmd(),
		newMergeCmd(),
		newRemoveCmd(),
		newCleanupCmd(),
		newListCmd(),
	)

	return root
}
