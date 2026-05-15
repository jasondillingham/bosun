package main

import (
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var ready bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print session names, one per line",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd, ready)
		},
	}

	cmd.Flags().BoolVar(&ready, "ready", false, "only print sessions marked DONE")

	return cmd
}

func runList(cmd *cobra.Command, ready bool) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	for _, s := range sessions {
		if ready && s.State != session.StateDone {
			continue
		}
		println(s.Name)
	}
	return nil
}
