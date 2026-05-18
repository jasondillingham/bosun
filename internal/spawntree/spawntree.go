// Package spawntree tracks parent-child relationships between bosun
// sessions when agents spawn sub-sessions via the v0.9 `bosun_spawn`
// MCP tool.
//
// The data lives in .bosun/spawn-tree.json — one record per session
// with its parent (or nil for top-level), depth, and the labels of any
// children it has spawned. All writes serialize through the lockfile
// package so concurrent writers (an MCP daemon racing a CLI cleanup,
// say) can't tear the file.
//
// Top-level sessions created by `bosun init` are recorded with
// parent=nil, depth=0. Sub-sessions created by `bosun_spawn` are
// recorded with parent=<calling-session-label>, depth=parent.depth+1.
//
// The package is intentionally narrow: data access only. Quota
// enforcement, depth-limit clamping, and the MCP wiring live in their
// respective consumers (cmd/bosun, internal/mcp).
package spawntree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/lockfile"
)

const (
	// fileRelative is the path inside the repo where the spawn-tree
	// snapshot lives.
	fileRelative = ".bosun/spawn-tree.json"

	// lockRelative is the POSIX flock companion file that serializes
	// writes. Same pattern internal/state and internal/claims use.
	lockRelative = ".bosun/spawn-tree.lock"

	// schemaVersion identifies the on-disk format. Bumped on any
	// breaking change to the JSON shape; consumers should refuse to
	// load a higher version than they know.
	schemaVersion = "v1"
)

// Node is one session's place in the tree.
type Node struct {
	// Depth is 0 for top-level sessions, parent.Depth+1 for
	// sub-sessions. Used for `max_depth` quota checks.
	Depth int `json:"depth"`

	// Parent is the label of the session that spawned this one, or
	// empty for top-level sessions.
	Parent string `json:"parent,omitempty"`

	// Children is the list of sub-session labels this node has spawned.
	// Sorted alphabetically on write so the JSON output is stable.
	Children []string `json:"children,omitempty"`

	// SpawnedAt records when the node was added. RFC3339 UTC.
	SpawnedAt string `json:"spawned_at"`
}

// Tree is the on-disk snapshot — schema version plus the label→Node
// map. Operate on it through the Store, not directly.
type Tree struct {
	Version  string          `json:"version"`
	Sessions map[string]Node `json:"sessions"`
}

// Store reads and writes the spawn-tree.json file under a repo root.
// Concurrent callers (the MCP daemon racing a CLI cleanup) serialize
// through lockfile.WithLock on .bosun/spawn-tree.lock so partial-write
// races cannot tear the JSON.
type Store struct {
	repoRoot string
}

// NewStore returns a Store rooted at repoRoot. Doesn't touch disk.
func NewStore(repoRoot string) *Store { return &Store{repoRoot: repoRoot} }

func (s *Store) path() string { return filepath.Join(s.repoRoot, fileRelative) }
func (s *Store) lock() string { return filepath.Join(s.repoRoot, lockRelative) }

// Load reads the tree from disk. A missing file returns an empty
// tree (the common case before any session has spawned anything).
// A version mismatch is a hard error — callers should refuse to
// proceed rather than silently truncate state.
func (s *Store) Load() (*Tree, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Tree{Version: schemaVersion, Sessions: map[string]Node{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.path(), err)
	}
	var t Tree
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path(), err)
	}
	if t.Version == "" {
		t.Version = schemaVersion
	}
	if t.Version != schemaVersion {
		return nil, fmt.Errorf("spawn-tree version %q unsupported (this bosun expects %q); upgrade bosun or delete %s to start fresh", t.Version, schemaVersion, s.path())
	}
	if t.Sessions == nil {
		t.Sessions = map[string]Node{}
	}
	return &t, nil
}

// AddTopLevel records a session as a root (no parent, depth 0). Used
// when `bosun init` creates a fresh session — the spawn-tree wants to
// know about every session, not just spawned ones, so the depth/
// children data is complete for callers that walk the tree.
//
// Idempotent: re-adding an existing label is a no-op (preserves
// children/spawned_at).
func (s *Store) AddTopLevel(label string) error {
	return lockfile.WithLock(s.lock(), func() error {
		t, err := s.Load()
		if err != nil {
			return err
		}
		if _, ok := t.Sessions[label]; ok {
			return nil
		}
		t.Sessions[label] = Node{
			Depth:     0,
			SpawnedAt: time.Now().UTC().Format(time.RFC3339),
		}
		return s.writeLocked(t)
	})
}

