package control

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
)

// fakeServices records every callback invocation and returns canned
// responses. Tests assert against the recorded calls and the resulting
// model state. Any nil callback in Services is treated by the model as
// "action unavailable" — fakeServices stays explicit.
type fakeServices struct {
	mergeOneCalled  []session.Session
	mergeOneStatus  string
	mergeOneReason  string
	mergeOneErr     error
	mergeAllCalled  bool
	mergeAllResults []ActionResult
	cleanupCalled   bool
	cleanupResults  []ActionResult
	removeCalled    []session.Session
	removeErr       error
	launchCalled    []session.Session
	launchErr       error
	briefCalled     []string
	briefBody       string
	briefErr        error
}

func (f *fakeServices) build() Services {
	return Services{
		Refresh: func() ([]session.Session, []status.Event, error) {
			return mkSessions(), nil, nil
		},
		MergeOne: func(s session.Session) (string, string, error) {
			f.mergeOneCalled = append(f.mergeOneCalled, s)
			return f.mergeOneStatus, f.mergeOneReason, f.mergeOneErr
		},
		MergeAllReady: func() ([]ActionResult, error) {
			f.mergeAllCalled = true
			return f.mergeAllResults, nil
		},
		CleanupAll: func() ([]ActionResult, error) {
			f.cleanupCalled = true
			return f.cleanupResults, nil
		},
		Remove: func(s session.Session) error {
			f.removeCalled = append(f.removeCalled, s)
			return f.removeErr
		},
		Launch: func(s session.Session) error {
			f.launchCalled = append(f.launchCalled, s)
			return f.launchErr
		},
		ReadBrief: func(p string) (string, error) {
			f.briefCalled = append(f.briefCalled, p)
			return f.briefBody, f.briefErr
		},
	}
}

func mkSessions() []session.Session {
	return []session.Session{
		{Number: 1, Name: "session-1", Branch: "bosun/session-1", Path: "/wt/1", State: session.StateWorking, Ahead: 2, Dirty: 0},
		{Number: 2, Name: "session-2", Branch: "bosun/session-2", Path: "/wt/2", State: session.StateDone, Ahead: 3, Dirty: 0},
		{Number: 3, Name: "session-3", Branch: "bosun/session-3", Path: "/wt/3", State: session.StateStuck, Ahead: 1, Dirty: 0},
	}
}

func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{r}})
}

func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg(tea.Key{Type: t})
}

// newSeededModel constructs a model and seeds its session list (without
// going through tea.Cmd plumbing) so tests can drive key events
// immediately.
func newSeededModel(svc Services) *Model {
	m := New(svc, true)
	m.tickInterval = 0
	m.ApplyRefresh(mkSessions(), nil, nil)
	return m
}

// runCmd executes a tea.Cmd (if non-nil) and returns the resulting
// message — analogous to what the bubbletea runtime does between Update
// calls. Lets tests chain "press key → observe action result" in one go.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	return cmd()
}

func TestModel_Navigation_JK(t *testing.T) {
	m := newSeededModel((&fakeServices{}).build())
	if m.Selected() != 0 {
		t.Fatalf("initial selection = %d, want 0", m.Selected())
	}

	m.Update(keyRune('j'))
	if m.Selected() != 1 {
		t.Fatalf("after j, selected = %d, want 1", m.Selected())
	}
	m.Update(keyRune('j'))
	m.Update(keyRune('j')) // beyond end — should clamp
	if m.Selected() != 2 {
		t.Fatalf("after jjj, selected = %d, want 2 (clamped)", m.Selected())
	}
	m.Update(keyRune('k'))
	if m.Selected() != 1 {
		t.Fatalf("after k, selected = %d, want 1", m.Selected())
	}
	// Arrow keys should behave the same as j/k.
	m.Update(keyType(tea.KeyDown))
	if m.Selected() != 2 {
		t.Fatalf("after KeyDown, selected = %d, want 2", m.Selected())
	}
	m.Update(keyType(tea.KeyUp))
	m.Update(keyType(tea.KeyUp))
	if m.Selected() != 0 {
		t.Fatalf("after two KeyUp, selected = %d, want 0", m.Selected())
	}
}

