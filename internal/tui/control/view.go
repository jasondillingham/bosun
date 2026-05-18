package control

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
)

// render builds the View string for the current Model state. Layout:
//
//	┌─────────────────────────────────────────────────────────────┐
//	│ Bosun control · 3 sessions — 1 DONE, 2 WORKING · 5 ahead   │
//	│                                                             │
//	│   SESSION    BRANCH              STATE   AHEAD  DIRTY  ...  │
//	│ ▸ session-1  bosun/session-1     DONE        3      0  ...  │
//	│   session-2  bosun/session-2     WORKING     2      0  ...  │
//	│                                                             │
//	│ [brief preview, if toggled]                                 │
//	│                                                             │
//	│ status: merge session-1: merged — 3 commit(s) squashed     │
//	│ j/k move · m merge · M merge-all · c cleanup · r remove ... │
//	└─────────────────────────────────────────────────────────────┘
func (m *Model) render() string {
	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")
	b.WriteString(m.renderTable())

	if m.view == viewBriefPreview {
		b.WriteString("\n\n")
		b.WriteString(m.renderBrief())
	}

	if len(m.events) > 0 {
		b.WriteString("\n\n")
		b.WriteString(m.renderEvents())
	}

	b.WriteString("\n\n")
	b.WriteString(m.renderFooter())
	return b.String()
}

// renderHeader is the one-line summary at the top of the screen. Counts
// states + total commits ahead, same data internal/status's
// writeSummary surfaces in `bosun status`.
func (m *Model) renderHeader() string {
	if len(m.sessions) == 0 {
		return m.style(headerStyle).Render("Bosun control · no sessions (run `bosun init` to create some)")
	}
	var done, working, stuck, ahead int
	for _, s := range m.sessions {
		switch s.State {
		case session.StateDone:
			done++
		case session.StateWorking:
			working++
		case session.StateStuck:
			stuck++
		}
		ahead += s.Ahead
	}
	parts := []string{fmt.Sprintf("%d sessions", len(m.sessions))}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", done, m.colorState(session.StateDone)))
	}
	if working > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", working, m.colorState(session.StateWorking)))
	}
	if stuck > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", stuck, m.colorState(session.StateStuck)))
	}
	parts = append(parts, fmt.Sprintf("%d ahead", ahead))
	return m.style(headerStyle).Render("Bosun control · " + strings.Join(parts, " · "))
}

