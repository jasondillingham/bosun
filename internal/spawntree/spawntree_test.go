package spawntree

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jasondillingham/bosun/internal/git"
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

// writeTreeJSON drops a hand-crafted spawn-tree.json into the store's
// .bosun/ dir. Used by the validateTree tests below to seed pathological
// states the in-process API would normally refuse to produce.
func writeTreeJSON(t *testing.T, repoRoot string, body string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spawn-tree.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write spawn-tree.json: %v", err)
	}
}

// TestLoad_RefusesSelfParent pins the validation gate: a node listing
// itself as parent (corrupt JSON, hand-edited file) must be refused
// instead of silently loaded — otherwise ParentOf walks indefinitely.
func TestLoad_RefusesSelfParent(t *testing.T) {
	dir := t.TempDir()
	writeTreeJSON(t, dir, `{"version":"v1","sessions":{"session-1":{"depth":0,"parent":"session-1","spawned_at":"2026-01-01T00:00:00Z"}}}`)
	_, err := NewStore(dir).Load()
	if err == nil {
		t.Fatal("expected error on self-parent, got nil")
	}
	if !strings.Contains(err.Error(), "lists itself as parent") {
		t.Errorf("err %v should mention self-parent", err)
	}
}

// TestLoad_RefusesOrphanedChild: a node references a parent that
// doesn't exist in the Sessions map. Loading silently would leave
// EnrichSessions to crash on the missing entry.
func TestLoad_RefusesOrphanedChild(t *testing.T) {
	dir := t.TempDir()
	writeTreeJSON(t, dir, `{"version":"v1","sessions":{"orphan":{"depth":1,"parent":"ghost","spawned_at":"2026-01-01T00:00:00Z"}}}`)
	_, err := NewStore(dir).Load()
	if err == nil {
		t.Fatal("expected error on orphan-with-missing-parent, got nil")
	}
	if !strings.Contains(err.Error(), `references parent "ghost"`) {
		t.Errorf("err %v should mention missing parent", err)
	}
}

