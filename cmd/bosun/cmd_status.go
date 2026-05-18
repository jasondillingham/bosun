package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/jasondillingham/bosun/internal/tui"
	"github.com/spf13/cobra"
)

// statusEventTailN caps how many announcements the status command tails
// from the events log. Five is enough for a glanceable feed; the operator
// can tail the JSONL file directly if they want full history.
const statusEventTailN = 5

func newStatusCmd() *cobra.Command {
	var (
		withOverlaps bool
		jsonOut      bool
		noColor      bool
		watch        bool
		interval     int
		summaryOnly  bool
		noTree       bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print a table of session states",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if summaryOnly && jsonOut {
				return userErr("--summary-only and --json are mutually exclusive")
			}
			opts := statusOpts{
				withOverlaps: withOverlaps,
				jsonOut:      jsonOut,
				noColor:      noColor,
				summaryOnly:  summaryOnly,
				noTree:       noTree,
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
	cmd.Flags().BoolVar(&summaryOnly, "summary-only", false, "print just the one-line summary (no table, events, or overlaps)")
	cmd.Flags().BoolVar(&noTree, "no-tree", false, "force the flat table even when spawn trees exist (for scripts that parse the output)")

	cmd.GroupID = "during"
	return cmd
}

type statusOpts struct {
	withOverlaps bool
	jsonOut      bool
	noColor      bool
	summaryOnly  bool
	noTree       bool
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

	// v0.9: enrich with spawn-tree info so the renderer can group
	// parents + children. Best-effort — a missing/torn spawn-tree
	// shouldn't break status; we just fall through to the flat
	// rendering when there's no parent/child data.
	enrichWithSpawnTree(rc.repoRoot, sessions)

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
		Events:       recentEvents(rc.repoRoot),
		SummaryOnly:  opts.summaryOnly,
		NoTree:       opts.noTree,
	})
}

// enrichWithSpawnTree populates Parent / Children / Depth on each
// session by looking up its label in .bosun/spawn-tree.json. Failures
// are swallowed because the tree is advisory — a missing or torn file
// shouldn't break status rendering. Callers that need the tree info
// for correctness (cleanup --tree, merge --tree) handle errors at
// their own level.
func enrichWithSpawnTree(repoRoot string, sessions []session.Session) {
	tree := spawntree.NewStore(repoRoot)
	likes := make([]spawntree.SessionLike, len(sessions))
	for i := range sessions {
		likes[i] = &sessions[i]
	}
	_ = tree.EnrichSessions(likes)
}

// recentEvents pulls the last few bosun_announce entries from
// .bosun/events.log so the renderer can surface them. Errors are swallowed
// — a missing or unreadable log shouldn't keep `bosun status` from showing
// the session table.
func recentEvents(repoRoot string) []status.Event {
	logPath := filepath.Join(repoRoot, bosunmcp.EventLogRelative)
	raw, err := bosunmcp.TailEvents(logPath, statusEventTailN)
	if err != nil || len(raw) == 0 {
		return nil
	}
	out := make([]status.Event, 0, len(raw))
	for _, e := range raw {
		out = append(out, status.Event{
			Session: e.Session,
			Kind:    e.Kind,
			Message: e.Message,
			At:      e.At,
		})
	}
	return out
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