func TestModel_Quit(t *testing.T) {
	m := newSeededModel((&fakeServices{}).build())
	_, cmd := m.Update(keyRune('q'))
	if !m.Quitting() {
		t.Fatal("q should set quitting flag")
	}
	if cmd == nil {
		t.Fatal("q should return tea.Quit cmd")
	}
}

func TestModel_QuitCtrlC(t *testing.T) {
	m := newSeededModel((&fakeServices{}).build())
	_, cmd := m.Update(keyType(tea.KeyCtrlC))
	if !m.Quitting() {
		t.Fatal("ctrl+c should set quitting flag")
	}
	if cmd == nil {
		t.Fatal("ctrl+c should return tea.Quit cmd")
	}
}

func TestModel_BriefToggle_LoadsAndClears(t *testing.T) {
	f := &fakeServices{briefBody: "BOSUN_BRIEF body"}
	m := newSeededModel(f.build())
	if m.ViewMode() != ViewTable {
		t.Fatalf("initial view = %v, want ViewTable", m.ViewMode())
	}

	_, cmd := m.Update(keyRune('s'))
	if m.ViewMode() != ViewBriefPreview {
		t.Fatalf("after s, view = %v, want ViewBriefPreview", m.ViewMode())
	}
	// The toggle returns the brief-load cmd. Run it and feed the result back.
	msg := runCmd(t, cmd)
	if msg == nil {
		t.Fatal("brief toggle should produce a load cmd")
	}
	if _, ok := msg.(briefMsg); !ok {
		t.Fatalf("expected briefMsg, got %T", msg)
	}
	if len(f.briefCalled) != 1 || f.briefCalled[0] != "/wt/1" {
		t.Fatalf("ReadBrief not called for /wt/1: %v", f.briefCalled)
	}

	m.Update(msg)
	if !strings.Contains(m.BriefBody(), "BOSUN_BRIEF body") {
		t.Fatalf("briefBody = %q, want contains body", m.BriefBody())
	}

	// Toggle off — view returns to table, brief body cleared.
	m.Update(keyRune('s'))
	if m.ViewMode() != ViewTable {
		t.Fatalf("after second s, view = %v, want ViewTable", m.ViewMode())
	}
	if m.BriefBody() != "" {
		t.Fatalf("briefBody should clear on toggle-off, got %q", m.BriefBody())
	}
}

func TestModel_BriefToggle_RereadsOnSelectionChange(t *testing.T) {
	f := &fakeServices{briefBody: "body"}
	m := newSeededModel(f.build())

	// Toggle preview on — drive the resulting cmd so ReadBrief fires.
	_, cmd := m.Update(keyRune('s'))
	runCmd(t, cmd)
	// Move selection — should issue a fresh ReadBrief for the new worktree.
	_, cmd = m.Update(keyRune('j'))
	runCmd(t, cmd)

	if len(f.briefCalled) < 2 {
		t.Fatalf("expected ReadBrief to fire on selection change, calls=%v", f.briefCalled)
	}
	if f.briefCalled[len(f.briefCalled)-1] != "/wt/2" {
		t.Fatalf("expected most recent ReadBrief on /wt/2, got %v", f.briefCalled)
	}
}

func TestModel_Merge_OnNotDoneShowsHint(t *testing.T) {
	f := &fakeServices{}
	m := newSeededModel(f.build())
	// Selection 0 is session-1, WORKING.
	_, cmd := m.Update(keyRune('m'))
	if cmd == nil {
		t.Fatal("expected hint cmd even when merge isn't applicable")
	}
	msg := runCmd(t, cmd)
	m.Update(msg)
	if !strings.Contains(m.StatusLine(), "not DONE") {
		t.Fatalf("status line = %q, want hint about not-DONE", m.StatusLine())
	}
	if len(f.mergeOneCalled) != 0 {
		t.Fatalf("MergeOne should NOT be invoked on non-DONE session, got %v", f.mergeOneCalled)
	}
}

func TestModel_Merge_OnDoneCallsMergeOne(t *testing.T) {
	f := &fakeServices{
		mergeOneStatus: "merged",
		mergeOneReason: "3 commit(s) squashed",
	}
	m := newSeededModel(f.build())
	m.Update(keyRune('j')) // move to session-2 (DONE)

	_, cmd := m.Update(keyRune('m'))
	msg := runCmd(t, cmd)
	if msg == nil {
		t.Fatal("merge on DONE should produce an action message")
	}
	m.Update(msg)
	if len(f.mergeOneCalled) != 1 || f.mergeOneCalled[0].Name != "session-2" {
		t.Fatalf("MergeOne called with %v, want session-2 once", f.mergeOneCalled)
	}
	if !strings.Contains(m.StatusLine(), "merged") {
		t.Fatalf("status line = %q, want it to mention 'merged'", m.StatusLine())
	}
}

