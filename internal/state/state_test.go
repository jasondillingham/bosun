package state

import (
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
