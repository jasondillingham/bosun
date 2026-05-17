package spawntree

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func TestLoad_MissingFileReturnsEmptyTree(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	tree, err := s.Load()
	if err != nil {
		t.Fatalf("Load on empty repo: %v", err)
	}
	if tree.Version != schemaVersion {
		t.Errorf("Version = %q, want %q", tree.Version, schemaVersion)
	}
	if len(tree.Sessions) != 0 {
		t.Errorf("Sessions = %+v, want empty", tree.Sessions)
	}
}

func TestAddTopLevel_RecordsDepthZero(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.AddTopLevel("session-1"); err != nil {
		t.Fatalf("AddTopLevel: %v", err)
	}
	tree, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	n, ok := tree.Sessions["session-1"]
	if !ok {
		t.Fatal("session-1 missing from tree")
	}
	if n.Depth != 0 || n.Parent != "" {
		t.Errorf("Depth=%d Parent=%q, want 0/empty", n.Depth, n.Parent)
	}
	if n.SpawnedAt == "" {
		t.Error("SpawnedAt unset")
	}
}

func TestAddTopLevel_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.AddTopLevel("session-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddChild("session-1", "session-1.auth"); err != nil {
		t.Fatal(err)
	}
	// Re-adding the top-level must not erase the child.
	if err := s.AddTopLevel("session-1"); err != nil {
		t.Fatalf("re-AddTopLevel: %v", err)
	}
	kids, _ := s.ChildrenOf("session-1")
	if !reflect.DeepEqual(kids, []string{"session-1.auth"}) {
		t.Errorf("children after re-add = %v, want [session-1.auth]", kids)
	}
}

func TestAddChild_LinksBothSides(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.AddTopLevel("session-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddChild("session-1", "session-1.auth"); err != nil {
		t.Fatal(err)
	}

	tree, _ := s.Load()

	parent := tree.Sessions["session-1"]
	if !reflect.DeepEqual(parent.Children, []string{"session-1.auth"}) {
		t.Errorf("parent.Children = %v, want [session-1.auth]", parent.Children)
	}

	child := tree.Sessions["session-1.auth"]
	if child.Parent != "session-1" {
		t.Errorf("child.Parent = %q, want session-1", child.Parent)
	}
	if child.Depth != 1 {
		t.Errorf("child.Depth = %d, want 1", child.Depth)
	}
}

func TestAddChild_RefusesMissingParent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	err := s.AddChild("session-ghost", "session-ghost.kid")
	if err == nil {
		t.Fatal("expected error when parent doesn't exist")
	}
}

func TestAddChild_RefusesDuplicateChild(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.AddTopLevel("session-1")
	_ = s.AddChild("session-1", "session-1.auth")
	err := s.AddChild("session-1", "session-1.auth")
	if err == nil {
		t.Fatal("expected error on duplicate child")
	}
}

func TestAddChild_DepthAccumulates(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.AddTopLevel("session-1")
	_ = s.AddChild("session-1", "session-1.auth")
	if err := s.AddChild("session-1.auth", "session-1.auth.parser"); err != nil {
		t.Fatal(err)
	}
	depth, _ := s.DepthOf("session-1.auth.parser")
	if depth != 2 {
		t.Errorf("grandchild depth = %d, want 2", depth)
	}
}

func TestRemove_UnlinksFromParent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.AddTopLevel("session-1")
	_ = s.AddChild("session-1", "session-1.auth")
	_ = s.AddChild("session-1", "session-1.http")

	if err := s.Remove("session-1.auth"); err != nil {
		t.Fatal(err)
	}
	kids, _ := s.ChildrenOf("session-1")
	if !reflect.DeepEqual(kids, []string{"session-1.http"}) {
		t.Errorf("children after remove = %v, want [session-1.http]", kids)
	}
}

func TestRemove_MissingLabelIsNoop(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Remove("nobody-home"); err != nil {
		t.Errorf("Remove on missing label should be no-op, got %v", err)
	}
}

func TestAdopt_PromotesToTopLevel(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.AddTopLevel("session-1")
	_ = s.AddChild("session-1", "session-1.auth")

	if err := s.Adopt("session-1.auth"); err != nil {
		t.Fatal(err)
	}
	parent, _ := s.ParentOf("session-1.auth")
	if parent != "" {
		t.Errorf("after adopt, parent = %q, want empty", parent)
	}
	depth, _ := s.DepthOf("session-1.auth")
	if depth != 0 {
		t.Errorf("after adopt, depth = %d, want 0", depth)
	}
	// Old parent's children list must no longer include the adoptee.
	kids, _ := s.ChildrenOf("session-1")
	for _, k := range kids {
		if k == "session-1.auth" {
			t.Errorf("old parent still lists adopted child")
		}
	}
}

func TestCountChildren(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.AddTopLevel("session-1")
	if n, _ := s.CountChildren("session-1"); n != 0 {
		t.Errorf("count with no kids = %d, want 0", n)
	}
	_ = s.AddChild("session-1", "session-1.a")
	_ = s.AddChild("session-1", "session-1.b")
	if n, _ := s.CountChildren("session-1"); n != 2 {
		t.Errorf("count with 2 kids = %d, want 2", n)
	}
}

func TestVersionMismatch_RefusesLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	bogus := `{"version":"v999","sessions":{}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, fileRelative), []byte(bogus), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if _, err := s.Load(); err == nil {
		t.Fatal("expected error on version mismatch")
	}
}

// TestConcurrentChildAdds_NoTear exercises the flock — multiple
// goroutines adding children to the same parent must not lose
// updates or produce a torn JSON file.
func TestConcurrentChildAdds_NoTear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.AddTopLevel("session-1")

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			child := "session-1.c" + string(rune('a'+i))
			if err := s.AddChild("session-1", child); err != nil {
				t.Errorf("AddChild %s: %v", child, err)
			}
		}(i)
	}
	wg.Wait()

	kids, err := s.ChildrenOf("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != n {
		t.Fatalf("children after concurrent adds = %d, want %d (%+v)", len(kids), n, kids)
	}
	// Must be sorted (writeLocked normalizes via appendSorted).
	if !sort.StringsAreSorted(kids) {
		t.Errorf("children not sorted: %v", kids)
	}
}
