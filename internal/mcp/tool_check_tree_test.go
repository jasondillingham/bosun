package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/state"
)

// TestCheckTree_UnconfiguredServerRefuses mirrors the spawn safety
// default — a server built without WithSpawnSupport refuses every
// bosun_check_tree call, no matter how the args look. Without this gate
// the tool would NPE on s.spawnTree.ChildrenOf, surfacing as a transport
// error rather than a structured tool-result error the agent can read.
func TestCheckTree_UnconfiguredServerRefuses(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	// No WithSpawnSupport call.

	result, _, err := srv.toolCheckTree(context.Background(), nil, CheckTreeArgs{Parent: "session-1"})
	if err != nil {
		t.Fatalf("toolCheckTree returned Go error (want tool-result error): %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result; got success")
	}
	if msg := toolResultText(result); !strings.Contains(msg, "not configured") {
		t.Errorf("expected 'not configured' in error; got %q", msg)
	}
}

// TestCheckTree_AuthGateRejectsNonParentCaller pins the auth contract:
// if proc.Running (via the injected fake) reports no live agent in the
// parent's worktree, the call is refused. This is the same gate #3
// bosun_spawn enforces; the wording differs but the intent is the same.
func TestCheckTree_AuthGateRejectsNonParentCaller(t *testing.T) {
	tmp := t.TempDir()
	srv := newSpawnSupportedServer(t, tmp)
	// runningFn returns ok=false for everything → no live parent.
	srv.runningFn = func(string) (int, bool) { return 0, false }

	if err := srv.spawnTree.AddTopLevel("session-1"); err != nil {
		t.Fatalf("seed parent: %v", err)
	}

	result, _, err := srv.toolCheckTree(context.Background(), nil, CheckTreeArgs{Parent: "session-1"})
	if err != nil {
		t.Fatalf("toolCheckTree returned Go error: %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected auth-gate refusal; got success")
	}
	if msg := toolResultText(result); !strings.Contains(msg, "no live agent") {
		t.Errorf("expected 'no live agent' in refusal; got %q", msg)
	}
}

