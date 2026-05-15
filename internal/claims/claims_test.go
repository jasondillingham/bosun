package claims

import (
	"reflect"
	"sort"
	"testing"
)

func TestAddIdempotentAndDedupes(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.Add("session-1", []string{"internal/auth/handler.go", "internal/auth/handler.go"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("session-1", []string{"internal/auth/handler.go"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Read("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.Paths); got != 1 {
		t.Fatalf("paths = %d, want 1 (got %v)", got, c.Paths)
	}
}

func TestReplaceOverwrites(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Add("session-1", []string{"a.go", "b.go"})
	_ = s.Replace("session-1", []string{"c.go"})
	c, _ := s.Read("session-1")
	if !reflect.DeepEqual(c.Paths, []string{"c.go"}) {
		t.Fatalf("paths after replace = %v, want [c.go]", c.Paths)
	}
}

func TestClearMissingIsOK(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Clear("session-99"); err != nil {
		t.Fatalf("Clear missing = %v, want nil", err)
	}
}

func TestCountFor(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Add("session-1", []string{"a", "b", "c"})
	n, err := s.CountFor(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("CountFor = %d, want 3", n)
	}
	n, err = s.CountFor(dir, "session-99")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("CountFor missing = %d, want 0", n)
	}
}

func TestOverlaps_Equality(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Add("session-1", []string{"internal/auth/handler.go"})
	_ = s.Add("session-2", []string{"internal/auth/handler.go"})
	_ = s.Add("session-3", []string{"unrelated/file.go"})

	overlaps, err := s.Overlaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(overlaps) != 1 {
		t.Fatalf("overlaps = %d, want 1: %+v", len(overlaps), overlaps)
	}
	if overlaps[0].Path != "internal/auth/handler.go" {
		t.Errorf("Path = %q", overlaps[0].Path)
	}
	sort.Strings(overlaps[0].Sessions)
	if !reflect.DeepEqual(overlaps[0].Sessions, []string{"session-1", "session-2"}) {
		t.Errorf("Sessions = %v", overlaps[0].Sessions)
	}
}

func TestOverlaps_DirectoryContainment(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Add("session-1", []string{"internal/auth/"})
	_ = s.Add("session-2", []string{"internal/auth/handler.go"})

	overlaps, err := s.Overlaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(overlaps) == 0 {
		t.Fatal("expected overlap between dir and file in dir")
	}
}

func TestOverlaps_NoneWhenDisjoint(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Add("session-1", []string{"a.go"})
	_ = s.Add("session-2", []string{"b.go"})

	overlaps, err := s.Overlaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(overlaps) != 0 {
		t.Fatalf("overlaps = %d, want 0: %+v", len(overlaps), overlaps)
	}
}

func TestNormalizeForwardSlashes(t *testing.T) {
	got := normalizeAll([]string{`internal\auth\h.go`, " ", "./foo/bar"})
	want := []string{"internal/auth/h.go", "foo/bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeAll = %v, want %v", got, want)
	}
}
