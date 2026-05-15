package main

import (
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newClaimCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claim <session> <paths...>",
		Short: "Declare paths the session is currently editing (advisory)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaim(cmd, args[0], args[1:])
		},
	}
	return cmd
}

func runClaim(cmd *cobra.Command, sessionArg string, paths []string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	n, err := session.ParseName(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}
	name := rc.cfg.SessionName(n)

	if err := rc.claims.Add(name, paths); err != nil {
		return internalErr("write claims", err)
	}
	c, _ := rc.claims.Read(name)
	count := 0
	if c != nil {
		count = len(c.Paths)
	}
	printf("bosun: %s now claims %d path(s)\n", name, count)
	return nil
}
