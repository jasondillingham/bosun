package main

import (
	"os"

	"github.com/jasondillingham/bosun/internal/doctor"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var (
		fix    bool
		dryRun bool
	)

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

Exit code (no --fix):
  0  all checks passed
  1  one or more warnings (operator may proceed at their own risk)
  2  one or more failures (bosun won't work cleanly until addressed)

Exit code (with --fix):
  0  all fixable issues remediated cleanly
  1  some fixers errored (see output)
  2  unfixable failures remain (e.g. git not on PATH; doctor can't
     reinstall it)

--fix auto-remediates the safe-to-touch issues: stale init.lock,
phantom branch refs, orphan worktree dirs. Issues like iCloud-managed
repository paths or missing git binary need operator intervention
and are reported but not auto-fixed.

--dry-run paired with --fix previews the fixes without executing
anything; useful before running --fix unsupervised.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := loadCtx()
			if err != nil {
				return err
			}
			results := doctor.Run(rc.ctx, rc.repoRoot, doctor.DefaultChecks())
			doctor.WriteReport(os.Stdout, rc.repoRoot, results)

			if fix {
				outcomes := doctor.ApplyFixes(rc.repoRoot, results, dryRun)
				if len(outcomes) > 0 {
					os.Stdout.WriteString("\n")
					doctor.WriteFixReport(os.Stdout, outcomes)
				}
				// Exit non-zero if any fix errored, regardless of unfixable
				// warnings/fails (operator already saw those above).
				for _, oc := range outcomes {
					if oc.Err != nil {
						os.Exit(1)
					}
				}
				// On dry-run, treat as advisory — exit reflects the
				// pre-fix diagnosis, not the fix preview.
				if !dryRun {
					// Re-evaluate the worst status considering fixers ran.
					// For simplicity: if any unfixable Fail remains, exit 2.
					// (Tracking which Failures were addressable vs not is
					// future polish.)
					for _, r := range results {
						if r.Status == doctor.Fail && r.FixFn == nil {
							os.Exit(2)
						}
					}
				}
				return nil
			}

			switch doctor.Worst(results) {
			case doctor.Fail:
				os.Exit(2)
			case doctor.Warn:
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "auto-remediate the safe-to-touch issues (stale init.lock, phantom refs, orphan worktree dirs)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "with --fix: print what would be fixed, don't apply")
	cmd.GroupID = "setup"
	return cmd
}
