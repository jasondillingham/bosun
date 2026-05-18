package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/phantom"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CheckTree child-state vocabulary. Kept as named constants so the wire
// shape doesn't drift if a tool author touches the struct literals
// inside evaluateChild without realizing they're public protocol.
const (
	CheckTreeStateAlive    = "alive"
	CheckTreeStateDead     = "dead"
	CheckTreeStateNoLaunch = "no-launch"
	CheckTreeStateDone     = "done"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name: "bosun_check_tree",
			Description: "Inspect the live state of every sub-session a parent has spawned. " +
				"Returns one entry per direct child with state in {alive, dead, no-launch, done} so " +
				"the parent agent can spot subs that have vanished (issue #15 worktree corruption) " +
				"or died (launcher failure) before silently taking on their work. Auth: only the " +
				"parent's own running agent may query — same proc.Running gate bosun_spawn uses.",
		}, s.toolCheckTree)
	})
}

// CheckTreeArgs is the input schema for bosun_check_tree.
type CheckTreeArgs struct {
	Parent string `json:"parent" jsonschema:"name of the parent session whose children to check"`
}

// CheckTreeChildResult is one row of the per-child report.
type CheckTreeChildResult struct {
	Label  string `json:"label"`
	State  string `json:"state"` // "alive" | "dead" | "no-launch" | "done"
	Reason string `json:"reason,omitempty"`
}

// CheckTreeResult is the structured tool output.
type CheckTreeResult struct {
	Parent   string                 `json:"parent"`
	Children []CheckTreeChildResult `json:"children"`
}

// toolCheckTree implements bosun_check_tree. Mirrors bosun_spawn's gate
// pattern: the daemon must be wired for spawn (so the spawn-tree exists),
// and the caller must be running inside the parent's worktree. The
// per-child evaluation lives in evaluateChild for unit-test isolation.
func (s *Server) toolCheckTree(_ context.Context, _ *mcp.CallToolRequest, args CheckTreeArgs) (*mcp.CallToolResult, CheckTreeResult, error) {
	if s.spawnTree == nil || s.cfg == nil {
		return errResult(errors.New("bosun_check_tree is not configured on this daemon (operator must wire WithSpawnSupport in cmd_mcp)")), CheckTreeResult{}, nil
	}

	parentRaw := strings.TrimSpace(args.Parent)
	if parentRaw == "" {
		return errResult(errors.New("parent is required")), CheckTreeResult{}, nil
	}
	parent, err := session.ParseLabel(parentRaw)
	if err != nil {
		return errResult(err), CheckTreeResult{}, nil
	}

	repoRoot := s.state.RepoRoot()
	parentWorktree := session.WorktreePathForLabel(repoRoot, *s.cfg, parent, "")
	if _, running := s.runningFn(parentWorktree); !running {
		return errResult(fmt.Errorf("no live agent detected in %s's worktree; bosun_check_tree requires the caller to be running inside the named parent", parent)), CheckTreeResult{}, nil
	}

	kids, err := s.spawnTree.ChildrenOf(parent)
	if err != nil {
		return errResult(fmt.Errorf("read spawn tree: %w", err)), CheckTreeResult{}, nil
	}

	// One admin-scan up front; per-child evaluation only checks set
	// membership. A scan failure (e.g. .git/worktrees/ unreadable for
	// real reasons, not just absent) is non-fatal — broken-admin
	// detection degrades to "we couldn't tell", but worktree-missing
	// detection still works.
	scan, _ := phantom.ScanWorktreeAdmin(repoRoot)
	brokenAdmins := make(map[string]struct{}, len(scan.BrokenDirs))
	for _, d := range scan.BrokenDirs {
		brokenAdmins[filepath.Base(d)] = struct{}{}
	}

	out := CheckTreeResult{
		Parent:   parent,
		Children: make([]CheckTreeChildResult, 0, len(kids)),
	}
	for _, label := range kids {
		out.Children = append(out.Children, s.evaluateChild(repoRoot, label, brokenAdmins))
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summarizeTree(out)}},
	}, out, nil
}

// evaluateChild computes one child's CheckTree state. The order of
// checks is the published priority:
//
//  1. `done` — operator-visible DONE marker dominates everything; once
//     a sub has shipped, its process state doesn't matter.
//  2. `no-launch` — worktree dir missing OR its git admin metadata is
//     broken (issue #15 shape). Distinguishes "never launched / lost"
//     from "ran and then crashed" so the parent can tell the operator
//     which corruption mode they're looking at.
//  3. `alive` — a live agent process is running in the worktree.
//  4. `dead` — fallthrough: worktree exists, admin intact, no process.
//
// The repoRoot + cfg are pulled from the receiver so this stays callable
// from the test harness without re-wiring the world.
func (s *Server) evaluateChild(repoRoot, label string, brokenAdmins map[string]struct{}) CheckTreeChildResult {
	donePath := filepath.Join(repoRoot, ".bosun", "state", label+".done")
	if _, err := os.Stat(donePath); err == nil {
		return CheckTreeChildResult{Label: label, State: CheckTreeStateDone}
	}

	worktreePath := session.WorktreePathForLabel(repoRoot, *s.cfg, label, "")
	if _, err := os.Stat(worktreePath); err != nil {
		if os.IsNotExist(err) {
			return CheckTreeChildResult{
				Label:  label,
				State:  CheckTreeStateNoLaunch,
				Reason: "worktree directory missing on disk",
			}
		}
		// Permission / I/O errors are surfaced — the caller probably
		// wants to know why bosun couldn't see the worktree, not just
		// that it didn't.
		return CheckTreeChildResult{
			Label:  label,
			State:  CheckTreeStateNoLaunch,
			Reason: fmt.Sprintf("stat worktree: %v", err),
		}
	}

	if _, broken := brokenAdmins[filepath.Base(worktreePath)]; broken {
		return CheckTreeChildResult{
			Label:  label,
			State:  CheckTreeStateNoLaunch,
			Reason: "git admin metadata broken (missing HEAD/commondir/gitdir)",
		}
	}

	if _, running := s.runningFn(worktreePath); running {
		return CheckTreeChildResult{Label: label, State: CheckTreeStateAlive}
	}
	return CheckTreeChildResult{
		Label:  label,
		State:  CheckTreeStateDead,
		Reason: "worktree present but no agent process detected",
	}
}

// summarizeTree renders a one-line, human-readable summary suitable for
// the MCP text content block. The structured Children list is the
// authoritative shape; the text is for log-style display when the
// caller doesn't decode the JSON.
func summarizeTree(r CheckTreeResult) string {
	if len(r.Children) == 0 {
		return fmt.Sprintf("%s: no children", r.Parent)
	}
	parts := make([]string, 0, len(r.Children))
	for _, c := range r.Children {
		parts = append(parts, fmt.Sprintf("%s=%s", c.Label, c.State))
	}
	return fmt.Sprintf("%s: %s", r.Parent, strings.Join(parts, ", "))
}