func TestModel_MergeAll_Invoked(t *testing.T) {
	f := &fakeServices{
		mergeAllResults: []ActionResult{
			{Session: "session-2", Status: "merged", Reason: "ok"},
			{Session: "session-3", Status: "skipped", Reason: "not done"},
		},
	}
	m := newSeededModel(f.build())
	_, cmd := m.Update(keyRune('M'))
	msg := runCmd(t, cmd)
	if !f.mergeAllCalled {
		t.Fatal("MergeAllReady should be invoked")
	}
	m.Update(msg)
	if !strings.Contains(m.StatusLine(), "merged") || !strings.Contains(m.StatusLine(), "skipped") {
		t.Fatalf("status line = %q, want mentions of merged and skipped", m.StatusLine())
	}
}

func TestModel_Cleanup_Invoked(t *testing.T) {
	f := &fakeServices{
		cleanupResults: []ActionResult{
			{Session: "session-1", Status: "removed", Reason: "DONE"},
		},
	}
	m := newSeededModel(f.build())
	_, cmd := m.Update(keyRune('c'))
	msg := runCmd(t, cmd)
	if !f.cleanupCalled {
		t.Fatal("CleanupAll should be invoked")
	}
	m.Update(msg)
	if !strings.Contains(m.StatusLine(), "removed") {
		t.Fatalf("status line = %q, want mentions 'removed'", m.StatusLine())
	}
}

func TestModel_Remove_RequiresConfirm(t *testing.T) {
	f := &fakeServices{}
	m := newSeededModel(f.build())

	_, cmd := m.Update(keyRune('r'))
	if cmd != nil {
		// The 'r' handler may produce nil (set confirm flag, no message).
		// Run anyway — if it returns a message, it should NOT trigger Remove.
		msg := runCmd(t, cmd)
		if msg != nil {
			m.Update(msg)
		}
	}
	if !m.ConfirmingRemove() {
		t.Fatal("r should set ConfirmingRemove")
	}
	if len(f.removeCalled) != 0 {
		t.Fatalf("Remove should NOT fire before confirm, got %v", f.removeCalled)
	}

	// Cancel with anything other than y.
	_, _ = m.Update(keyRune('n'))
	if m.ConfirmingRemove() {
		t.Fatal("ConfirmingRemove should clear after non-y key")
	}
	if len(f.removeCalled) != 0 {
		t.Fatalf("Remove should NOT fire after cancel, got %v", f.removeCalled)
	}
	if !strings.Contains(m.StatusLine(), "cancel") {
		t.Fatalf("status line after cancel = %q, want mentions 'cancel'", m.StatusLine())
	}

	// Now confirm.
	m.Update(keyRune('r'))
	_, cmd2 := m.Update(keyRune('y'))
	msg := runCmd(t, cmd2)
	if len(f.removeCalled) != 1 || f.removeCalled[0].Name != "session-1" {
		t.Fatalf("Remove not called for session-1: %v", f.removeCalled)
	}
	if msg != nil {
		m.Update(msg)
	}
	if !strings.Contains(m.StatusLine(), "session-1") {
		t.Fatalf("status line = %q, want mentions session-1", m.StatusLine())
	}
}

func TestModel_Launch_Invoked(t *testing.T) {
	f := &fakeServices{}
	m := newSeededModel(f.build())
	_, cmd := m.Update(keyRune('l'))
	msg := runCmd(t, cmd)
	if len(f.launchCalled) != 1 || f.launchCalled[0].Name != "session-1" {
		t.Fatalf("Launch should fire for session-1, got %v", f.launchCalled)
	}
	if msg != nil {
		m.Update(msg)
	}
	if !strings.Contains(m.StatusLine(), "launch") {
		t.Fatalf("status line = %q, want mentions launch", m.StatusLine())
	}
}

