package subtask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreate_HappyPath pins the registry-level contract: a single
// Create call writes a JSON file at .bosun/subtasks/<parent>/<id>.json,
// the returned record carries an ID of the form "<parent>.<12hex>",
// and the on-disk file deserializes back to that same record.
func TestCreate_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(tmp)

	rec, err := store.Create("session-1", "audit internal/git for nil-pointer risks", []string{"internal/git/"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !strings.HasPrefix(rec.ID, "session-1.") {
		t.Errorf("ID %q should start with parent.<…>", rec.ID)
	}
	if got := strings.TrimPrefix(rec.ID, "session-1."); len(got) != 12 {
		t.Errorf("ID suffix %q should be 12 hex chars; got len=%d", got, len(got))
	}
	if rec.Status != "running" {
		t.Errorf("Status should be \"running\"; got %q", rec.Status)
	}
	if rec.Started == "" {
		t.Errorf("Started should be set")
	}
	if rec.Description != "audit internal/git for nil-pointer risks" {
		t.Errorf("Description not round-tripped: %q", rec.Description)
	}
	if len(rec.Files) != 1 || rec.Files[0] != "internal/git/" {
		t.Errorf("Files not preserved: %v", rec.Files)
	}

	// Record file lands at the documented path.
	path := filepath.Join(tmp, ".bosun", "subtasks", "session-1", rec.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var back Subtask
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	if back.ID != rec.ID {
		t.Errorf("on-disk ID %q != returned ID %q", back.ID, rec.ID)
	}
}

// TestCreate_RejectsEmptyArgs guards the registry against partial
// records. The MCP gate catches these too, but keeping the store
// strict means the failure can't fall through if a future caller
// forgets to validate.
func TestCreate_RejectsEmptyArgs(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(tmp)

	if _, err := store.Create("", "desc", nil); err == nil {
		t.Error("expected error for empty parent")
	}
	if _, err := store.Create("session-1", "", nil); err == nil {
		t.Error("expected error for empty description")
	}
	if _, err := store.Create("session-1", "   ", nil); err == nil {
		t.Error("expected error for whitespace-only description")
	}
}

// TestCountActive_NoDirectoryReturnsZero pins the "no parent dir
// yet" path. The MCP quota gate calls CountActive before any
// record exists; a missing dir must not be an error.
func TestCountActive_NoDirectoryReturnsZero(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(tmp)

	n, err := store.CountActive("session-1")
	if err != nil {
		t.Fatalf("CountActive on missing dir: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0; got %d", n)
	}
}

// TestCountActive_CountsRunningRecords is the load-bearing assertion
// the MCP quota gate relies on. Five Create calls → five active;
// nothing else should slip in.
func TestCountActive_CountsRunningRecords(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(tmp)

	for i := 0; i < 5; i++ {
		if _, err := store.Create("session-1", "desc", nil); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}

	n, err := store.CountActive("session-1")
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 active; got %d", n)
	}

	// A different parent must not bleed into the count.
	if _, err := store.Create("session-2", "desc", nil); err != nil {
		t.Fatalf("Create session-2: %v", err)
	}
	n, err = store.CountActive("session-1")
	if err != nil {
		t.Fatalf("CountActive after sibling parent: %v", err)
	}
	if n != 5 {
		t.Errorf("sibling parent leaked into count; got %d", n)
	}
}

// TestCountActive_SkipsNonRunningRecords pins the schema's
// forward-compatibility: when a future lane writes a "completed"
// record into the registry, the quota gate must not double-count
// it. Simulated here by hand-writing a record with Status="completed".
func TestCountActive_SkipsNonRunningRecords(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(tmp)

	if _, err := store.Create("session-1", "running one", nil); err != nil {
		t.Fatalf("Create running: %v", err)
	}

	// Hand-write a completed record.
	dir := filepath.Join(tmp, ".bosun", "subtasks", "session-1")
	completed := Subtask{
		ID:          "session-1.aaaaaaaaaaaa",
		Parent:      "session-1",
		Description: "done one",
		Started:     "2026-05-18T00:00:00Z",
		Status:      "completed",
	}
	data, _ := json.Marshal(completed)
	if err := os.WriteFile(filepath.Join(dir, completed.ID+".json"), data, 0o644); err != nil {
		t.Fatalf("write completed record: %v", err)
	}

	n, err := store.CountActive("session-1")
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if n != 1 {
		t.Errorf("completed record should not count; got %d", n)
	}
}

// TestListActive_SortedByID gives the caller a deterministic shape
// without forcing the on-disk readdir order through every test.
func TestListActive_SortedByID(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(tmp)

	for i := 0; i < 3; i++ {
		if _, err := store.Create("session-1", "desc", nil); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}

	got, err := store.ListActive("session-1")
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3; got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID > got[i].ID {
			t.Errorf("list not sorted: %v", got)
		}
	}
}

// TestIDsAreUniqueAcrossCalls is a smoke test on the random suffix —
// 64 sequential creates should never collide if the random source is
// behaving. A failure here would mean a bug in the collision loop,
// not RNG bad luck (2^48 collision odds at 64 draws are vanishing).
func TestIDsAreUniqueAcrossCalls(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(tmp)

	seen := map[string]struct{}{}
	for i := 0; i < 64; i++ {
		rec, err := store.Create("session-1", "desc", nil)
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		if _, dup := seen[rec.ID]; dup {
			t.Fatalf("duplicate ID %q at iteration %d", rec.ID, i)
		}
		seen[rec.ID] = struct{}{}
	}
}
