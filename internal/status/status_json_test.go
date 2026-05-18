package status

// Schema-lock tests for the `bosun status --json` / `GET /api/status`
// surface. Documented in `docs/json-schema.md`. If you change a key name,
// add a new key, flip omitempty, or change a type, these tests fail —
// and the fix is to update the doc and the lock fixture together.

import (
	"bytes"
	"encoding/json"
	"sort"
	"testing"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/session"
)

// lockedKeys for the top-level payload. The order here is the order the
// doc records; that's also the JSON struct tag order. We compare sorted
// sets, so the order in this list is informational only.
var statusJSON_TopLevelKeys_WithOverlaps = []string{"sessions", "overlaps"}
var statusJSON_TopLevelKeys_WithoutOverlaps = []string{"sessions"}

// Per-session keys when every optional field is populated. The
// omitempty fields (`running_pid`, `last_*`, `state_message`,
// `parent`, `children`, `depth`) appear only when non-zero — the
// omitempty-collapses test below proves the other half of the
// contract.
var statusJSON_PerSessionKeys_Full = []string{
	"name", "number", "branch", "path", "state",
	"ahead", "dirty", "claimed", "running",
	"running_pid",
	"last_sha", "last_subject", "last_relative", "last_unix",
	"state_message",
	"parent", "children", "depth",
	"subtasks",
}

// Per-session keys for a session with no commits, no claims, no agent
// process, no state message. The omitempty fields must all be absent.
var statusJSON_PerSessionKeys_Minimal = []string{
	"name", "number", "branch", "path", "state",
	"ahead", "dirty", "claimed", "running",
}

func TestSchema_StatusJSON_LockedKeys_AllFieldsPopulated(t *testing.T) {
	sessions := []session.Session{{
		Number: 1, Name: "session-1", Branch: "bosun/session-1",
		Path: "/abs/myproj-bosun-1", State: session.StateDone, StateMsg: "shipped",
		Ahead: 2, Dirty: 0, Claimed: 3,
		Running: true, RunningPID: 12345,
		Last:     &git.LogEntry{ShortSHA: "abc1234", Subject: "implement auth", Relative: "3m ago", Unix: 1700000000},
		Parent:   "session-0",
		Children: []string{"session-1.auth"},
		Depth:    1,
		Subtasks: 3,
	}}
	overlaps := []claims.Overlap{
		{Path: "internal/auth.go", Sessions: []string{"session-1", "session-3"}},
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, sessions, overlaps, true); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	assertKeySet(t, "status top-level (with overlaps)", top, statusJSON_TopLevelKeys_WithOverlaps)

	sessionsArr, ok := top["sessions"].([]any)
	if !ok || len(sessionsArr) != 1 {
		t.Fatalf("sessions: want 1-element array, got %T (%v)", top["sessions"], top["sessions"])
	}
	row, ok := sessionsArr[0].(map[string]any)
	if !ok {
		t.Fatalf("sessions[0]: want object, got %T", sessionsArr[0])
	}
	assertKeySet(t, "status per-session (full)", row, statusJSON_PerSessionKeys_Full)

	// Spot-check types so a future refactor that, say, turns `ahead` into
	// a string fails here too — `assertKeySet` only checks key names.
	if _, ok := row["number"].(float64); !ok {
		t.Errorf("number: want number, got %T (%v)", row["number"], row["number"])
	}
	if _, ok := row["running"].(bool); !ok {
		t.Errorf("running: want bool, got %T (%v)", row["running"], row["running"])
	}
	if _, ok := row["last_unix"].(float64); !ok {
		t.Errorf("last_unix: want number, got %T (%v)", row["last_unix"], row["last_unix"])
	}

	overlapsArr, ok := top["overlaps"].([]any)
	if !ok || len(overlapsArr) != 1 {
		t.Fatalf("overlaps: want 1-element array, got %T", top["overlaps"])
	}
	overlap, ok := overlapsArr[0].(map[string]any)
	if !ok {
		t.Fatalf("overlaps[0]: want object")
	}
	assertKeySet(t, "status overlap entry", overlap, []string{"path", "sessions"})
}

func TestSchema_StatusJSON_LockedKeys_OmitemptyCollapses(t *testing.T) {
	// A minimal session: no commits, not running, no claims, no state
	// message. The omitempty fields must disappear and `overlaps` must
	// not be present (withOverlaps=false).
	sessions := []session.Session{{
		Number: 3, Name: "session-3", Branch: "bosun/session-3",
		Path: "/abs/myproj-bosun-3", State: session.StateWorking,
	}}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, sessions, nil, false); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	assertKeySet(t, "status top-level (no overlaps)", top, statusJSON_TopLevelKeys_WithoutOverlaps)

	sessionsArr := top["sessions"].([]any)
	row := sessionsArr[0].(map[string]any)
	assertKeySet(t, "status per-session (minimal)", row, statusJSON_PerSessionKeys_Minimal)
}

func TestSchema_StatusJSON_LockedKeys_EmptySessionsIsArrayNotNull(t *testing.T) {
	// `sessions` must marshal as `[]`, never `null`, even on an empty
	// repo. Consumers iterate without nil-guarding.
	var buf bytes.Buffer
	if err := RenderJSON(&buf, nil, nil, false); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"sessions": []`)) {
		t.Errorf("empty sessions should marshal as []:\n%s", buf.String())
	}
	if bytes.Contains(buf.Bytes(), []byte(`"sessions": null`)) {
		t.Errorf("sessions must not be null:\n%s", buf.String())
	}
}

func TestSchema_StatusJSON_LockedKeys_VersionDeliberatelyAbsent(t *testing.T) {
	// The status surface predates JSONSchemaVersion. Its contract is the
	// doc comment in status_json.go, not an embedded version. This test
	// locks that decision in so a future "let's add version everywhere"
	// patch can't sneak it in without a deliberate doc update.
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleSessions(), nil, false); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := top["version"]; ok {
		t.Errorf("status --json must NOT emit a top-level 'version' field (see docs/json-schema.md F3):\n%s", buf.String())
	}
}

// assertKeySet asserts that obj has exactly the given keys (sorted-set
// equality). Reports both missing and extra keys.
func assertKeySet(t *testing.T, label string, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	expected := append([]string(nil), want...)
	sort.Strings(expected)

	missing := diff(expected, got)
	extra := diff(got, expected)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	t.Errorf("%s key set mismatch — update docs/json-schema.md and this lock list when intentional.\n  want: %v\n  got:  %v\n  missing: %v\n  extra:   %v",
		label, expected, got, missing, extra)
}

// diff returns the elements in a that are not in b.
func diff(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, x := range b {
		set[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := set[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}
