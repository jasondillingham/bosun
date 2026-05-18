// Package status renders the `bosun status` table (plain text or JSON).
package status

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/tui"
)

// Event is one operator-visible announcement (typed in the status package
// so the renderer doesn't have to import internal/mcp). The caller in
// cmd_status.go fills these from mcp.TailEvents and hands them in.
type Event struct {
	Session string
	Kind    string
	Message string
	At      time.Time
}

// RenderOptions controls table rendering.
type RenderOptions struct {
	Sessions     []session.Session
	Overlaps     []claims.Overlap // nil if --with-overlaps not requested
	WithOverlaps bool
	NoColor      bool
	// Events is an optional list of recent bosun_announce records. When
	// non-empty, RenderText prints one "Recent: ..." line per event after
	// the summary line.
	Events []Event
	// Now is the reference time used to format event ages. Zero defers to
	// time.Now() — tests pin it for deterministic output.
	Now time.Time
	// SummaryOnly suppresses the table, the events block, and the overlap
	// section, leaving just the one-line summary. For scripting and small
	// terminals — the same data `bosun status` would derive, minus the
	// per-session breakdown.
	SummaryOnly bool
	// NoTree forces the flat one-row-per-session table even when spawn
	// trees exist. Set by `bosun status --no-tree` for scripts that parse
	// the table. Default (false) renders tree-shaped output indented
	// under each parent.
	NoTree bool
}

