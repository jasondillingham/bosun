package main

import (
	"os"

	"github.com/jasondillingham/bosun/internal/doctor"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run a system health check before bosun goes to work",
		Long: `Doctor runs every environmental probe bosun cares about and reports
anything that would derail a new user. Examples:

  - iCloud Drive / Spotlight indexing the repo (creates phantom files)
  - Orphan worktree directories from prior cleanups
  - Stale .bosun/init.lock from a killed init
  - Git binary missing or too old (need >= 2.40)
  - .bosun/ directory not writable
  - Phantom branch refs under .git/refs/heads/bosun/
  - Unix socket bind failure (blocks MCP daemon)

Exit code:
  0  all checks passed
  1  one or more warnings (operator may proceed at their own risk)
  2  one or more failures (bosun won't work cleanly until addressed)

Doctor is read-only by default — it doesn't modify anything. A
future --fix flag will offer to auto-remediate the orphan dirs,
stale lock, phantom refs (the safe-to-touch issues).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := loadCtx()
			if err != nil {
				return err
			}
			results := doctor.Run(rc.ctx, rc.repoRoot, doctor.DefaultChecks())
			doctor.WriteReport(os.Stdout, rc.repoRoot, results)
			switch doctor.Worst(results) {
			case doctor.Fail:
				os.Exit(2)
			case doctor.Warn:
				os.Exit(1)
			}
			return nil
		},
	}
	return cmd
}
