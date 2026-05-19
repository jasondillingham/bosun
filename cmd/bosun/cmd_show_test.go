package main

// Schema-lock test for `bosun show <session> --json`. Documented in
// `docs/json-schema.md` (especially F1, F2, F4 — this surface owns two
// of the live drift findings from the post-round-1 audit).

import (
	"encoding/json"
	"testing"

	"github.com/jasondillingham/bosun/internal/status"
)

// The full set of keys `bosun show --json` emits. Deliberately diverges
// from `/api/show/<session>` and from the per-session shape on
// `bosun status --json` — see F1/F2/F4 in docs/json-schema.md. The
// divergences are locked here intentionally so a future "let's add
// number/running" patch can't happen silently.
var showJSON_TopLevelKeys = []string{
	"version",
	"name",
	"branch",
	"worktree", // NB: not "path" — see F1
	"state",
	"state_msg", // NB: not "state_message" — see F2
	"ahead",
	"dirty",
	"claimed_paths",
	"recent_commits",
	"brief",
	"agent_command",
}

func TestSchema_ShowJSON_LockedKeys(t *testing.T) {
	payload := showJSON{
		Version:       status.JSONSchemaVersion,
		Name:          "session-1",
		Branch:        "bosun/session-1",
		Worktree:      "/abs/myproj-bosun-1",
		State:         "WORKING",
		StateMsg:      "",
		Ahead:         2,
		Dirty:         0,
		ClaimedPaths:  []string{"internal/auth.go"},
		RecentCommits: "abc1234 wire up handler\n",
		Brief:         "# Bosun brief — session-1\n",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	assertExactKeys(t, "show top-level", top, showJSON_TopLevelKeys)

	version, ok := top["version"].(string)
	if !ok || version != status.JSONSchemaVersion {
		t.Errorf("version: want %q, got %v (%T)", status.JSONSchemaVersion, top["version"], top["version"])
	}

	// claimed_paths must be an array, never null.
	if _, ok := top["claimed_paths"].([]any); !ok {
		t.Errorf("claimed_paths: want array, got %T (%v)", top["claimed_paths"], top["claimed_paths"])
	}

	// Lock the deliberate name divergence (see F1, F2). If these fields
	// move under different names, the assertExactKeys above will fail
	// first — but be explicit so the failure points an implementer at
	// the drift findings.
	if _, ok := top["path"]; ok {
		t.Errorf("show --json emits 'worktree', not 'path' (see docs/json-schema.md F1) — found unexpected 'path' key")
	}
	if _, ok := top["state_message"]; ok {
		t.Errorf("show --json emits 'state_msg', not 'state_message' (see docs/json-schema.md F2) — found unexpected 'state_message' key")
	}
}

func TestSchema_ShowJSON_LockedKeys_EmptyOptionalsArePresent(t *testing.T) {
	// `show --json` deliberately does NOT use omitempty for optional
	// strings. All keys above must be present even when the values are
	// empty — see docs/json-schema.md (this is one half of F5).
	payload := showJSON{
		Version:      status.JSONSchemaVersion,
		Name:         "session-1",
		Branch:       "bosun/session-1",
		Worktree:     "/abs/path",
		State:        "WORKING",
		ClaimedPaths: []string{},
		// StateMsg, RecentCommits, Brief intentionally left zero.
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"state_msg", "recent_commits", "brief"} {
		if _, ok := top[k]; !ok {
			t.Errorf("show --json must always emit %q even when empty (see F5 in docs/json-schema.md)", k)
		}
	}
	// claimed_paths must round-trip as [], not null.
	arr, ok := top["claimed_paths"].([]any)
	if !ok {
		t.Errorf("claimed_paths must be [] when empty, got %T", top["claimed_paths"])
	}
	if len(arr) != 0 {
		t.Errorf("claimed_paths: want length 0, got %d", len(arr))
	}
}
