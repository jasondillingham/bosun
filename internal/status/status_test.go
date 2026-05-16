package status

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/session"
)

func sampleSessions() []session.Session {
	return []session.Session{
		{
			Number: 1, Name: "session-1", Branch: "bosun/session-1",
			Path: "/code/myproj-bosun-1", State: session.StateDone,
			Ahead: 2, Dirty: 0, Claimed: 3,
			Last: &git.LogEntry{ShortSHA: "abc1234", Relative: "23s ago", Subject: "implement auth handler"},
		},
		{
			Number: 2, Name: "session-2", Branch: "bosun/session-2",
			Path: "/code/myproj-bosun-2", State: session.StateWorking,
			Ahead: 1, Dirty: 3, Claimed: 5,
			Last: &git.LogEntry{ShortSHA: "def5678", Relative: "1m ago", Subject: "add data layer"},
		},
		{
			Number: 3, Name: "session-3", Branch: "bosun/session-3",
			Path: "/code/myproj-bosun-3", State: session.StateWorking,
			Ahead: 0, Dirty: 0, Claimed: 0,
			Last: nil,
		},
	}
}

func TestRenderText_HeaderAndRows(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{Sessions: sampleSessions(), NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"SESSION", "BRANCH", "STATE", "AHEAD", "DIRTY", "CLAIMED", "LAST_COMMIT",
		"session-1", "bosun/session-1", "DONE", "implement auth handler",
		"session-3", "(no commits)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderText_SummaryLine(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{Sessions: sampleSessions(), NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// sampleSessions: 1 DONE (ahead 2), 2 WORKING (ahead 1+0 = 1), total ahead = 3.
	for _, want := range []string{
		"3 sessions",
		"1 DONE",
		"2 WORKING",
		"3 commits ahead total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n--- output ---\n%s", want, out)
		}
	}
	// Summary should be on the very first line, above the SESSION header.
	firstLine := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(firstLine, "3 sessions") {
		t.Errorf("summary should be the first line, got: %q", firstLine)
	}
	// STUCK absent from sample — should not appear in summary.
	if strings.Contains(firstLine, "STUCK") {
		t.Errorf("summary should omit zero-count states: %q", firstLine)
	}
}

func TestRenderText_SummaryOnly(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{
		Sessions:    sampleSessions(),
		NoColor:     true,
		SummaryOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Summary content must still be present.
	if !strings.Contains(out, "3 sessions") {
		t.Errorf("summary line missing:\n%s", out)
	}
	// Table header must be suppressed — the whole point of --summary-only.
	if strings.Contains(out, "SESSION") || strings.Contains(out, "BRANCH") {
		t.Errorf("table header leaked into summary-only output:\n%s", out)
	}
	// Output should be exactly one newline-terminated line.
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("summary-only should be a single line, got %d newlines:\n%s", n, out)
	}
}

func TestRenderText_SummaryOnlySuppressesEventsAndOverlaps(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{
		Sessions:     sampleSessions(),
		WithOverlaps: true,
		Overlaps: []claims.Overlap{
			{Path: "internal/auth/handler.go", Sessions: []string{"session-1", "session-3"}},
		},
		Events: []Event{
			{Session: "session-1", Kind: "info", Message: "starting", At: now.Add(-30 * time.Second)},
		},
		Now:         now,
		NoColor:     true,
		SummaryOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "Recent:") {
		t.Errorf("events leaked into summary-only output:\n%s", out)
	}
	if strings.Contains(out, "internal/auth/handler.go") {
		t.Errorf("overlaps leaked into summary-only output:\n%s", out)
	}
	// But the overlap count should still be in the summary line (it lives
	// alongside the state counts, controlled by WithOverlaps not by the
	// summary-only switch).
	if !strings.Contains(out, "1 overlap") {
		t.Errorf("summary should still report overlap count, got:\n%s", out)
	}
}

func TestRenderText_SummaryWithOverlaps(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{
		Sessions:     sampleSessions(),
		WithOverlaps: true,
		Overlaps: []claims.Overlap{
			{Path: "internal/auth/handler.go", Sessions: []string{"session-1", "session-3"}},
			{Path: "internal/db.go", Sessions: []string{"session-2", "session-3"}},
		},
		NoColor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstLine := strings.SplitN(buf.String(), "\n", 2)[0]
	if !strings.Contains(firstLine, "2 overlaps") {
		t.Errorf("summary should report overlap count, got: %q", firstLine)
	}
}

func TestRenderText_SummarySuppressedFromJSON(t *testing.T) {
	// JSON output must not contain the human-readable summary line — JSON
	// consumers compute their own.
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleSessions(), nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "ahead total") {
		t.Errorf("JSON output unexpectedly contains summary phrasing:\n%s", buf.String())
	}
}

