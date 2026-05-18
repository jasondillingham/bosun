package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/state"
	"github.com/jasondillingham/bosun/internal/subtask"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSubtaskCancel_AdvertisedAndHappyPath covers the load-bearing
// contract: the tool shows up in ListTools (so an agent can discover
// it without operator priming) and one call against a valid in-flight
// id writes the .cancelled marker + returns cancelled=true.
func TestSubtaskCancel_AdvertisedAndHappyPath(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	sess, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer sess.Close()

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !hasTool(tools.Tools, ToolSubtaskCancel) {
		t.Fatalf("%s missing from tool list: %v", ToolSubtaskCancel, toolNames(tools.Tools))
	}

	if err := subtask.CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := callSubtaskCancel(t, sess, "session-1", "session-1.sub.1")
	if !res.Cancelled {
		t.Fatalf("happy-path cancelled = false, want true: %+v", res)
	}
	if res.ID != "session-1.sub.1" {
		t.Fatalf("result.ID = %q, want %q", res.ID, "session-1.sub.1")
	}
	marker := filepath.Join(tmp, ".bosun", "subtasks", "session-1", "session-1.sub.1.cancelled")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
}

// TestSubtaskCancel_InvalidID — the registry has no record for the
// requested id under any parent, so the tool refuses with
// cancelled=false and a detail string the caller can show the user.
func TestSubtaskCancel_InvalidID(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	sess, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer sess.Close()

	res := callSubtaskCancel(t, sess, "session-1", "no-such-id")
	if res.Cancelled {
		t.Fatalf("invalid id should refuse, got cancelled=true: %+v", res)
	}
	if res.Detail == "" {
		t.Fatalf("invalid id refusal should carry a detail string: %+v", res)
	}
}

// TestSubtaskCancel_ParentMismatch — the registry has the id under
// session-1, but session-2 tries to cancel it. Refuses without
// writing a marker.
func TestSubtaskCancel_ParentMismatch(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	sess, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer sess.Close()

	if err := subtask.CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res := callSubtaskCancel(t, sess, "session-2", "session-1.sub.1")
	if res.Cancelled {
		t.Fatalf("parent-mismatch should refuse: %+v", res)
	}
	if res.Detail == "" {
		t.Fatalf("parent-mismatch refusal should carry a detail string: %+v", res)
	}
	// And no marker should have been written under session-1.
	marker := filepath.Join(tmp, ".bosun", "subtasks", "session-1", "session-1.sub.1.cancelled")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("parent-mismatch should not write marker: err=%v", err)
	}
}

// TestSubtaskCancel_AlreadyCancelled — the second call for the same
// id returns cancelled=false (the first wrote the marker) with a
// detail string. Confirms the refusal is idempotent and doesn't
// re-touch the marker.
func TestSubtaskCancel_AlreadyCancelled(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	sess, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer sess.Close()

	if err := subtask.CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first := callSubtaskCancel(t, sess, "session-1", "session-1.sub.1")
	if !first.Cancelled {
		t.Fatalf("first call should succeed: %+v", first)
	}
	second := callSubtaskCancel(t, sess, "session-1", "session-1.sub.1")
	if second.Cancelled {
		t.Fatalf("second call should report already-cancelled: %+v", second)
	}
	if second.Detail == "" {
		t.Fatalf("already-cancelled refusal should carry a detail string: %+v", second)
	}
}

// TestSubtaskCancel_InvalidArgs — empty parent / empty id round-trip
// as refusals rather than transport errors. The same shape covers
// both because session.ParseLabel rejects each before we hit the
// registry.
func TestSubtaskCancel_InvalidArgs(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	sess, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer sess.Close()

	for _, tc := range []struct {
		name   string
		parent string
		id     string
	}{
		{"empty parent", "", "session-1.sub.1"},
		{"empty id", "session-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callSubtaskCancel(t, sess, tc.parent, tc.id)
			if res.Cancelled {
				t.Fatalf("expected refusal, got cancelled=true: %+v", res)
			}
			if res.Detail == "" {
				t.Fatalf("expected detail string on refusal: %+v", res)
			}
		})
	}
}

// callSubtaskCancel runs one bosun_subtask_cancel call and decodes
// the structured result. Refusals come back as IsError=false +
// cancelled=false in the structured payload, so we don't treat
// IsError as a fatal here.
func callSubtaskCancel(t *testing.T, sess *mcpsdk.ClientSession, parent, id string) SubtaskCancelResult {
	t.Helper()
	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      ToolSubtaskCancel,
		Arguments: map[string]any{"parent": parent, "id": id},
	})
	if err != nil {
		t.Fatalf("CallTool %s: %v", ToolSubtaskCancel, err)
	}
	var out SubtaskCancelResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &out)
	}
	return out
}
