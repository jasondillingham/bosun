package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/session"
)

func TestMarkDone_ReplacesStuck(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.MarkStuck("session-1", "blocked on review"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone("session-1", "shipped"); err != nil {
		t.Fatal(err)
	}
	st, body, err := s.Read(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if st != session.StateDone {
		t.Fatalf("state = %s, want DONE", st)
	}
	if !strings.Contains(body, "shipped") {
		t.Fatalf("body missing message: %q", body)
	}
}

func TestMarkStuck_ReplacesDone(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_ = s.MarkDone("session-1", "")
	if err := s.MarkStuck("session-1", "merge conflict"); err != nil {
		t.Fatal(err)
	}
	st, body, err := s.Read(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if st != session.StateStuck {
		t.Fatalf("state = %s, want STUCK", st)
	}
	if !strings.Contains(body, "merge conflict") {
		t.Fatalf("body missing message: %q", body)
	}
}

func TestRead_Missing(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	st, body, err := s.Read(dir, "session-99")
	if err != nil {
		t.Fatal(err)
	}
	if st != session.StateWorking {
		t.Fatalf("state = %s, want WORKING", st)
	}
	if body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
}

// TestLoadAll_FiltersFinderDuplicates pins the phantom-file filter from
// roadmap ask #4: macOS Spotlight / iCloud occasionally clone marker
// files inside .bosun/state/ (`session-1 2.done`, `session-1 (1).json`,
// …). LoadAll must strip those so `bosun list` / `bosun status` don't
// surface ghost sessions.
func TestLoadAll_FiltersFinderDuplicates(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".bosun", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"session-1.done",
		"session-1 2.done",   // Spotlight duplicate
		"session-1.json",     // hypothetical .json sibling
		"session-1 2.json",   // Spotlight duplicate of json
		"session-1 (1).done", // iCloud duplicate
	} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	s := NewStore(dir)
	got, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 1 || got[0] != "session-1" {
		t.Fatalf("LoadAll = %v, want [session-1]", got)
	}
}

// TestLoadAll_MissingDir returns an empty result without erroring so
// callers iterating markers on a fresh repo don't have to special-case
// "no .bosun/state/ yet".
func TestLoadAll_MissingDir(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	got, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll on missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LoadAll = %v, want empty", got)
	}
}

// TestEnsureSpotlightMarker_CreatesAndIsIdempotent pins the marker drop
// from roadmap ask #4 — the macOS-documented filename Spotlight honors
// to stop indexing the .bosun/ directory tree. Second call is a no-op.
func TestEnsureSpotlightMarker_CreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureSpotlightMarker(dir); err != nil {
		t.Fatalf("EnsureSpotlightMarker: %v", err)
	}
	marker := filepath.Join(dir, ".bosun", ".metadata_never_index")
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("marker size = %d, want 0", info.Size())
	}
	// Second call: still nil, file unchanged.
	if err := EnsureSpotlightMarker(dir); err != nil {
		t.Fatalf("second EnsureSpotlightMarker: %v", err)
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_ = s.MarkDone("session-1", "")
	if err := s.Clear("session-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear("session-1"); err != nil {
		t.Fatalf("second Clear (missing) = %v, want nil", err)
	}
	st, _, _ := s.Read(dir, "session-1")
	if st != session.StateWorking {
		t.Fatalf("state after clear = %s, want WORKING", st)
	}
}