// renderTable lays out the session rows. The selected row gets a ▸
// marker and the selectedRowStyle (bold + reverse) when color is on.
// Columns: SESSION | BRANCH | STATE | AHEAD | DIRTY | CLAIMED | LAST
func (m *Model) renderTable() string {
	if len(m.sessions) == 0 {
		return ""
	}
	headers := []string{"   SESSION", "BRANCH", "STATE", "AHEAD", "DIRTY", "CLAIMED", "LAST"}
	rows := make([][]string, 0, len(m.sessions)+1)
	rows = append(rows, headers)
	for i, s := range m.sessions {
		marker := "  "
		if i == m.selected {
			marker = "▸ "
		}
		last := "—"
		if s.Last != nil {
			subj := s.Last.Subject
			if len(subj) > 40 {
				subj = subj[:40]
			}
			last = fmt.Sprintf("%s · %s", s.Last.Relative, subj)
		}
		// Sub-task counter rides inline in the LAST cell — no new
		// column. Terminal width pressure is already real with seven
		// columns; the suffix matches the CLI status table's shape.
		if s.Subtasks > 0 {
			last = fmt.Sprintf("%s · +%d subs", last, s.Subtasks)
		}
		// Mirror the CLI's tree-prefix from status.RenderText: indent by
		// Depth and prefix sub-sessions with └─ so the tree shape reads
		// the same in `bosun tui` and `bosun status`.
		name := s.Name
		if s.Depth > 0 {
			name = status.Indent(s.Depth) + "└─ " + s.Name
		}
		rows = append(rows, []string{
			marker + name,
			s.Branch,
			m.colorState(s.State),
			fmt.Sprintf("%d", s.Ahead),
			fmt.Sprintf("%d", s.Dirty),
			fmt.Sprintf("%d", s.Claimed),
			last,
		})
	}

	// Compute column widths (visible width — strip ANSI before measuring).
	colW := make([]int, len(headers))
	for _, row := range rows {
		for i, cell := range row {
			if w := visibleWidth(cell); w > colW[i] {
				colW[i] = w
			}
		}
	}

	var b strings.Builder
	for ri, row := range rows {
		line := joinCols(row, colW, 2)
		if ri == m.selected+1 { // +1 to skip header row
			line = m.style(selectedRowStyle).Render(line)
		} else if ri == 0 {
			line = m.style(headerRowStyle).Render(line)
		}
		b.WriteString(line)
		if ri < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderBrief renders the brief preview pane when viewBriefPreview is
// active. Empty body → "(no brief)". Truncated to ~20 lines so the
// preview doesn't take over the screen.
func (m *Model) renderBrief() string {
	s := m.SelectedSession()
	if s == nil {
		return m.style(briefStyle).Render("(no session selected)")
	}
	header := m.style(briefHeaderStyle).Render("BOSUN_BRIEF.md — " + s.Name)
	body := m.briefBody
	if body == "" {
		body = "(no brief written for this session)"
	}
	body = truncateLines(body, 20)
	return header + "\n" + m.style(briefStyle).Render(body)
}

// renderEvents shows the most recent announcement lines (same data
// `bosun status` puts in its "Recent:" feed).
func (m *Model) renderEvents() string {
	var b strings.Builder
	b.WriteString(m.style(headerStyle).Render("Recent activity"))
	// Newest first (events are stored chronological).
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		b.WriteString("\n  ")
		b.WriteString(fmt.Sprintf("%s [%s] %s", e.Session, e.Kind, e.Message))
	}
	return b.String()
}

// renderFooter has two pieces: an action status line (when one is set)
// plus the keybind reminder. The confirm-remove prompt replaces the
// status line when active.
func (m *Model) renderFooter() string {
	var b strings.Builder
	if m.confirmRemove {
		s := m.SelectedSession()
		name := "selected"
		if s != nil {
			name = s.Name
		}
		b.WriteString(m.style(confirmStyle).Render(fmt.Sprintf("Remove %s? [y/N]", name)))
		b.WriteString("\n")
	} else if m.statusLine != "" {
		b.WriteString(m.style(statusLineStyle).Render(m.statusLine))
		b.WriteString("\n")
	}
	b.WriteString(m.style(footerStyle).Render(
		"j/k move · m merge · M merge-all · c cleanup · r remove · l launch · s brief · R refresh · q quit",
	))
	return b.String()
}

// colorState returns the colored state label when color is enabled,
// otherwise just the string. Mirrors internal/status's colorState so the
// TUI and `bosun status` use the same palette.
func (m *Model) colorState(s session.State) string {
	if m.noColor {
		return string(s)
	}
	switch s {
	case session.StateDone:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true).Render(string(s))
	case session.StateStuck:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render(string(s))
	case session.StateWorking:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render(string(s))
	}
	return string(s)
}

// style returns the lipgloss.Style for the given role when color is on;
// otherwise a no-op style so renders stay plain ASCII. Centralizing the
// switch keeps every Render() call uniform.
func (m *Model) style(s lipgloss.Style) lipgloss.Style {
	if m.noColor {
		return lipgloss.NewStyle()
	}
	return s
}

// --- styles ---

var (
	headerStyle      = lipgloss.NewStyle().Bold(true)
	headerRowStyle   = lipgloss.NewStyle().Bold(true).Faint(true)
	selectedRowStyle = lipgloss.NewStyle().Reverse(true)
	footerStyle      = lipgloss.NewStyle().Faint(true)
	statusLineStyle  = lipgloss.NewStyle().Italic(true)
	confirmStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	briefHeaderStyle = lipgloss.NewStyle().Bold(true).Underline(true)
	briefStyle       = lipgloss.NewStyle()
)

// joinCols pads each cell to its column width and joins with `gap` spaces
// between cells. Uses visibleWidth so ANSI-styled cells don't widen the
// column.
func joinCols(cells []string, widths []int, gap int) string {
	var b strings.Builder
	for i, c := range cells {
		b.WriteString(c)
		if i < len(cells)-1 {
			pad := widths[i] - visibleWidth(c) + gap
			if pad < gap {
				pad = gap
			}
			b.WriteString(strings.Repeat(" ", pad))
		}
	}
	return b.String()
}

// visibleWidth returns the printable rune count of s with ANSI escape
// sequences stripped. lipgloss wraps cells in \x1b[...m...\x1b[0m and we
// don't want those to inflate the column width.
func visibleWidth(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// skip
		default:
			n++
		}
	}
	return n
}

// truncateLines returns at most max lines of s, with a trailing
// "(… N more lines)" hint when content was clipped.
func truncateLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	clipped := lines[:max]
	return strings.Join(clipped, "\n") + fmt.Sprintf("\n(… %d more lines)", len(lines)-max)
}
