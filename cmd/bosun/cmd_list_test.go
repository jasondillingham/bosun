package main

// Schema-lock test for `bosun list --json`. Documented in
// `docs/json-schema.md`. If a key is added, renamed, removed, or
// retyped, this test fails — and the fix is to update the doc and the
// lock list together.

import (
	"bytes"
	"encoding/json"
	"sort"
	"testing"

	"github.com/jasondillingham/bosun/internal/status"
)

var listJSON_TopLevelKeys = []string{"version", "sessions"}
var listJSON_PerSessionKeys = []string{"name", "branch", "state"}

func TestSchema_ListJSON_LockedKeys(t *testing.T) {
	payload := listJSON{
		Version: status.JSONSchemaVersion,
		Sessions: []listSessionJSON{
			{Name: "session-1", Branch: "bosun/session-1", State: "DONE"},
			{Name: "session-2", Branch: "bosun/session-2", State: "WORKING"},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	assertExactKeys(t, "list top-level", top, listJSON_TopLevelKeys)

	version, ok := top["version"].(string)
	if !ok || version != status.JSONSchemaVersion {
		t.Errorf("version: want %q, got %v (%T)", status.JSONSchemaVersion, top["version"], top["version"])
	}

	sessions, ok := top["sessions"].([]any)
	if !ok || len(sessions) != 2 {
		t.Fatalf("sessions: want 2-element array, got %T (%v)", top["sessions"], top["sessions"])
	}
	row, ok := sessions[0].(map[string]any)
	if !ok {
		t.Fatalf("sessions[0]: want object, got %T", sessions[0])
	}
	assertExactKeys(t, "list per-session", row, listJSON_PerSessionKeys)
}

func TestSchema_ListJSON_EmptySessionsIsArrayNotNull(t *testing.T) {
	// list always emits `sessions: []` (never null), so consumers don't
	// need to nil-guard the iteration.
	payload := listJSON{
		Version:  status.JSONSchemaVersion,
		Sessions: []listSessionJSON{},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"sessions":[]`)) {
		t.Errorf("empty sessions should marshal as []:\n%s", data)
	}
	if bytes.Contains(data, []byte(`"sessions":null`)) {
		t.Errorf("sessions must not be null:\n%s", data)
	}
}

// assertExactKeys checks that obj has exactly the given keys.
// Shared between cmd_list_test.go, cmd_show_test.go, and any future
// schema-lock test in this package.
func assertExactKeys(t *testing.T, label string, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	expected := append([]string(nil), want...)
	sort.Strings(expected)

	missing := keyDiff(expected, got)
	extra := keyDiff(got, expected)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	t.Errorf("%s key set mismatch — update docs/json-schema.md and this lock list when intentional.\n  want: %v\n  got:  %v\n  missing: %v\n  extra:   %v",
		label, expected, got, missing, extra)
}

func keyDiff(a, b []string) []string {
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