func TestModel_ManualRefresh(t *testing.T) {
	calls := 0
	svc := Services{
		Refresh: func() ([]session.Session, []status.Event, error) {
			calls++
			return mkSessions(), nil, nil
		},
	}
	m := New(svc, true)
	m.tickInterval = 0
	m.ApplyRefresh(mkSessions(), nil, nil)
	calls = 0 // reset after seed

	_, cmd := m.Update(keyRune('R'))
	msg := runCmd(t, cmd)
	if calls != 1 {
		t.Fatalf("R should call Refresh once, got %d", calls)
	}
	m.Update(msg) // shouldn't crash
}

func TestModel_RefreshError_SurfacesAsStatusLine(t *testing.T) {
	svc := Services{
		Refresh: func() ([]session.Session, []status.Event, error) {
			return nil, nil, errors.New("boom")
		},
	}
	m := New(svc, true)
	m.tickInterval = 0

	cmd := m.refreshCmd()
	if cmd == nil {
		t.Fatal("refreshCmd should be non-nil when Refresh service is set")
	}
	msg := runCmd(t, cmd)
	m.Update(msg)
	if !strings.Contains(m.StatusLine(), "boom") {
		t.Fatalf("status line = %q, want it to surface the refresh error", m.StatusLine())
	}
}

func TestModel_RefreshShrink_ClampsSelection(t *testing.T) {
	m := newSeededModel((&fakeServices{}).build())
	m.Update(keyRune('j'))
	m.Update(keyRune('j')) // selected = 2
	if m.Selected() != 2 {
		t.Fatalf("setup: selected = %d, want 2", m.Selected())
	}
	// Refresh with only one session — selection should clamp to 0.
	m.ApplyRefresh(mkSessions()[:1], nil, nil)
	if m.Selected() != 0 {
		t.Fatalf("selection should clamp to 0 after shrink, got %d", m.Selected())
	}
}

func TestModel_NilService_ShowsHintInsteadOfCrashing(t *testing.T) {
	// Build with most callbacks nil — only Refresh present so the model
	// can seed itself.
	svc := Services{
		Refresh: func() ([]session.Session, []status.Event, error) {
			return mkSessions(), nil, nil
		},
	}
	m := New(svc, true)
	m.tickInterval = 0
	m.ApplyRefresh(mkSessions(), nil, nil)
	m.Update(keyRune('j')) // session-2 (DONE) so the merge gate is happy

	for _, k := range []rune{'m', 'M', 'c', 'r', 'l'} {
		_, cmd := m.Update(keyRune(k))
		msg := runCmd(t, cmd)
		if msg != nil {
			m.Update(msg)
		}
		if !strings.Contains(m.StatusLine(), "unavailable") {
			t.Fatalf("key %q with nil service: status line = %q, want 'unavailable'", string(k), m.StatusLine())
		}
	}
}

func TestModel_View_RendersTable(t *testing.T) {
	m := newSeededModel((&fakeServices{}).build())
	out := m.View()
	for _, want := range []string{"session-1", "session-2", "session-3", "DONE", "WORKING", "STUCK"} {
		if !strings.Contains(out, want) {
			t.Errorf("View() missing %q\n%s", want, out)
		}
	}
}

func TestModel_View_BriefPreviewIncludesHeading(t *testing.T) {
	m := newSeededModel((&fakeServices{briefBody: "hello"}).build())
	m.Update(keyRune('s'))
	m.ApplyBrief("hello brief contents")
	out := m.View()
	if !strings.Contains(out, "BOSUN_BRIEF.md") {
		t.Errorf("View() missing brief heading:\n%s", out)
	}
	if !strings.Contains(out, "hello brief contents") {
		t.Errorf("View() missing brief body:\n%s", out)
	}
}

func TestSummarizeResults(t *testing.T) {
	tests := []struct {
		name string
		in   []ActionResult
		want string
	}{
		{"empty", nil, "nothing to do"},
		{"merged + skipped", []ActionResult{
			{Status: "merged"}, {Status: "merged"}, {Status: "skipped"},
		}, "2 merged, 1 skipped"},
		{"removed only", []ActionResult{
			{Status: "removed"}, {Status: "removed"},
		}, "2 removed"},
		{"with conflict", []ActionResult{
			{Status: "merged"}, {Status: "conflict"},
		}, "1 merged, 1 conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeResults(tt.in)
			if got != tt.want {
				t.Errorf("summarizeResults(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
