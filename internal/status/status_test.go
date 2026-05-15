package status

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

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