// RenderText writes a human-readable table (and optional overlap section) to w.
func RenderText(w io.Writer, opts RenderOptions) error {
	useColor := tui.ShouldColor(opts.NoColor)

	if len(opts.Sessions) == 0 {
		fmt.Fprintln(w, "bosun: no sessions. Run `bosun init` to create some.")
		return nil
	}

	writeSummary(w, opts, useColor)
	if opts.SummaryOnly {
		return nil
	}
	writeEvents(w, opts, useColor)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tBRANCH\tSTATE\tAHEAD\tDIRTY\tCLAIMED\tRUNNING\tLAST_COMMIT")
	rows := opts.Sessions
	if !opts.NoTree {
		rows = TreeOrdered(opts.Sessions)
	}
	for _, s := range rows {
		name := s.Name
		if !opts.NoTree && s.Depth > 0 {
			// One indent level per depth step + a tree-prefix glyph at
			// the leaf. Children of the same parent show └─ uniformly;
			// renderers more clever about │ / ├─ aren't worth the code
			// when --no-tree exists for scripts that care.
			name = Indent(s.Depth) + "└─ " + s.Name
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
			name,
			s.Branch,
			renderStateCell(s, useColor),
			s.Ahead,
			s.Dirty,
			s.Claimed,
			formatRunning(s),
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
	var doneN, workingN, stuckN, crashedN, totalAhead int
	for _, s := range opts.Sessions {
		switch s.State {
		case session.StateDone:
			doneN++
		case session.StateWorking:
			workingN++
		case session.StateStuck:
			stuckN++
		case session.StateCrashed:
			crashedN++
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
	if crashedN > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d %s", crashedN, colorState(session.StateCrashed, useColor)))
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

// writeEvents prints one "Recent:" line per buffered announcement, newest
// first. No-op when opts.Events is empty so the table still butts up
// directly against the summary in the common case.
func writeEvents(w io.Writer, opts RenderOptions, useColor bool) {
	if len(opts.Events) == 0 {
		return
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	// Iterate newest-first: caller hands them oldest-first (chronological).
	for i := len(opts.Events) - 1; i >= 0; i-- {
		e := opts.Events[i]
		kind := e.Kind
		if useColor {
			kind = colorKind(e.Kind)
		}
		fmt.Fprintf(w, "Recent: %s [%s] %s (%s)\n",
			e.Session, kind, e.Message, relativeAge(now, e.At))
	}
}

// colorKind tints the kind label so the operator can scan progress vs.
// warnings at a glance. Unknown kinds pass through unstyled.
func colorKind(kind string) string {
	switch kind {
	case "warn":
		return tui.Colorize(kind, tui.Red(), true)
	case "progress":
		return tui.Colorize(kind, tui.Yellow(), true)
	case "info":
		return tui.Colorize(kind, tui.Green(), true)
	}
	return kind
}

// relativeAge formats now-then in a compact form (e.g. "3s ago", "12m ago",
// "2h ago"). Future timestamps (clock skew) get "just now" so the line
// doesn't show a confusing negative duration.
func relativeAge(now, then time.Time) string {
	d := now.Sub(then)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
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
	case session.StateCrashed:
		return tui.Colorize(string(st), tui.Red(), true)
	case session.StateWorking:
		return tui.Colorize(string(st), tui.Yellow(), true)
	}
	return string(st)
}

// renderStateCell builds the STATE column value: the colored state name
// followed by a "(STALE)" suffix when the session is flagged stale. STALE
// stays attached to the State column so the operator scans one column for
// "is this session OK?" instead of two.
func renderStateCell(s session.Session, color bool) string {
	cell := colorState(s.State, color)
	if !s.Stale {
		return cell
	}
	tag := "STALE"
	if color {
		tag = tui.Colorize(tag, tui.Yellow(), true)
	}
	return cell + " (" + tag + ")"
}

// formatRunning renders the RUNNING column: the pid of the live agent when
// one was detected, or an em-dash placeholder otherwise. The pid is more
// useful than a bare "yes" since it lets the operator attach or kill the
// session without rederiving it.
func formatRunning(s session.Session) string {
	if !s.Running {
		return "—"
	}
	return fmt.Sprintf("%d", s.RunningPID)
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

// TreeOrdered reorders sessions so each parent is immediately followed
// by its children (in the order spawntree.Store reports them). Sessions
// with no Parent entry are emitted at the top of their group in their
// original sort order (which session.Derive already sorts by number).
// A session that references a Parent not present in the slice falls
// through to the orphan-pass at the bottom — defensive against
// spawn-tree drift after a parent has been reaped.
//
// Exported so the TUI and web dashboard render in the same tree order
// the status table uses.
func TreeOrdered(sessions []session.Session) []session.Session {
	if len(sessions) == 0 {
		return sessions
	}
	// Key on Name rather than Label — they're canonically the same
	// (struct doc: "Label matches Name") but Derive populates both
	// while many test fixtures only set Name. Name is the resilient
	// pick when consumers might be either.
	byName := make(map[string]session.Session, len(sessions))
	childrenOf := make(map[string][]string, len(sessions))
	for _, s := range sessions {
		byName[s.Name] = s
		if s.Parent != "" {
			childrenOf[s.Parent] = append(childrenOf[s.Parent], s.Name)
		}
	}
	emitted := make(map[string]bool, len(sessions))
	out := make([]session.Session, 0, len(sessions))
	var emit func(name string)
	emit = func(name string) {
		if emitted[name] {
			return
		}
		s, ok := byName[name]
		if !ok {
			return
		}
		emitted[name] = true
		out = append(out, s)
		for _, child := range childrenOf[name] {
			emit(child)
		}
	}
	// First pass: top-level sessions in original order.
	for _, s := range sessions {
		if s.Parent == "" {
			emit(s.Name)
		}
	}
	// Orphan pass: anything still un-emitted (Parent named a session not
	// in the snapshot — usually because the parent was reaped but the
	// spawn-tree.json reference lingers). Emit at the bottom so the
	// operator can see them; tree-prefix indentation still fires since
	// Depth was populated.
	for _, s := range sessions {
		emit(s.Name)
	}
	return out
}

// Indent returns the leading whitespace for a tree-level. Two spaces
// per depth step makes parent/child visually distinct without
// consuming horizontal space in the terminal table.
//
// Exported so TUI and web renderers can match the CLI's indentation.
func Indent(depth int) string {
	return strings.Repeat("  ", depth)
}
