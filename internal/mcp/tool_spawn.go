package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// spawnToolDescription is the bosun_spawn MCP tool description. The
// LLM reads this at decision time, so the leading sentences carry the
// most weight: lead with context isolation as the value prop, not raw
// parallelism. Trial #3b showed the old "parallelize" pitch lost
// against the agent's solo-tractable heuristic on small work — agents
// correctly self-handled the work and skipped the spawn tool. The
// reframing teaches the LLM to reach for bosun_spawn only when the
// parent's context window is the real constraint.
//
// Kept as a package-level var (not inlined into the init() literal)
// so tests can pin the wording without round-tripping through the
// SDK's tool list.
var spawnToolDescription = "Spawn sub-sessions for context isolation when the " +
	"parent's full plan would exceed a comfortable context window. " +
	"Each sub starts cold — zero context cost from the parent's " +
	"conversation — so spawning is cheaper than scrolling the " +
	"parent's history past N independent edits. Use only when " +
	"(a) the lanes are genuinely disjoint package-shaped work AND " +
	"(b) the parent's window is at risk. Don't spawn for tractable " +
	"solo work — false-spawning is anti-signal; the value prop is " +
	"context isolation, not raw parallelism. Operator authorizes " +
	"once via .bosun/config.json (agent_spawn.enabled=true); " +
	"per-parent and per-tree quotas apply. Each `## suffix` heading " +
	"in the brief becomes a sub-session named `<parent>.<suffix>` " +
	"branched from the parent's HEAD. Returns the labels created " +
	"and any per-sub failures."

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        "bosun_spawn",
			Description: spawnToolDescription,
		}, s.toolSpawn)
	})
}

// maxBriefBytes caps the size of the brief argument an agent can
// pass to bosun_spawn. 256KB is well above realistic operator-shaped
// briefs (typically <10KB) and gives the MCP server a hard upper
// bound on allocator pressure from a malicious or merely confused
// caller. Refused requests still get audited with
// spawnGateInvalidArgs.
const maxBriefBytes = 256 << 10

// SpawnArgs is the input schema for bosun_spawn. See
// docs/v0.9-spawn-spec.md for full semantics.
type SpawnArgs struct {
	Parent string `json:"parent" jsonschema:"calling session's own label (e.g. session-1); bosun verifies the caller is inside this worktree"`
	Brief  string `json:"brief" jsonschema:"inline markdown brief — same shape bosun init --brief consumes. Each '## suffix' heading becomes a sub-session named <parent>.<suffix>"`
	Launch bool   `json:"launch" jsonschema:"when true (the default), spawn agents in each new sub-session; false creates worktrees + briefs without launching"`
}

// SpawnResult is the structured tool output.
type SpawnResult struct {
	Created []string         `json:"created" jsonschema:"sub-session labels successfully created (full parent.suffix form)"`
	Failed  []SpawnFailureMC `json:"failed,omitempty" jsonschema:"per-label failures with reason"`
}

