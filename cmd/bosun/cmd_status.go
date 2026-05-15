package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/jasondillingham/bosun/internal/tui"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var (
		withOverlaps bool
		jsonOut      bool
		noColor      bool
		watch        bool
		interval     int
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print a table of session states",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := statusOpts{
				withOverlaps: withOverlaps,
				jsonOut:      jsonOut,
				noColor:      noColor,
			}
			if watch {
				if jsonOut {
					return userErr("--watch and --json are mutually exclusive")
				}
				if interval < 1 {
					return userErr("--interval must be >= 1 (seconds)")
				}
				return runStatusWatch(opts, time.Duration(interval)*time.Second)
			}
			return runStatus(cmd, opts)
		},
	}

	cmd.Flags().BoolVar(&withOverlaps, "with-overlaps", false, "include claim overlap detection")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable color even on a TTY")
	cmd.Flags().BoolVar(&watch, "watch", false, "re-render the table on an interval until interrupted (Ctrl-C)")
	cmd.Flags().IntVar(&interval, "interval", 2, "seconds between refreshes when --watch is set")

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
	return renderStatusOnce(os.Stdout, rc, opts)
}

// renderStatusOnce derives session state and writes one rendering (text or
// JSON) to w. Factored out of runStatus so the --watch loop can reuse it.
func renderStatusOnce(w io.Writer, rc *runCtx, opts statusOpts) error {
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
		if err := status.RenderJSON(w, sessions, overlaps, opts.withOverlaps); err != nil {
			return internalErr("render json", err)
		}
		return nil
	}

	return status.RenderText(w, status.RenderOptions{
		Sessions:     sessions,
		Overlaps:     overlaps,
		WithOverlaps: opts.withOverlaps,
		NoColor:      opts.noColor,
	})
}

// runStatusWatch wires up SIGINT handling and drives the watch loop against
// stdout. SIGINT cancels the context, the loop returns nil, and the process
// exits 0 — instead of the non-zero exit Go's default signal handling
// produces.
func runStatusWatch(opts statusOpts, interval time.Duration) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return watchStatusLoop(ctx, os.Stdout, rc, opts, interval)
}

// watchStatusLoop is the testable loop body: clear screen, render, wait for
// either the next tick or ctx cancellation, repeat. Returns nil on
// cancellation (so SIGINT translates to exit 0).
func watchStatusLoop(ctx context.Context, w io.Writer, rc *runCtx, opts statusOpts, interval time.Duration) error {
	for {
		tui.ClearScreen(w)
		if err := renderStatusOnce(w, rc, opts); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
