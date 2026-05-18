package main

import (
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is the build-time version string. Plain `go build ./...` leaves it
// as "dev"; release builds override it via -ldflags "-X main.version=…".
// resolvedVersion() applies a `go install ...@vX.Y.Z` fallback so users on
// the recommended-by-README install path don't see "dev".
var version = "dev"

// resolvedVersion returns the version string surfaced to `bosun --version`.
// Priority order:
//
//  1. The ldflags-injected `version` var, when something other than the
//     default "dev". `make build`, the release GitHub Action, and any
//     hand-rolled `go build -ldflags="-X main.version=vX.Y.Z" ./cmd/bosun`
//     all land here.
//  2. The Go module version from runtime/debug.BuildInfo. A user who runs
//     `go install github.com/jasondillingham/bosun/cmd/bosun@v0.11.1`
//     has Go fill `Main.Version` with the tag — this lets us report
//     "v0.11.1" without an ldflags step in the install path.
//  3. The literal "dev" fallback. Covers local `go build ./...`,
//     `go install ./cmd/bosun` from a clone (which Go represents as
//     "(devel)"), and any other path that doesn't carry a meaningful
//     version through.
//
// Issue #25 motivated this — the released README leads with
// `go install`, which never picks up the Makefile's ldflags injection.
func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bosun",
		Short: "Coordinate parallel coding-agent sessions on isolated git worktrees",
		Long: `Bosun runs a fleet of parallel coding-agent sessions, each on its own git
worktree, with one place to see what every session is doing.`,
		Version:       resolvedVersion(),
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
		newMigrateCmd(),
		newAdoptCmd(),
		newAttachCmd(),
		newHookCmd(),
		newTourCmd(),
		newNewBriefCmd(),
		newEventsCmd(),
		newDebugCmd(),
		newHistoryCmd(),
	)

	return root
}
