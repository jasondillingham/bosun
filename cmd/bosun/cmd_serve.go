package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jasondillingham/bosun/internal/web"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var (
		port     int
		bind     string
		interval int
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run an HTTP dashboard server (experimental, v0.3)",
		Long: `Run a long-lived HTTP server that exposes the same data the TUI shows
over JSON (/api/status) and Server-Sent Events (/api/events). A minimal
HTML page at / consumes both, giving a browser-based view of the fleet.

Defaults to binding 127.0.0.1 — there is no authentication, so binding to
a non-loopback address exposes the dashboard to anyone on that network at
your own risk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if port < 0 || port > 65535 {
				return userErr("--port must be 0..65535")
			}
			if interval < 1 {
				return userErr("--interval must be >= 1 (seconds)")
			}
			if bind == "" {
				return userErr("--bind must not be empty")
			}
			return runServe(bind, port, time.Duration(interval)*time.Second)
		},
	}

	cmd.Flags().IntVar(&port, "port", 8765, "TCP port to bind")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "address to bind (non-loopback exposes the dashboard with no auth)")
	cmd.Flags().IntVar(&interval, "interval", 2, "seconds between /api/status recomputes")

	cmd.GroupID = "wiring"
	return cmd
}

func runServe(bind string, port int, interval time.Duration) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	srv := web.New(web.Config{
		RepoRoot: rc.repoRoot,
		Git:      rc.git,
		Cfg:      rc.cfg,
		Claims:   rc.claims,
		State:    rc.state,
		Bind:     bind,
		Port:     port,
		Interval: interval,
	})

	fmt.Fprintf(os.Stdout, "bosun serve: listening on http://%s:%d\n", bind, port)
	fmt.Fprintln(os.Stdout, "bosun serve: ^C to stop")

	ctx, cancel := signal.NotifyContext(rc.ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		return internalErr("serve", err)
	}
	fmt.Fprintln(os.Stdout, "\nbosun serve: stopped")
	return nil
}