// AddChild records a parent→child relationship. The parent must
// already exist in the tree (top-level sessions are added via
// AddTopLevel; sub-sessions land via AddChild from their parent's
// own AddChild call chain).
//
// Returns an error if the parent doesn't exist or the child label is
// already in the tree (collision indicates a caller bug — the
// MCP tool should refuse with a clear message before getting here).
func (s *Store) AddChild(parent, child string) error {
	return lockfile.WithLock(s.lock(), func() error {
		t, err := s.Load()
		if err != nil {
			return err
		}
		pnode, ok := t.Sessions[parent]
		if !ok {
			return fmt.Errorf("parent session %q not in spawn tree", parent)
		}
		if _, exists := t.Sessions[child]; exists {
			return fmt.Errorf("child session %q already exists in spawn tree", child)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		t.Sessions[child] = Node{
			Depth:     pnode.Depth + 1,
			Parent:    parent,
			SpawnedAt: now,
		}
		pnode.Children = appendSorted(pnode.Children, child)
		t.Sessions[parent] = pnode
		return s.writeLocked(t)
	})
}

// Remove deletes a session from the tree and unlinks it from its
// parent's Children list (if any). Used by `bosun cleanup` / `bosun
// remove` to keep the tree in sync after a session is reaped.
//
// Idempotent: removing a missing label is a no-op. Does NOT cascade
// — removing a parent with live children leaves the children
// orphaned (they retain their Parent field). Cleanup callers should
// remove in dependency order (children first) or use Cascade.
func (s *Store) Remove(label string) error {
	return lockfile.WithLock(s.lock(), func() error {
		t, err := s.Load()
		if err != nil {
			return err
		}
		node, ok := t.Sessions[label]
		if !ok {
			return nil
		}
		if node.Parent != "" {
			if p, ok := t.Sessions[node.Parent]; ok {
				p.Children = removeString(p.Children, label)
				t.Sessions[node.Parent] = p
			}
		}
		delete(t.Sessions, label)
		return s.writeLocked(t)
	})
}

// Adopt promotes label to top-level: clears its Parent reference and
// removes it from its previous parent's Children list. Used by
// `bosun adopt` when an operator decides a sub-session should
// continue independently after its parent goes away.
func (s *Store) Adopt(label string) error {
	return lockfile.WithLock(s.lock(), func() error {
		t, err := s.Load()
		if err != nil {
			return err
		}
		node, ok := t.Sessions[label]
		if !ok {
			return fmt.Errorf("session %q not in spawn tree", label)
		}
		if node.Parent == "" {
			return nil // already top-level
		}
		if p, ok := t.Sessions[node.Parent]; ok {
			p.Children = removeString(p.Children, label)
			t.Sessions[node.Parent] = p
		}
		node.Parent = ""
		node.Depth = 0
		t.Sessions[label] = node
		return s.writeLocked(t)
	})
}

// ChildrenOf returns the immediate children of label (or nil if
// label has no entry).
func (s *Store) ChildrenOf(label string) ([]string, error) {
	t, err := s.Load()
	if err != nil {
		return nil, err
	}
	node, ok := t.Sessions[label]
	if !ok {
		return nil, nil
	}
	return append([]string(nil), node.Children...), nil
}

// ParentOf returns the parent label of label, or empty for top-level
// sessions (and for sessions not in the tree).
func (s *Store) ParentOf(label string) (string, error) {
	t, err := s.Load()
	if err != nil {
		return "", err
	}
	node, ok := t.Sessions[label]
	if !ok {
		return "", nil
	}
	return node.Parent, nil
}

// DepthOf returns label's depth in the tree, or 0 if not present.
func (s *Store) DepthOf(label string) (int, error) {
	t, err := s.Load()
	if err != nil {
		return 0, err
	}
	return t.Sessions[label].Depth, nil
}

// CountChildren returns the number of live children of label. The
// MCP spawn tool uses this for max_concurrent_sub_sessions quota
// enforcement.
func (s *Store) CountChildren(label string) (int, error) {
	kids, err := s.ChildrenOf(label)
	if err != nil {
		return 0, err
	}
	return len(kids), nil
}

// EnrichSessions populates Parent / Children / Depth on each entry in
// sessions by looking up its Label in this tree. Sessions not in the
// tree are left at their zero values (top-level, depth 0, no
// children). Designed as the bridge for renderers — status, list,
// show — that need tree info without their own spawntree imports
// at every call site.
//
// Lives in spawntree (not internal/session) so internal/session stays
// independent of the spawn-tree package. Callers that don't care
// about tree info just skip the call.
//
// Uses a SessionLike interface so spawntree doesn't have to import
// internal/session and risk a cycle. The shape matches session.Session
// exactly; any future consumer can satisfy it without dragging in the
// whole session package.
func (s *Store) EnrichSessions(sessions []SessionLike) error {
	if len(sessions) == 0 {
		return nil
	}
	t, err := s.Load()
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		node, ok := t.Sessions[sess.GetLabel()]
		if !ok {
			continue
		}
		sess.SetTreeInfo(node.Parent, append([]string(nil), node.Children...), node.Depth)
	}
	return nil
}