// SpawnFailureMC mirrors spawn.Failure for the MCP wire shape.
type SpawnFailureMC struct {
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// toolSpawn implements bosun_spawn. Runs the auth + quota + depth
// gates, then delegates the per-sub create pipeline to
// internal/spawn.Run. Any spawn-tree mutation happens inside that
// helper under the flock; the gates here are quota/depth checks
// against an already-loaded tree snapshot.
//
// Every refusal AND the success path call logSpawnAttempt so
// .bosun/audit/spawn.log captures the full decision history (issue
// #14). The refuse() helper keeps the per-gate logging DRY without
// scattering log calls across 11 return sites.
func (s *Server) toolSpawn(_ context.Context, _ *mcp.CallToolRequest, args SpawnArgs) (*mcp.CallToolResult, SpawnResult, error) {
	// repoRoot is captured up front so every audit log call has the
	// same target path. s.state is always populated by NewServer, so
	// even the server-not-configured branch below has a valid root.
	repoRoot := s.state.RepoRoot()
	parentRaw := strings.TrimSpace(args.Parent)

	refuse := func(gate string, err error) (*mcp.CallToolResult, SpawnResult, error) {
		logSpawnAttempt(repoRoot, spawnAuditEntry{
			Parent:         parentRaw,
			Outcome:        "refused",
			RefusalGate:    gate,
			RefusalMessage: err.Error(),
		})
		return errResult(err), SpawnResult{}, nil
	}

	if s.spawnTree == nil || s.cfg == nil {
		return refuse(spawnGateConfigDisabled, errors.New("bosun_spawn is not configured on this daemon (operator must wire WithSpawnSupport in cmd_mcp)"))
	}

	// Auth gate #1: feature-flag.
	if !s.cfg.AgentSpawn.Enabled {
		return refuse(spawnGateConfigDisabled, errors.New("agent_spawn is not enabled for this repo. Operator: set agent_spawn.enabled=true in .bosun/config.json"))
	}

	if parentRaw == "" {
		return refuse(spawnGateInvalidArgs, errors.New("parent is required"))
	}
	if len(args.Brief) > maxBriefBytes {
		return refuse(spawnGateInvalidArgs, fmt.Errorf("brief exceeds %d-byte cap (got %d bytes); real briefs are typically <10KB", maxBriefBytes, len(args.Brief)))
	}
	parent, err := session.ParseLabel(parentRaw)
	if err != nil {
		return refuse(spawnGateInvalidArgs, err)
	}

	// Auth gate #2: whitelist (if configured).
	if !isParentAllowedToSpawn(parent, s.cfg.AgentSpawn) {
		return refuse(spawnGateAllowedForSessions, fmt.Errorf("session %q is not in agent_spawn.allowed_for_sessions whitelist", parent))
	}

	// Auth gate #3: parent identity. The calling agent must actually
	// be running inside the parent's worktree. s.runningFn is
	// per-instance indirection over proc.Running so tests can mock the
	// liveness check without touching real processes; the production
	// wiring (NewServer) defaults to proc.Running.
	//
	// The worktree path is resolved by branch via `git worktree list`
	// rather than reconstructed from cfg-template, so scheme-C UID-per-
	// worktree sessions (the default since v0.11) resolve correctly
	// without needing the round timestamp threaded in. Pre-bughunt-1
	// this was WorktreePathForLabel(..., "") which produced the legacy
	// shape `<repo>-bosun-<sub>` and missed every scheme-C session
	// (Bughunt-1 F009). s.worktreePathFn is per-instance indirection
	// for the same reason as runningFn.
	worktreePath, found, lerr := s.worktreePathFn(parent)
	if lerr != nil {
		return refuse(spawnGateParentLiveness, fmt.Errorf("resolve worktree for %s: %w", parent, lerr))
	}
	if !found {
		return refuse(spawnGateParentLiveness, fmt.Errorf("no worktree found for %s; bosun_spawn requires the caller to be running inside the named parent", parent))
	}
	if _, running := s.runningFn(worktreePath); !running {
		return refuse(spawnGateParentLiveness, fmt.Errorf("no live agent detected in %s's worktree; bosun_spawn requires the caller to be running inside the named parent", parent))
	}

	// Auth gate #4: depth ceiling. Parent's depth + 1 must be <=
	// MaxDepth (which itself was already clamped to
	// MaxAgentSpawnDepthCeiling at config load).
	parentDepth, derr := s.spawnTree.DepthOf(parent)
	if derr != nil {
		return refuse(spawnGateDepthCeiling, fmt.Errorf("read spawn tree: %w", derr))
	}
	if parentDepth+1 > s.cfg.AgentSpawn.MaxDepth {
		return refuse(spawnGateMaxDepth, fmt.Errorf("spawn depth %d would exceed agent_spawn.max_depth=%d (parent is at depth %d)", parentDepth+1, s.cfg.AgentSpawn.MaxDepth, parentDepth))
	}

	// Auth gate #5: per-parent quota.
	existing, cerr := s.spawnTree.CountChildren(parent)
	if cerr != nil {
		return refuse(spawnGateConcurrentQuota, fmt.Errorf("count children: %w", cerr))
	}
	if existing >= s.cfg.AgentSpawn.MaxConcurrentSubSessions {
		return refuse(spawnGateConcurrentQuota, fmt.Errorf("parent %q already has %d live sub-session(s); agent_spawn.max_concurrent_sub_sessions=%d", parent, existing, s.cfg.AgentSpawn.MaxConcurrentSubSessions))
	}

	// All gates passed — delegate to the per-sub create pipeline.
	res, runErr := spawn.Run(context.Background(), s.gitClient, s.spawnTree, spawn.Request{
		RepoRoot:      repoRoot,
		ParentLabel:   parent,
		BriefMarkdown: args.Brief,
		Launch:        args.Launch,
		Cfg:           *s.cfg,
	})
	if runErr != nil && len(res.Created) == 0 {
		return refuse(spawnGateInvalidArgs, runErr)
	}

	out := SpawnResult{Created: res.Created}
	for _, f := range res.Failed {
		out.Failed = append(out.Failed, SpawnFailureMC{Label: f.Label, Reason: f.Reason})
	}

	logSpawnAttempt(repoRoot, spawnAuditEntry{
		Parent:         parent,
		RequestedLabel: strings.Join(res.Created, ","),
		Outcome:        "success",
	})

	msg := fmt.Sprintf("%s: spawned %d sub-session(s)", parent, len(res.Created))
	if len(res.Failed) > 0 {
		msg += fmt.Sprintf(" (%d failed)", len(res.Failed))
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}, out, nil
}

// isParentAllowedToSpawn checks the whitelist when configured. An
// empty list means any session may spawn.
func isParentAllowedToSpawn(parent string, cfg config.AgentSpawnConfig) bool {
	if len(cfg.AllowedForSessions) == 0 {
		return true
	}
	for _, allowed := range cfg.AllowedForSessions {
		if allowed == parent {
			return true
		}
	}
	return false
}