// TestCheckTree_FourStates is the table-driven outcome test the brief
// requires. One Server, one parent, four children — each child seeded
// with a fixture that should land it in a distinct state.
//
// Why one Server with all four children at once: a single call to the
// tool must produce a coherent snapshot across mixed-state subs (that's
// the whole point of the tool — the parent reads one report and sees
// who's where). Driving them in isolation would let a bug in the
// child-loop slip through (e.g. always returning the last child's
// state) without ever firing a test.
func TestCheckTree_FourStates(t *testing.T) {
	tmp := t.TempDir()
	srv := newSpawnSupportedServer(t, tmp)
	cfg := *srv.cfg
	parent := "session-1"

	// Seed the spawn tree: parent + four children, one per state.
	if err := srv.spawnTree.AddTopLevel(parent); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	cases := []struct {
		label       string
		wantState   string
		wantReason  string // substring; "" → don't check
		needsWT     bool   // create the worktree dir?
		brokenAdmin bool   // create the admin dir without HEAD/commondir/gitdir?
		intactAdmin bool   // create a valid admin dir?
		markDone    bool   // drop a .done state marker?
		processAlive bool  // declare the worktree as having a live agent process?
	}{
		{
			label:        parent + ".alive",
			wantState:    CheckTreeStateAlive,
			needsWT:      true,
			intactAdmin:  true,
			processAlive: true,
		},
		{
			label:       parent + ".dead",
			wantState:   CheckTreeStateDead,
			wantReason:  "no agent process",
			needsWT:     true,
			intactAdmin: true,
		},
		{
			label:      parent + ".vanished",
			wantState:  CheckTreeStateNoLaunch,
			wantReason: "directory missing",
			// needsWT=false → worktree never created → no-launch.
		},
		{
			label:      parent + ".done",
			wantState:  CheckTreeStateDone,
			needsWT:    true,
			intactAdmin: true,
			markDone:   true,
			// processAlive intentionally false — once a sub is DONE,
			// its process state stops mattering. The test pins that
			// dominance.
		},
	}

	// Build a running-paths set the fake runningFn will check against.
	// Always include the parent so the auth gate passes.
	parentWT := session.WorktreePathForLabel(tmp, cfg, parent)
	if err := os.MkdirAll(parentWT, 0o755); err != nil {
		t.Fatalf("mkdir parent worktree: %v", err)
	}
	running := map[string]bool{parentWT: true}

	for _, tc := range cases {
		if err := srv.spawnTree.AddChild(parent, tc.label); err != nil {
			t.Fatalf("add child %s: %v", tc.label, err)
		}
		wtPath := session.WorktreePathForLabel(tmp, cfg, tc.label)
		if tc.needsWT {
			if err := os.MkdirAll(wtPath, 0o755); err != nil {
				t.Fatalf("mkdir %s worktree: %v", tc.label, err)
			}
		}
		adminDir := filepath.Join(tmp, ".git", "worktrees", filepath.Base(wtPath))
		switch {
		case tc.brokenAdmin:
			if err := os.MkdirAll(adminDir, 0o755); err != nil {
				t.Fatalf("mkdir broken admin %s: %v", tc.label, err)
			}
			// Deliberately omit HEAD/commondir/gitdir.
		case tc.intactAdmin:
			if err := os.MkdirAll(adminDir, 0o755); err != nil {
				t.Fatalf("mkdir intact admin %s: %v", tc.label, err)
			}
			for _, f := range []string{"HEAD", "commondir", "gitdir"} {
				if err := os.WriteFile(filepath.Join(adminDir, f), []byte("x\n"), 0o644); err != nil {
					t.Fatalf("write admin file %s/%s: %v", tc.label, f, err)
				}
			}
		}
		if tc.markDone {
			if err := srv.state.MarkDone(tc.label, "test"); err != nil {
				t.Fatalf("mark done %s: %v", tc.label, err)
			}
		}
		if tc.processAlive {
			running[wtPath] = true
		}
	}

	// Add a fifth child whose admin metadata is *broken* — the
	// trickier no-launch shape (issue #15 corruption). Done separately
	// so the table above stays a clean 4-state pin and this case can
	// document the second no-launch path explicitly.
	brokenLabel := parent + ".corrupt"
	if err := srv.spawnTree.AddChild(parent, brokenLabel); err != nil {
		t.Fatalf("add corrupt child: %v", err)
	}
	brokenWT := session.WorktreePathForLabel(tmp, cfg, brokenLabel)
	if err := os.MkdirAll(brokenWT, 0o755); err != nil {
		t.Fatalf("mkdir corrupt worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".git", "worktrees", filepath.Base(brokenWT)), 0o755); err != nil {
		t.Fatalf("mkdir corrupt admin: %v", err)
	}
	// No HEAD/commondir/gitdir → phantom.ScanWorktreeAdmin flags it
	// as broken → state should be no-launch with the "admin metadata
	// broken" reason.

	srv.runningFn = func(path string) (int, bool) {
		if running[path] {
			return 4242, true
		}
		return 0, false
	}

	result, structured, err := srv.toolCheckTree(context.Background(), nil, CheckTreeArgs{Parent: parent})
	if err != nil {
		t.Fatalf("toolCheckTree returned Go error: %v", err)
	}
	if isErrToolResult(result) {
		t.Fatalf("expected success; got error result: %s", toolResultText(result))
	}
	if structured.Parent != parent {
		t.Errorf("Parent = %q, want %q", structured.Parent, parent)
	}

	// Index by label so order changes in spawn-tree iteration don't
	// fail the test.
	got := map[string]CheckTreeChildResult{}
	for _, c := range structured.Children {
		got[c.Label] = c
	}

	for _, tc := range cases {
		c, ok := got[tc.label]
		if !ok {
			t.Errorf("missing child %s in result", tc.label)
			continue
		}
		if c.State != tc.wantState {
			t.Errorf("%s: state = %q, want %q (reason=%q)", tc.label, c.State, tc.wantState, c.Reason)
		}
		if tc.wantReason != "" && !strings.Contains(c.Reason, tc.wantReason) {
			t.Errorf("%s: reason = %q, want substring %q", tc.label, c.Reason, tc.wantReason)
		}
	}

	// And the broken-admin case: distinct no-launch shape from the
	// missing-worktree case the table covered.
	cb, ok := got[brokenLabel]
	if !ok {
		t.Fatalf("missing corrupt child %s in result", brokenLabel)
	}
	if cb.State != CheckTreeStateNoLaunch {
		t.Errorf("%s: state = %q, want %q", brokenLabel, cb.State, CheckTreeStateNoLaunch)
	}
	if !strings.Contains(cb.Reason, "admin metadata") {
		t.Errorf("%s: reason = %q, want substring 'admin metadata'", brokenLabel, cb.Reason)
	}

	// Text summary should mention every child by label — operator log
	// readability matters as much as the structured shape.
	text := toolResultText(result)
	for _, tc := range cases {
		if !strings.Contains(text, tc.label) {
			t.Errorf("text summary missing %s: %q", tc.label, text)
		}
	}
}

// TestCheckTree_EmptyTreeReturnsEmptyChildren pins the harmless case: a
// parent that has spawned nothing returns an empty Children slice with
// no error. Agents that call check_tree as a probe ("am I a leaf?")
// should get a clean answer.
func TestCheckTree_EmptyTreeReturnsEmptyChildren(t *testing.T) {
	tmp := t.TempDir()
	srv := newSpawnSupportedServer(t, tmp)
	parent := "session-1"
	if err := srv.spawnTree.AddTopLevel(parent); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	parentWT := session.WorktreePathForLabel(tmp, *srv.cfg, parent)
	if err := os.MkdirAll(parentWT, 0o755); err != nil {
		t.Fatalf("mkdir parent worktree: %v", err)
	}
	srv.runningFn = func(p string) (int, bool) { return 1, p == parentWT }

	result, structured, err := srv.toolCheckTree(context.Background(), nil, CheckTreeArgs{Parent: parent})
	if err != nil {
		t.Fatalf("toolCheckTree returned Go error: %v", err)
	}
	if isErrToolResult(result) {
		t.Fatalf("expected success; got: %s", toolResultText(result))
	}
	if len(structured.Children) != 0 {
		// Defensive: a non-empty Children list here would indicate the
		// tool surfaced unrelated tree entries (sibling top-levels,
		// say) instead of just this parent's direct kids.
		labels := make([]string, 0, len(structured.Children))
		for _, c := range structured.Children {
			labels = append(labels, c.Label)
		}
		sort.Strings(labels)
		t.Errorf("expected no children, got %v", labels)
	}
	if !strings.Contains(toolResultText(result), "no children") {
		t.Errorf("text summary should say 'no children'; got %q", toolResultText(result))
	}
}

// newSpawnSupportedServer assembles a Server with the spawn config
// enabled and the spawn-tree store wired. Mirrors the production
// cmd_mcp wiring closely enough that gate refusals here are the same
// shape an operator would see at runtime.
func newSpawnSupportedServer(t *testing.T, repoRoot string) *Server {
	t.Helper()
	srv := NewServer(claims.NewStore(repoRoot), state.NewStore(repoRoot), nil)
	cfg := config.Defaults()
	cfg.AgentSpawn.Enabled = true
	srv.WithSpawnSupport(cfg, spawntree.NewStore(repoRoot))
	return srv
}
