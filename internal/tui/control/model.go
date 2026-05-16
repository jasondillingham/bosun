// Package control implements the bubbletea-driven control center for bosun
// (`bosun tui`). The Model holds derived session state plus the recent
// events feed; Update drives keybinds for the common operator actions
// (merge / cleanup / remove / launch / show); View renders the result.
//
// Action handlers are injected via the Services struct so the wiring in
// cmd_tui.go can hand the model real callbacks that call mergeOne,
// cleanupOne, launcher.Launch, etc., while tests construct a Model with
// fakes and drive it through Update without a real terminal.
package control

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
)

// Services bundles the side-effecting callbacks the model invokes for
// operator actions. cmd_tui.go fills these with real bosun functions;
// tests pass fakes. Any callback may be nil — the model treats that as
// "action not available" and surfaces a status line instead of crashing.
type Services struct {
	// Refresh returns the latest session list and recent events. Called
	// on startup, on the 2s tick, and after any action that mutates state.
	Refresh func() ([]session.Session, []status.Event, error)

	// MergeOne merges the named session (passing the user's choice
	// through whatever the wiring decides — typically the default opts).
	// Returns (statusStr, reason, err). statusStr is one of
	// "merged"/"skipped"/"conflict" so the model can color the result.
	MergeOne func(s session.Session) (string, string, error)

	// MergeAllReady merges every DONE session in dependency order. The
	// returned results are rendered as a single status line.
	MergeAllReady func() ([]ActionResult, error)

	// CleanupAll runs the same plan as `bosun cleanup` (no flags).
	CleanupAll func() ([]ActionResult, error)

	// Remove tears down the session's worktree and branch (called after
	// the user confirms).
	Remove func(s session.Session) error

	// Launch opens a launcher window for the session.
	Launch func(s session.Session) error

	// ReadBrief returns the BOSUN_BRIEF.md contents for the worktree
	// (or "" if no brief was written). Used by the preview pane.
	ReadBrief func(worktreePath string) (string, error)
}

// ActionResult is a per-session outcome returned by bulk actions like
// MergeAllReady and CleanupAll. The model renders these as a single
// summary line below the table.
type ActionResult struct {
	Session string // e.g. "session-1"
	Status  string // "merged" / "skipped" / "removed" / "conflict" / etc.
	Reason  string // human-readable detail
}

// viewMode tracks what the model is showing in the body of the screen.
type viewMode int

const (
	viewTable viewMode = iota
	viewBriefPreview
)

// Model is the bubbletea.Model for `bosun tui`. Field visibility is
// package-private — tests construct via New() and observe via the
// exported accessors (Selected, ViewMode, etc.).
type Model struct {
	services Services
	sessions []session.Session
	events   []status.Event
	selected int
	view     viewMode

	// confirmRemove, when true, means the next key press is interpreted
	// as the answer to "Remove session-N? [y/N]" — 'y' executes, anything
	// else cancels.
	confirmRemove bool

	// briefBody holds the cached BOSUN_BRIEF.md contents for the currently-
	// selected session when view == viewBriefPreview. Refreshed when the
	// user toggles preview on or moves selection.
	briefBody string

	// statusLine is the one-line message shown above the keybind footer.
	// Action handlers write here so the operator sees what happened
	// without leaving the TUI.
	statusLine string

	// noColor disables lipgloss coloring (mirrors `--no-color` on other
	// bosun subcommands). The model still renders structure; just no ANSI.
	noColor bool

	// tickInterval is the auto-refresh cadence. Configurable so tests can
	// avoid scheduling real tea.Cmd timers.
	tickInterval time.Duration

	// quitting is set when the user presses q / Ctrl-C — Update returns
	// tea.Quit on the next message.
	quitting bool
}

// New constructs a Model with the given services. The model starts in
// viewTable mode and immediately schedules a refresh + tick on Init.
func New(services Services, noColor bool) *Model {
	return &Model{
		services:     services,
		view:         viewTable,
		noColor:      noColor,
		tickInterval: 2 * time.Second,
	}
}

// --- bubbletea.Model interface ---

// Init triggers the first refresh and starts the auto-refresh tick.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), m.tickCmd())
}

// Update routes incoming messages to the appropriate handler. Messages
// the model produces internally (tickMsg, refreshMsg, actionMsg) are
// handled by name; everything else is treated as a key press.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tickMsg:
		return m, tea.Batch(m.refreshCmd(), m.tickCmd())
	case refreshMsg:
		m.applyRefresh(msg)
		return m, nil
	case actionMsg:
		m.statusLine = msg.line
		return m, m.refreshCmd()
	case briefMsg:
		m.briefBody = msg.body
		return m, nil
	}
	return m, nil
}

// View defers to the package's view.go to keep model.go focused on state
// transitions.
func (m *Model) View() string {
	return m.render()
}

// --- accessors used by tests ---

// Sessions returns the current session slice. Tests inspect this after
// driving Update to assert the model's view of the world.
func (m *Model) Sessions() []session.Session { return m.sessions }

