package main

import (
	"github.com/spf13/cobra"
)

// version is the build-time version string. Plain `go build ./...` leaves it
// as "dev"; release builds override it via -ldflags "-X main.version=…".
var version = "dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bosun",
		Short: "Coordinate parallel coding-agent sessions on isolated git worktrees",
		Long: `Bosun runs a fleet of parallel coding-agent sessions, each on its own git
worktree, with one place to see what every session is doing.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddGroup(
		&cobra.Group{ID: "setup", Title: "Setup:"},
		&cobra.Group{ID: "during", Title: "During a round:"},
		&cobra.Group{ID: "finishing", Title: "Finishing a round:"},
		&cobra.Group{ID: "wiring", Title: "Wiring + advanced:"},
	)

	root.AddCommand(
		newInitCmd(),
		newLaunchCmd(),
		newStatusCmd(),
		newShowCmd(),
		newClaimCmd(),
		newDoneCmd(),
		newMergeCmd(),
		newRemoveCmd(),
		newCleanupCmd(),
		newRescueCmd(),
		newListCmd(),
		newConfigCmd(),
		newMcpCmd(),
		newTuiCmd(),
		newServeCmd(),
		newSuggestCmd(),
		newPredictCmd(),
		newDoctorCmd(),
		newAdoptCmd(),
		newAttachCmd(),
		newHookCmd(),
		newTourCmd(),
		newNewBriefCmd(),
	)

	return root
}