// TestLoad_RefusesParentCycle: A → B → A. Walking parents would loop.
func TestLoad_RefusesParentCycle(t *testing.T) {
	dir := t.TempDir()
	body := `{"version":"v1","sessions":{
"a":{"depth":1,"parent":"b","spawned_at":"2026-01-01T00:00:00Z"},
"b":{"depth":1,"parent":"a","spawned_at":"2026-01-01T00:00:00Z"}
}}`
	writeTreeJSON(t, dir, body)
	_, err := NewStore(dir).Load()
	if err == nil {
		t.Fatal("expected error on parent cycle, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("err %v should mention cycle", err)
	}
}

// TestLoad_AcceptsValidTree confirms the validation doesn't false-
// positive on a well-formed three-level tree.
func TestLoad_AcceptsValidTree(t *testing.T) {
	dir := t.TempDir()
	body := `{"version":"v1","sessions":{
"root":{"depth":0,"children":["mid"],"spawned_at":"2026-01-01T00:00:00Z"},
"mid":{"depth":1,"parent":"root","children":["leaf"],"spawned_at":"2026-01-01T00:00:00Z"},
"leaf":{"depth":2,"parent":"mid","spawned_at":"2026-01-01T00:00:00Z"}
}}`
	writeTreeJSON(t, dir, body)
	tree, err := NewStore(dir).Load()
	if err != nil {
		t.Fatalf("Load on valid tree: %v", err)
	}
	if len(tree.Sessions) != 3 {
		t.Errorf("got %d sessions, want 3", len(tree.Sessions))
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
	// Message should name the offending label and point operators at
	// the next step. Tightened 2026-05-21 (bug-hunt pass-2 follow-up).
	msg := err.Error()
	for _, want := range []string{`"session-1.auth"`, "already exists", "bosun status", "bosun cleanup"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\n  got: %s", want, msg)
		}
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

// fakeGitProbe stubs the GitProbe interface for SyncWithGit tests.
// `branches` is the set of branches (without the refs/heads/ prefix)
// reported as live by BranchExists. `worktreeBranches` is the set of
// branches reported as mounted in `git worktree list` (full ref name,
// e.g. "refs/heads/bosun/session-1"). Either set being missing means
// the corresponding probe returns "not present."
type fakeGitProbe struct {
	worktreeBranches map[string]bool
	branches         map[string]bool
}

func (f *fakeGitProbe) ListWorktrees(_ context.Context, _ string) ([]git.Worktree, error) {
	out := make([]git.Worktree, 0, len(f.worktreeBranches))
	// Sort so the slice is deterministic — saves a flaky-test debug
	// session later if any consumer ever cares about order.
	branches := make([]string, 0, len(f.worktreeBranches))
	for b := range f.worktreeBranches {
		branches = append(branches, b)
	}
	sort.Strings(branches)
	for _, b := range branches {
		out = append(out, git.Worktree{Branch: b, Path: "/fake/" + b})
	}
	return out, nil
}

func (f *fakeGitProbe) BranchExists(_ context.Context, _, branch string) (bool, error) {
	return f.branches[branch], nil
}

// TestSyncWithGit covers the matrix described in trial #3c Bug A: an
// entry is "ghost" — and only then pruned — when its worktree is gone
// from `git worktree list` AND its branch is gone from `git branch
// --list bosun/<label>`. Asymmetric divergence (one side missing,
// the other present) is left untouched so bosun doesn't second-guess
// an operator mid-move.
func TestSyncWithGit(t *testing.T) {
	type seed struct {
		topLevels []string
		children  [][2]string // {parent, child}
	}
	tests := []struct {
		name             string
		seed             seed
		worktreeBranches map[string]bool // full refs/heads/... names
		branches         map[string]bool // bare branch names (no refs/heads/)
		wantPruned       []PrunedLabel
		wantRemaining    []string
	}{
		{
			name: "intact tree — no prune",
			seed: seed{
				topLevels: []string{"session-1"},
			},
			worktreeBranches: map[string]bool{"refs/heads/bosun/session-1": true},
			branches:         map[string]bool{"bosun/session-1": true},
			wantPruned:       nil,
			wantRemaining:    []string{"session-1"},
		},
		{
			name: "worktree missing, branch present — leave alone",
			seed: seed{
				topLevels: []string{"session-1"},
			},
			// No worktree, but branch still exists. Operator may have
			// just removed the worktree intentionally — don't rewrite.
			worktreeBranches: map[string]bool{},
			branches:         map[string]bool{"bosun/session-1": true},
			wantPruned:       nil,
			wantRemaining:    []string{"session-1"},
		},
		{
			name: "branch missing, worktree present — leave alone",
			seed: seed{
				topLevels: []string{"session-1"},
			},
			// Worktree mounted but branch deleted. Operator may be
			// mid-rename — don't rewrite.
			worktreeBranches: map[string]bool{"refs/heads/bosun/session-1": true},
			branches:         map[string]bool{},
			wantPruned:       nil,
			wantRemaining:    []string{"session-1"},
		},
		{
			name: "both missing — prune",
			seed: seed{
				topLevels: []string{"session-1"},
			},
			worktreeBranches: map[string]bool{},
			branches:         map[string]bool{},
			wantPruned:       []PrunedLabel{"session-1"},
			wantRemaining:    nil,
		},
		{
			name: "nested tree — prune ghost child, keep live parent",
			seed: seed{
				topLevels: []string{"session-1"},
				children:  [][2]string{{"session-1", "session-1.auth"}},
			},
			// Parent's branch + worktree intact; child both gone.
			worktreeBranches: map[string]bool{"refs/heads/bosun/session-1": true},
			branches:         map[string]bool{"bosun/session-1": true},
			wantPruned:       []PrunedLabel{"session-1.auth"},
			wantRemaining:    []string{"session-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := NewStore(dir)
			for _, tl := range tc.seed.topLevels {
				if err := s.AddTopLevel(tl); err != nil {
					t.Fatalf("seed AddTopLevel %s: %v", tl, err)
				}
			}
			for _, c := range tc.seed.children {
				if err := s.AddChild(c[0], c[1]); err != nil {
					t.Fatalf("seed AddChild %s->%s: %v", c[0], c[1], err)
				}
			}

			gc := &fakeGitProbe{
				worktreeBranches: tc.worktreeBranches,
				branches:         tc.branches,
			}

			pruned, err := s.SyncWithGit(context.Background(), gc, dir)
			if err != nil {
				t.Fatalf("SyncWithGit: %v", err)
			}
			if !reflect.DeepEqual(pruned, tc.wantPruned) {
				t.Errorf("pruned = %v, want %v", pruned, tc.wantPruned)
			}

			tree, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			var gotLabels []string
			for l := range tree.Sessions {
				gotLabels = append(gotLabels, l)
			}
			sort.Strings(gotLabels)
			want := append([]string(nil), tc.wantRemaining...)
			sort.Strings(want)
			if !reflect.DeepEqual(gotLabels, want) {
				t.Errorf("remaining = %v, want %v", gotLabels, want)
			}

			// Nested case: confirm the parent's Children list dropped
			// the ghost. Otherwise a stale child name lingers in the
			// JSON and breaks postOrderSubtree walks.
			if tc.name == "nested tree — prune ghost child, keep live parent" {
				kids, _ := s.ChildrenOf("session-1")
				if len(kids) != 0 {
					t.Errorf("parent still lists ghost child: %v", kids)
				}
			}
		})
	}
}

// TestSyncWithGit_Idempotent guarantees a second call after a prune
// produces no further mutations and no further reports — the brief
// requires the operator stderr line to fire only the first time.
func TestSyncWithGit_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.AddTopLevel("session-1"); err != nil {
		t.Fatal(err)
	}
	gc := &fakeGitProbe{worktreeBranches: map[string]bool{}, branches: map[string]bool{}}

	first, err := s.SyncWithGit(context.Background(), gc, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first sync = %v, want one prune", first)
	}
	second, err := s.SyncWithGit(context.Background(), gc, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("second sync = %v, want no further prunes", second)
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
