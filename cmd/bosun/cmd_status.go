package main

import (
	"os"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var (
		withOverlaps bool
		jsonOut      bool
		noColor      bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print a table of session states",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, statusOpts{
				withOverlaps: withOverlaps,
				jsonOut:      jsonOut,
				noColor:      noColor,
			})
		},
	}

	cmd.Flags().BoolVar(&withOverlaps, "with-overlaps", false, "include claim overlap detection")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable color even on a TTY")

	return cmd
}

type statusOpts struct {
	withOverlaps bool
	jsonOut      bool
	noColor      bool
}

func runStatus(cmd *cobra.Command, opts statusOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	var overlaps []claims.Overlap
	if opts.withOverlaps {
		overlaps, err = rc.claims.Overlaps()
		if err != nil {
			return internalErr("compute overlaps", err)
		}
	}

	if opts.jsonOut {
		if err := status.RenderJSON(os.Stdout, sessions, overlaps, opts.withOverlaps); err != nil {
			return internalErr("render json", err)
		}
		return nil
	}

	return status.RenderText(os.Stdout, status.RenderOptions{
		Sessions:     sessions,
		Overlaps:     overlaps,
		WithOverlaps: opts.withOverlaps,
		NoColor:      opts.noColor,
	})
}