// Selected returns the currently-highlighted session index (0-based into
// Sessions()). Tests assert this after sending j/k key events.
func (m *Model) Selected() int { return m.selected }

// SelectedSession returns a pointer to the currently-highlighted session,
// or nil when the session list is empty.
func (m *Model) SelectedSession() *session.Session {
	if len(m.sessions) == 0 {
		return nil
	}
	if m.selected < 0 || m.selected >= len(m.sessions) {
		return nil
	}
	return &m.sessions[m.selected]
}

// ViewMode returns the current view (table or brief-preview). Tests
// assert this after sending the 's' toggle key.
func (m *Model) ViewMode() viewMode { return m.view }

// StatusLine returns the most-recent action result/status message.
// Tests assert this after merging/cleaning/removing to confirm the
// model surfaced the operation's outcome.
func (m *Model) StatusLine() string { return m.statusLine }

// ConfirmingRemove reports whether the model is currently asking the
// user to confirm a remove. Tests use this to verify the two-stage
// remove flow ('r' then 'y').
func (m *Model) ConfirmingRemove() bool { return m.confirmRemove }

// Quitting reports whether the model has been told to exit. Tests
// drive 'q'/Ctrl-C and assert this transitions to true.
func (m *Model) Quitting() bool { return m.quitting }

// BriefBody returns the cached brief contents (for the preview pane).
// Empty unless ViewMode() is viewBriefPreview.
func (m *Model) BriefBody() string { return m.briefBody }

// View mode constants exported for tests that need to assert against
// them without exporting the underlying type.
const (
	ViewTable        = viewTable
	ViewBriefPreview = viewBriefPreview
)

// --- key handling ---

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Confirm prompt eats every key — y executes, anything else cancels.
	if m.confirmRemove {
		switch msg.String() {
		case "y", "Y":
			m.confirmRemove = false
			return m, m.removeSelectedCmd()
		default:
			m.confirmRemove = false
			m.statusLine = "remove cancelled"
			return m, nil
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "j", "down":
		m.moveSelection(+1)
		return m, m.maybeRefreshBrief()
	case "k", "up":
		m.moveSelection(-1)
		return m, m.maybeRefreshBrief()

	case "m":
		return m, m.mergeSelectedCmd()
	case "M":
		return m, m.mergeAllCmd()
	case "c":
		return m, m.cleanupCmd()
	case "r":
		return m, m.promptRemove()
	case "l":
		return m, m.launchSelectedCmd()
	case "s":
		m.toggleBrief()
		return m, m.maybeRefreshBrief()
	case "R":
		return m, m.refreshCmd()
	}
	return m, nil
}

func (m *Model) moveSelection(delta int) {
	if len(m.sessions) == 0 {
		m.selected = 0
		return
	}
	m.selected += delta
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.sessions) {
		m.selected = len(m.sessions) - 1
	}
}

func (m *Model) toggleBrief() {
	if m.view == viewBriefPreview {
		m.view = viewTable
		m.briefBody = ""
		return
	}
	m.view = viewBriefPreview
}

// --- action commands ---

func (m *Model) mergeSelectedCmd() tea.Cmd {
	s := m.SelectedSession()
	if s == nil {
		return actionLine("no session selected")
	}
	if m.services.MergeOne == nil {
		return actionLine("merge: action unavailable")
	}
	if s.State != session.StateDone {
		return actionLine(fmt.Sprintf("merge: %s is not DONE — press M to override", s.Name))
	}
	mergeFn := m.services.MergeOne
	sCopy := *s
	return func() tea.Msg {
		statusStr, reason, err := mergeFn(sCopy)
		if err != nil {
			return actionMsg{line: fmt.Sprintf("merge %s: %v", sCopy.Name, err)}
		}
		return actionMsg{line: fmt.Sprintf("merge %s: %s — %s", sCopy.Name, statusStr, reason)}
	}
}

func (m *Model) mergeAllCmd() tea.Cmd {
	if m.services.MergeAllReady == nil {
		return actionLine("merge-all: action unavailable")
	}
	fn := m.services.MergeAllReady
	return func() tea.Msg {
		results, err := fn()
		if err != nil {
			return actionMsg{line: fmt.Sprintf("merge-all: %v", err)}
		}
		return actionMsg{line: fmt.Sprintf("merge-all: %s", summarizeResults(results))}
	}
}

func (m *Model) cleanupCmd() tea.Cmd {
	if m.services.CleanupAll == nil {
		return actionLine("cleanup: action unavailable")
	}
	fn := m.services.CleanupAll
	return func() tea.Msg {
		results, err := fn()
		if err != nil {
			return actionMsg{line: fmt.Sprintf("cleanup: %v", err)}
		}
		return actionMsg{line: fmt.Sprintf("cleanup: %s", summarizeResults(results))}
	}
}

func (m *Model) promptRemove() tea.Cmd {
	if m.SelectedSession() == nil {
		return actionLine("no session selected")
	}
	if m.services.Remove == nil {
		return actionLine("remove: action unavailable")
	}
	m.confirmRemove = true
	return nil
}

