package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/subtask"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// subtaskToolDescription is the bosun_subtask MCP tool description.
// Lead with the registry framing so the LLM understands bosun is the
// recorder — the parent agent is on the hook to actually run the
// sub-task's work (typically via Claude Code's Agent tool). The
// counterpart pitch to bosun_spawn lands in the second clause so an
// agent reading both descriptions back-to-back sees the trade-off.
var subtaskToolDescription = "Register a fresh-context sub-task that " +
	"shares the parent's worktree. Use when the work fans out into " +
	"independent reads (audit five packages, summarize three docs) and " +
	"you'd rather start each thread on a clean context window than " +
	"thread them through the parent's history. Cheaper than bosun_spawn " +
	"(no fork, no branch, no merge); the right answer when you don't " +
	"need a mergeable unit of committed work. Bosun's job is the " +
	"registry — it records the call, audit-logs the outcome, and tracks " +
	"the per-parent quota — and the parent agent runs the actual " +
	"sub-task (e.g. via Claude Code's Agent tool). Operator authorizes " +
	"once via .bosun/config.json (agent_subtask.enabled=true); " +
	"per-parent quota applies (agent_subtask.max_concurrent, default 5). " +
	"Returns the sub-task ID and the RFC3339 registration timestamp."

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        "bosun_subtask",
			Description: subtaskToolDescription,
		}, s.toolSubtask)
	})
}

// SubtaskArgs is the input schema for bosun_subtask. See
// docs/v1.0-sub-task-spec.md for full semantics. The lane-1 brief
// trims the spec's surface to the three fields the registry-only
// implementation needs; prompt / read_only / timeout_seconds land
// when the executor lane does.
type SubtaskArgs struct {
	Parent      string   `json:"parent" jsonschema:"the calling session's own label (e.g. session-1); bosun verifies the caller is running inside this worktree"`
	Description string   `json:"description" jsonschema:"one-paragraph description of the work this sub-task should do — recorded verbatim in the registry"`
	Files       []string `json:"files,omitempty" jsonschema:"optional path scope; sub-task is encouraged but not gated to stay within these"`
}

// SubtaskResult is the structured tool output. The synthetic ID is
// "<parent>.<12hex>" — stable for the lifetime of the registry
// record. Started is RFC3339 UTC so any consumer can sort calls
// chronologically without re-parsing.
type SubtaskResult struct {
	ID      string `json:"id" jsonschema:"synthetic label assigned by the registry: <parent>.<short-uuid>"`
	Started string `json:"started" jsonschema:"RFC3339 UTC registration timestamp"`
}

// toolSubtask implements bosun_subtask. The handler is the auth-gate
// front end; the on-disk registry lives in internal/subtask. Every
// refusal AND the success path call logSubtaskAttempt so
// .bosun/audit/subtask.log captures the full decision history (same
// stance bosun_spawn took for #14).
func (s *Server) toolSubtask(_ context.Context, _ *mcp.CallToolRequest, args SubtaskArgs) (*mcp.CallToolResult, SubtaskResult, error) {
	repoRoot := s.state.RepoRoot()
	parentRaw := strings.TrimSpace(args.Parent)

	refuse := func(gate string, err error) (*mcp.CallToolResult, SubtaskResult, error) {
		logSubtaskAttempt(repoRoot, subtaskAuditEntry{
			Parent:         parentRaw,
			Outcome:        "refused",
			RefusalGate:    gate,
			RefusalMessage: err.Error(),
		})
		return errResult(err), SubtaskResult{}, nil
	}

	// Auth gate #1: feature-flag. The server has to be wired (cfg
	// non-nil) AND the operator must have opted in. Cfg-unwired is
	// reported with the same "not configured" framing as spawn so
	// operators see a consistent error vocabulary across the two tools.
	if s.cfg == nil {
		return refuse(subtaskGateConfigDisabled, errors.New("bosun_subtask is not configured on this daemon (operator must wire WithSpawnSupport in cmd_mcp)"))
	}
	if !s.cfg.AgentSubtask.Enabled {
		return refuse(subtaskGateConfigDisabled, errors.New("agent_subtask is not enabled for this repo. Operator: set agent_subtask.enabled=true in .bosun/config.json"))
	}

	// Arg validation. Empty parent / empty description go through the
	// same gate so the audit log has a single "bad request" bucket.
	if parentRaw == "" {
		return refuse(subtaskGateInvalidArgs, errors.New("parent is required"))
	}
	parent, err := session.ParseLabel(parentRaw)
	if err != nil {
		return refuse(subtaskGateInvalidArgs, err)
	}
	if strings.TrimSpace(args.Description) == "" {
		return refuse(subtaskGateInvalidArgs, errors.New("description is required"))
	}

	// Auth gate #2: parent identity. Same s.runningFn indirection
	// bosun_spawn uses — tests substitute a deterministic fake so
	// the gate is exercisable without spawning a real claude.
	// Sub-tasks share the parent's worktree; ResolveWorktreePath's
	// legacy-fallback handles either naming scheme since liveness
	// only cares about the path matching a live agent's CWD.
	worktreePath := session.ResolveWorktreePath(repoRoot, *s.cfg, parent, "")
	if _, running := s.runningFn(worktreePath); !running {
		return refuse(subtaskGateParentLiveness, fmt.Errorf("no live agent detected in %s's worktree; bosun_subtask requires the caller to be running inside the named parent", parent))
	}

	// Auth gate #3: per-parent quota. CountActive consults the
	// registry under the same flock Create will take below, but the
	// count→create pair across the two locks is the documented race
	// surface for the registry-only lane — a concurrent registration
	// can squeak past the cap by 1. That's deemed acceptable for v1.0
	// because the cap is a soft guard rail against fork-bombs, not a
	// hard concurrency contract; the spec's lane-2 work tightens this
	// when real concurrency lands.
	store := subtask.NewStore(repoRoot)
	active, cerr := store.CountActive(parent)
	if cerr != nil {
		return refuse(subtaskGateInternal, fmt.Errorf("count active subtasks: %w", cerr))
	}
	maxConcurrent := s.cfg.AgentSubtask.MaxConcurrent
	if active >= maxConcurrent {
		return refuse(subtaskGateConcurrentCap, fmt.Errorf("parent %q already has %d active sub-task(s); agent_subtask.max_concurrent=%d", parent, active, maxConcurrent))
	}

	// All gates passed — register the sub-task. Per spec §4: bosun's
	// job is record + audit + display; the parent agent runs the
	// actual work (typically via Claude Code's Agent tool for real
	// context isolation).
	rec, cerr := store.Create(parent, args.Description, args.Files)
	if cerr != nil {
		return refuse(subtaskGateInternal, fmt.Errorf("create subtask: %w", cerr))
	}

	logSubtaskAttempt(repoRoot, subtaskAuditEntry{
		Parent:         parent,
		RequestedLabel: rec.ID,
		Outcome:        "success",
	})

	msg := fmt.Sprintf("%s: registered sub-task %s", parent, rec.ID)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}, SubtaskResult{ID: rec.ID, Started: rec.Started}, nil
}
