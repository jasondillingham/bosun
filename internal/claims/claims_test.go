package claims

import (
	"os"
	"path/filepath"
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

func TestAll_SkipsPhantomClaimFiles(t *testing.T) {
	// Regression for v0.7+ kickoff observation: stale `session-1 3.json`
	// phantom inflated CLAIMED count for session-2 in `bosun status`.
	// Both Spotlight (`name N.json`) and iCloud (`name (N).json`) shapes
	// must be skipped during enumeration.
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Add("session-1", []string{"a.go"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("session-2", []string{"b.go"}); err != nil {
		t.Fatal(err)
	}

	// Drop phantom duplicates next to the real claim files.
	claimsDir := filepath.Join(dir, ".bosun", "claims")
	phantoms := []string{
		"session-1 2.json",
		"session-1 3.json",
		"session-2 (1).json",
	}
	for _, p := range phantoms {
		if err := os.WriteFile(filepath.Join(claimsDir, p), []byte(`{"session":"phantom","paths":["x"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"session-1", "session-2"}
	sort.Strings(wantKeys)
	var gotKeys []string
	for k := range all {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("All() keys = %v, want %v (phantoms must be filtered)", gotKeys, wantKeys)
	}
}

func TestIsPhantomClaimFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"session-1.json", false},
		{"session-1 2.json", true},
		{"session-1 99.json", true},
		{"session-1 (1).json", true},
		{"session-1 (12).json", true},
		{"session-1.done", false}, // wrong extension; not our concern here
		{"session 2.json", true},  // bare-name phantom is still a phantom
		{"session.json", false},
		{".lock", false},
	}
	for _, tc := range cases {
		if got := isPhantomClaimFile(tc.name); got != tc.want {
			t.Errorf("isPhantomClaimFile(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