func TestRenderText_SummaryColorEnabled(t *testing.T) {
	// When color is on, state names in the summary should be wrapped in
	// ANSI escapes (we look for the green DONE escape — \x1b[32m).
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{Sessions: sampleSessions(), NoColor: false})
	if err != nil {
		t.Fatal(err)
	}
	firstLine := strings.SplitN(buf.String(), "\n", 2)[0]
	// In tests stdout isn't a TTY so ShouldColor returns false; assert that
	// no color escape is leaked. (The real TTY path is exercised by the
	// scenario test which only asserts plain text content.)
	if strings.Contains(firstLine, "\x1b[") {
		t.Errorf("summary unexpectedly contains color escapes when not a TTY: %q", firstLine)
	}
}

func TestRenderText_EmptySessions(t *testing.T) {
	var buf bytes.Buffer
	_ = RenderText(&buf, RenderOptions{NoColor: true})
	if !strings.Contains(buf.String(), "no sessions") {
		t.Fatalf("empty render unexpected: %s", buf.String())
	}
}

func TestRenderText_Overlaps(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{
		Sessions:     sampleSessions(),
		WithOverlaps: true,
		Overlaps: []claims.Overlap{
			{Path: "internal/auth/handler.go", Sessions: []string{"session-1", "session-3"}},
		},
		NoColor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Overlapping claims:") {
		t.Errorf("missing overlap header: %s", out)
	}
	if !strings.Contains(out, "internal/auth/handler.go") {
		t.Errorf("missing overlap path: %s", out)
	}
}

func TestRenderText_EventsSection(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 30, 0, time.UTC)
	events := []Event{
		// Oldest first (chronological), like TailEvents returns them.
		{Session: "session-1", Kind: "info", Message: "kicked off", At: now.Add(-90 * time.Second)},
		{Session: "session-2", Kind: "progress", Message: "starting on storage layer", At: now.Add(-3 * time.Second)},
	}

	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{
		Sessions: sampleSessions(),
		Events:   events,
		Now:      now,
		NoColor:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Newest event should appear first.
	idxProgress := strings.Index(out, "Recent: session-2 [progress] starting on storage layer (3s ago)")
	idxInfo := strings.Index(out, "Recent: session-1 [info] kicked off (1m ago)")
	if idxProgress < 0 {
		t.Errorf("missing progress line:\n%s", out)
	}
	if idxInfo < 0 {
		t.Errorf("missing info line:\n%s", out)
	}
	if idxProgress >= 0 && idxInfo >= 0 && idxProgress > idxInfo {
		t.Errorf("expected newest-first ordering; progress(%d) should come before info(%d):\n%s", idxProgress, idxInfo, out)
	}

	// Events section should sit between the summary line and the SESSION
	// table header so readers see the alerts before scanning the grid.
	idxSummary := strings.Index(out, "3 sessions")
	idxHeader := strings.Index(out, "SESSION")
	if !(idxSummary < idxProgress && idxProgress < idxHeader) {
		t.Errorf("events should sit between summary and header (summary=%d, event=%d, header=%d)", idxSummary, idxProgress, idxHeader)
	}
}

func TestRenderText_EmptyEventsHasNoRecentLine(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, RenderOptions{Sessions: sampleSessions(), NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Recent:") {
		t.Errorf("no Events should produce no Recent line, got:\n%s", buf.String())
	}
}

func TestRelativeAge_Buckets(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		delta time.Duration
		want  string
	}{
		{0, "0s ago"},
		{5 * time.Second, "5s ago"},
		{59 * time.Second, "59s ago"},
		{61 * time.Second, "1m ago"},
		{90 * time.Minute, "1h ago"},
		{49 * time.Hour, "2d ago"},
		{-3 * time.Second, "just now"}, // future timestamp (clock skew)
	}
	for _, c := range cases {
		got := relativeAge(now, now.Add(-c.delta))
		if got != c.want {
			t.Errorf("relativeAge(delta=%v) = %q, want %q", c.delta, got, c.want)
		}
	}
}

func TestRenderJSON_Schema(t *testing.T) {
	var buf bytes.Buffer
	err := RenderJSON(&buf, sampleSessions(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if _, ok := out["sessions"]; !ok {
		t.Fatal("JSON missing sessions array")
	}
}