func (m *Model) removeSelectedCmd() tea.Cmd {
	s := m.SelectedSession()
	if s == nil {
		return actionLine("no session selected")
	}
	if m.services.Remove == nil {
		return actionLine("remove: action unavailable")
	}
	removeFn := m.services.Remove
	sCopy := *s
	return func() tea.Msg {
		if err := removeFn(sCopy); err != nil {
			return actionMsg{line: fmt.Sprintf("remove %s: %v", sCopy.Name, err)}
		}
		return actionMsg{line: fmt.Sprintf("remove %s: done", sCopy.Name)}
	}
}

func (m *Model) launchSelectedCmd() tea.Cmd {
	s := m.SelectedSession()
	if s == nil {
		return actionLine("no session selected")
	}
	if m.services.Launch == nil {
		return actionLine("launch: action unavailable")
	}
	launchFn := m.services.Launch
	sCopy := *s
	return func() tea.Msg {
		if err := launchFn(sCopy); err != nil {
			return actionMsg{line: fmt.Sprintf("launch %s: %v", sCopy.Name, err)}
		}
		return actionMsg{line: fmt.Sprintf("launch %s: window opened", sCopy.Name)}
	}
}

// --- refresh + tick plumbing ---

// refreshCmd derives a fresh session list + events feed and dispatches a
// refreshMsg with the result. Errors come back inside the message so the
// model can render them rather than die.
func (m *Model) refreshCmd() tea.Cmd {
	if m.services.Refresh == nil {
		return nil
	}
	fn := m.services.Refresh
	return func() tea.Msg {
		sessions, events, err := fn()
		return refreshMsg{sessions: sessions, events: events, err: err}
	}
}

// maybeRefreshBrief re-reads the brief for the new selection when the
// preview pane is open. No-op otherwise.
func (m *Model) maybeRefreshBrief() tea.Cmd {
	if m.view != viewBriefPreview {
		return nil
	}
	s := m.SelectedSession()
	if s == nil || m.services.ReadBrief == nil {
		m.briefBody = ""
		return nil
	}
	briefFn := m.services.ReadBrief
	worktreePath := s.Path
	return func() tea.Msg {
		body, err := briefFn(worktreePath)
		if err != nil {
			return briefMsg{body: fmt.Sprintf("(brief read failed: %v)", err)}
		}
		return briefMsg{body: body}
	}
}

// tickCmd schedules the next 2-second auto-refresh.
func (m *Model) tickCmd() tea.Cmd {
	if m.tickInterval <= 0 {
		return nil
	}
	interval := m.tickInterval
	return tea.Tick(interval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) applyRefresh(msg refreshMsg) {
	if msg.err != nil {
		m.statusLine = fmt.Sprintf("refresh: %v", msg.err)
		return
	}
	m.sessions = msg.sessions
	m.events = msg.events
	// Keep the selection on the same session if possible, otherwise clamp
	// to the new bounds.
	if m.selected >= len(m.sessions) {
		m.selected = len(m.sessions) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

// --- internal messages ---

type tickMsg struct{}

type refreshMsg struct {
	sessions []session.Session
	events   []status.Event
	err      error
}

type actionMsg struct {
	line string
}

type briefMsg struct {
	body string
}

// ApplyRefresh is a test-only entry point that lets a test drive a
// refresh result directly without scheduling a real tea.Cmd. It mirrors
// the side effect Update has when it receives a refreshMsg.
func (m *Model) ApplyRefresh(sessions []session.Session, events []status.Event, err error) {
	m.applyRefresh(refreshMsg{sessions: sessions, events: events, err: err})
}

// ApplyBrief is a test-only entry point that simulates a brief read
// completing. Lets tests assert the View() output without a real I/O
// path through the bubbletea runtime.
func (m *Model) ApplyBrief(body string) {
	m.briefBody = body
}

// actionLine returns a no-arg tea.Cmd that emits an actionMsg with the
// given line. Used when an action can't run (nil service, wrong state)
// so the model still surfaces a message to the operator instead of
// silently no-op'ing.
func actionLine(line string) tea.Cmd {
	return func() tea.Msg { return actionMsg{line: line} }
}

// summarizeResults compresses a list of ActionResults into a short line
// like "2 merged, 1 skipped, 1 conflict" — what the operator wants to
// see at a glance after pressing M or c.
func summarizeResults(results []ActionResult) string {
	if len(results) == 0 {
		return "nothing to do"
	}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	// Stable ordering: merged/removed first, then skipped, then conflict,
	// then anything else alphabetized doesn't matter for this short summary.
	var parts []string
	for _, k := range []string{"merged", "removed"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
			delete(counts, k)
		}
	}
	if counts["skipped"] > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", counts["skipped"]))
		delete(counts, "skipped")
	}
	if counts["conflict"] > 0 {
		parts = append(parts, fmt.Sprintf("%d conflict", counts["conflict"]))
		delete(counts, "conflict")
	}
	for k, v := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", v, k))
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