// SessionLike is the slice element shape EnrichSessions consumes. A
// pointer to session.Session satisfies it via the SetTreeInfo /
// GetLabel methods session.Session implements.
type SessionLike interface {
	GetLabel() string
	SetTreeInfo(parent string, children []string, depth int)
}

// GitProbe is the narrow surface SyncWithGit needs from a git client.
// *git.Client satisfies it via its existing methods; tests stub it
// with a fake so the worktree/branch presence matrix can be driven
// without shelling out to the real binary.
type GitProbe interface {
	ListWorktrees(ctx context.Context, dir string) ([]git.Worktree, error)
	BranchExists(ctx context.Context, dir, branch string) (bool, error)
}

// PrunedLabel names a spawn-tree entry that SyncWithGit removed.
// Aliased to string so callers can range-and-print without
// destructuring; kept as a named type so future iterations can
// carry per-entry rationale (e.g. "both missing" vs operator-only
// asymmetries) without a signature break.
type PrunedLabel = string

// SyncWithGit prunes spawn-tree entries whose worktree AND branch
// are both missing from git — the "ghost" shape trial #3c found on
// macOS / iCloud File Provider where external file-system providers
// silently reap worktree directories and branches. When only ONE of
// the two is missing the entry is left alone: the operator may be
// mid-rename or have just deleted one side intentionally, and
// silently rewriting the tree under their feet would surprise more
// than help.
//
// The branch each entry is checked against is `bosun/<label>` — the
// canonical name bosun assigns when creating a session. Worktree
// presence is detected by matching `refs/heads/bosun/<label>` against
// the branch fields of `git worktree list` so we don't depend on the
// worktree path naming scheme (suffix patterns vary by config).
//
// Returns the pruned labels (sorted) so callers can surface them once
// in the next render. Idempotent: a second call after the first
// returns the same state with no further prune.
//
// repoRoot is the dir from which git is invoked. Passed explicitly
// rather than reusing s.repoRoot so callers that already hold a
// canonical main-worktree path (cmd_status, cmd_cleanup, cmd_merge)
// don't have to re-resolve it.
func (s *Store) SyncWithGit(ctx context.Context, gc GitProbe, repoRoot string) ([]PrunedLabel, error) {
	var pruned []PrunedLabel
	err := lockfile.WithLock(s.lock(), func() error {
		t, err := s.Load()
		if err != nil {
			return err
		}
		if len(t.Sessions) == 0 {
			return nil
		}
		worktrees, err := gc.ListWorktrees(ctx, repoRoot)
		if err != nil {
			return fmt.Errorf("list worktrees: %w", err)
		}
		mounted := make(map[string]bool, len(worktrees))
		for _, w := range worktrees {
			if w.Branch != "" {
				mounted[w.Branch] = true
			}
		}
		// Sort label lookup so prune order — and the returned slice —
		// is deterministic. Keeps test assertions and operator-facing
		// stderr output stable across runs.
		labels := make([]string, 0, len(t.Sessions))
		for label := range t.Sessions {
			labels = append(labels, label)
		}
		sort.Strings(labels)

		changed := false
		for _, label := range labels {
			branch := "bosun/" + label
			ref := "refs/heads/" + branch
			if mounted[ref] {
				continue
			}
			exists, berr := gc.BranchExists(ctx, repoRoot, branch)
			if berr != nil {
				return fmt.Errorf("check branch %s: %w", branch, berr)
			}
			if exists {
				continue
			}
			node := t.Sessions[label]
			if node.Parent != "" {
				if p, ok := t.Sessions[node.Parent]; ok {
					p.Children = removeString(p.Children, label)
					t.Sessions[node.Parent] = p
				}
			}
			delete(t.Sessions, label)
			pruned = append(pruned, label)
			changed = true
		}
		if !changed {
			return nil
		}
		return s.writeLocked(t)
	})
	if err != nil {
		return nil, err
	}
	return pruned, nil
}

// writeLocked persists the tree to disk. Caller must hold the flock
// — invoked only inside lockfile.WithLock callbacks above.
func (s *Store) writeLocked(t *Tree) error {
	if err := os.MkdirAll(filepath.Dir(s.path()), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(s.path()), err)
	}
	t.Version = schemaVersion
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spawn tree: %w", err)
	}
	data = append(data, '\n')
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, s.path(), err)
	}
	return nil
}

// appendSorted inserts s into the slice and keeps the result sorted
// + de-duplicated. Used so Children lists serialize deterministically
// regardless of insertion order.
func appendSorted(slice []string, s string) []string {
	for _, x := range slice {
		if x == s {
			return slice
		}
	}
	slice = append(slice, s)
	sort.Strings(slice)
	return slice
}

func removeString(slice []string, target string) []string {
	out := slice[:0]
	for _, s := range slice {
		if s != target {
			out = append(out, s)
		}
	}
	return out
}
