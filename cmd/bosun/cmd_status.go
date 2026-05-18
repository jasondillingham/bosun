package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/jasondillingham/bosun/internal/subtask"
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
		noSync       bool
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
				noSync:       noSync,
			}
			if watch {
				if jsonOut {
					return userErr("--watch and --json are mutually exclusive")
				}
				if interval < 1 || interval > 60 {
					return userErr("--interval must be between 1 and 60 seconds (got %d)", interval)
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
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "skip spawn-tree ↔ git reconciliation (for inspecting divergence before bosun rewrites it)")

	cmd.GroupID = "during"
	return cmd
}

type statusOpts struct {
	withOverlaps bool
	jsonOut      bool
	noColor      bool
	summaryOnly  bool
	noTree       bool
	// noSync skips the spawntree.SyncWithGit pass that runs before
	// rendering. The sync prunes ghost entries (worktree + branch
	// both gone) so the rendered table doesn't include orphans the
	// operator can't act on. --no-sync exists so an operator
	// debugging an unexpected divergence can see the on-disk
	// spawn-tree.json before bosun rewrites it.
	noSync bool
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
	// Reconcile spawn-tree against git BEFORE deriving sessions so
	// ghost entries (worktree + branch both gone, the trial #3c shape
	// macOS / iCloud File Provider produces) don't surface as
	// confusing rows. --no-sync skips this for operators inspecting
	// divergence directly.
	if !opts.noSync {
		pruned, err := spawntree.NewStore(rc.repoRoot).SyncWithGit(rc.ctx, rc.git, rc.repoRoot)
		if err != nil {
			// Sync is advisory — a missing/torn tree or a git probe
			// hiccup shouldn't break status rendering. Surface the
			// failure to stderr so an operator who cares can see it,
			// then fall through.
			fmt.Fprintf(os.Stderr, "bosun: warning: spawn-tree sync: %v\n", err)
		} else if len(pruned) > 0 {
			fmt.Fprintf(os.Stderr, "bosun: pruned %d ghost spawn-tree entr%s (worktree + branch missing): %s\n",
				len(pruned), pluralEntries(len(pruned)), strings.Join(pruned, ", "))
		}
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	// v0.9: enrich with spawn-tree info so the renderer can group
	// parents + children. Best-effort — a missing/torn spawn-tree
	// shouldn't break status; we just fall through to the flat
	// rendering when there's no parent/child data.
	enrichWithSpawnTree(rc.repoRoot, sessions)
	enrichWithSubtasks(rc.repoRoot, sessions)

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

// pluralEntries returns "y" or "ies" so the prune line reads
// "1 entry" / "3 entries" without a separate format string per case.
func pluralEntries(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
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

// enrichWithSubtasks fills sessions[i].Subtasks from the on-disk
// sub-task registry under .bosun/subtasks/<label>/. Best-effort —
// counting failures leave the field at zero rather than breaking
// status rendering. The registry is owned by `bosun_subtask` and
// `bosun_subtask_cancel`; this is a read-only consumer.
func enrichWithSubtasks(repoRoot string, sessions []session.Session) {
	labels := make([]string, len(sessions))
	for i := range sessions {
		labels[i] = sessions[i].Label
	}
	counts, err := subtask.CountsForSessions(repoRoot, labels)
	if err != nil {
		return
	}
	for i := range sessions {
		sessions[i].Subtasks = counts[sessions[i].Label]
	}
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

// ANSI escape sequences used by --watch. Pulled out as named constants
// so the tests can assert on them without re-hardcoding the bytes.
const (
	// ansiAltScreenEnter switches to the alternate screen buffer so the
	// regular scrollback is preserved across the watch session.
	ansiAltScreenEnter = "\x1b[?1049h"
	// ansiAltScreenExit restores the regular screen buffer.
	ansiAltScreenExit = "\x1b[?1049l"
	// ansiCursorHide hides the cursor for the duration of the loop.
	ansiCursorHide = "\x1b[?25l"
	// ansiCursorShow restores the cursor.
	ansiCursorShow = "\x1b[?25h"
	// ansiCursorHome moves the cursor to row 1, column 1.
	ansiCursorHome = "\x1b[H"
	// ansiClearDown clears the screen from the cursor to the end.
	ansiClearDown = "\x1b[J"
)

// runStatusWatch wires up SIGINT handling and drives the watch loop against
// stdout. SIGINT cancels the context, the loop returns nil, and the process
// exits 0 — instead of the non-zero exit Go's default signal handling
// produces.
func runStatusWatch(opts statusOpts, interval time.Duration) error {
	if !tui.IsTTY() {
		return userErr("--watch requires a terminal; use `bosun status --json` for pipe-friendly output")
	}
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	w := os.Stdout
	fmt.Fprint(w, ansiAltScreenEnter+ansiCursorHide)
	defer fmt.Fprint(w, ansiCursorShow+ansiAltScreenExit)

	return watchStatusLoop(ctx, w, rc, opts, interval)
}

// watchStatusLoop is the testable loop body: clear screen, render, wait for
// either the next tick or ctx cancellation, repeat. Returns nil on
// cancellation (so SIGINT translates to exit 0).
//
// The alt-screen entry / cursor hide / cleanup sequences live in
// runStatusWatch — the loop itself only emits per-frame escapes so a test
// driving it with a bytes.Buffer can assert on the frame content without
// pretending to manage the alt-screen lifecycle.
func watchStatusLoop(ctx context.Context, w io.Writer, rc *runCtx, opts statusOpts, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := renderWatchFrame(w, rc, opts, interval); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// renderWatchFrame draws one frame: cursor home + clear-down, the same
// content `bosun status` would emit, then the footer hint. Factored out
// of the loop so tests can verify a single frame's output without
// driving the ticker.
func renderWatchFrame(w io.Writer, rc *runCtx, opts statusOpts, interval time.Duration) error {
	fmt.Fprint(w, ansiCursorHome+ansiClearDown)
	if err := renderStatusOnce(w, rc, opts); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nPress Ctrl-C to exit. Refreshes every %s.\n", formatRefreshInterval(interval))
	return nil
}

// formatRefreshInterval renders the per-frame footer's interval as e.g.
// "2s". Whole seconds only — the CLI flag is an int seconds value.
func formatRefreshInterval(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d/time.Second))
}
