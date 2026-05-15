// Package status renders the `bosun status` table (plain text or JSON).
package status

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/tui"
)

// RenderOptions controls table rendering.
type RenderOptions struct {
	Sessions     []session.Session
	Overlaps     []claims.Overlap // nil if --with-overlaps not requested
	WithOverlaps bool
	NoColor      bool
}

// RenderText writes a human-readable table (and optional overlap section) to w.
func RenderText(w io.Writer, opts RenderOptions) error {
	useColor := tui.ShouldColor(opts.NoColor)

	if len(opts.Sessions) == 0 {
		fmt.Fprintln(w, "bosun: no sessions. Run `bosun init` to create some.")
		return nil
	}

	writeSummary(w, opts, useColor)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tBRANCH\tSTATE\tAHEAD\tDIRTY\tCLAIMED\tLAST_COMMIT")
	for _, s := range opts.Sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			s.Name,
			s.Branch,
			colorState(s.State, useColor),
			s.Ahead,
			s.Dirty,
			s.Claimed,
			formatLastCommit(s),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if opts.WithOverlaps {
		writeOverlaps(w, opts.Overlaps)
	}
	return nil
}

// writeSummary prints a one-line summary above the status table:
//   3 sessions — 1 DONE, 2 WORKING · 5 commits ahead total · 1 overlap
//
// State counts are colored when color is enabled. Only non-zero state
// buckets are listed (DONE, WORKING, STUCK order). Overlap count is only
// included when WithOverlaps is set.
func writeSummary(w io.Writer, opts RenderOptions, useColor bool) {
	var doneN, workingN, stuckN, totalAhead int
	for _, s := range opts.Sessions {
		switch s.State {
		case session.StateDone:
			doneN++
		case session.StateWorking:
			workingN++
		case session.StateStuck:
			stuckN++
		}
		totalAhead += s.Ahead
	}

	var stateParts []string
	if doneN > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d %s", doneN, colorState(session.StateDone, useColor)))
	}
	if workingN > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d %s", workingN, colorState(session.StateWorking, useColor)))
	}
	if stuckN > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d %s", stuckN, colorState(session.StateStuck, useColor)))
	}

	line := fmt.Sprintf("%d %s", len(opts.Sessions), pluralize("session", len(opts.Sessions)))
	if len(stateParts) > 0 {
		line += " — " + strings.Join(stateParts, ", ")
	}
	line += " · " + fmt.Sprintf("%d %s ahead total", totalAhead, pluralize("commit", totalAhead))
	if opts.WithOverlaps {
		n := len(opts.Overlaps)
		line += " · " + fmt.Sprintf("%d %s", n, pluralize("overlap", n))
	}
	fmt.Fprintln(w, line)
}

func pluralize(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func colorState(st session.State, color bool) string {
	if !color {
		return string(st)
	}
	switch st {
	case session.StateDone:
		return tui.Colorize(string(st), tui.Green(), true)
	case session.StateStuck:
		return tui.Colorize(string(st), tui.Red(), true)
	case session.StateWorking:
		return tui.Colorize(string(st), tui.Yellow(), true)
	}
	return string(st)
}

func formatLastCommit(s session.Session) string {
	if s.Last == nil {
		return "—       — (no commits)"
	}
	subj := s.Last.Subject
	if len(subj) > 60 {
		subj = subj[:60]
	}
	return fmt.Sprintf("%s — %s", s.Last.Relative, subj)
}

func writeOverlaps(w io.Writer, overlaps []claims.Overlap) {
	fmt.Fprintln(w)
	if len(overlaps) == 0 {
		fmt.Fprintln(w, "Overlapping claims: none")
		return
	}
	fmt.Fprintln(w, "Overlapping claims:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, o := range overlaps {
		fmt.Fprintf(tw, "  %s\t%s\n", o.Path, strings.Join(o.Sessions, ", "))
	}
	_ = tw.Flush()
}
