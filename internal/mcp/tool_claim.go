package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/usage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        "bosun_claim",
			Description: "Declare paths the calling bosun session is editing. Merges into the session's existing claim file; duplicates are deduped. Returns the total number of paths the session now claims.",
		}, s.toolClaim)
	})
}

// ClaimArgs is the input schema for bosun_claim.
type ClaimArgs struct {
	Session string   `json:"session" jsonschema:"the bosun session name (e.g. session-2) — read from BOSUN_BRIEF.md or the worktree path"`
	Paths   []string `json:"paths" jsonschema:"paths the session is editing (repo-root relative)"`
}

// ClaimResult is the structured output for bosun_claim.
type ClaimResult struct {
	Claimed int `json:"claimed" jsonschema:"total number of paths the session claims after the merge"`
	// BudgetWarning is set when the session's cumulative usage has
	// crossed the soft-warning threshold (80% of
	// config.usage_budget_usd) but not yet hit the hard refusal at
	// 100%. Empty when no budget is configured OR the session is
	// well under the threshold.
	BudgetWarning string `json:"budget_warning,omitempty" jsonschema:"set when the session has spent >= 80% of its configured usage budget; advisory only — the claim still succeeded"`
}

// maxClaimPaths caps the number of paths accepted in a single bosun_claim
// call. Real briefs touch single-digit path counts; anything above this is
// almost certainly an agent mistakenly handing the tool a directory listing.
const maxClaimPaths = 256

// toolClaim implements bosun_claim. Wraps claims.Store.Add — all
// normalization, dedupe, and merge semantics live in the claims package
// so the CLI and MCP paths stay byte-identical on disk.
func (s *Server) toolClaim(_ context.Context, _ *mcp.CallToolRequest, args ClaimArgs) (*mcp.CallToolResult, ClaimResult, error) {
	sessionName := strings.TrimSpace(args.Session)
	if sessionName == "" {
		return nil, ClaimResult{}, fmt.Errorf("session is required")
	}
	// Canonicalize via ParseLabel so callers can pass "1" or
	// "session-1" interchangeably — same shortcut the other MCP
	// tools accept. Falls back to the raw input when ParseLabel
	// can't make sense of it (e.g. named-session labels) — the
	// downstream claims.Add layer handles validation.
	if canonical, err := session.ParseLabel(sessionName); err == nil {
		sessionName = canonical
	}
	// Count entries that aren't blank-after-trim so a paths=[" "] payload
	// reports as "no paths" instead of silently no-oping.
	nonBlank := 0
	for _, p := range args.Paths {
		if strings.TrimSpace(p) != "" {
			nonBlank++
		}
	}
	if nonBlank == 0 {
		return nil, ClaimResult{}, fmt.Errorf("paths must contain at least one non-empty entry")
	}
	if nonBlank > maxClaimPaths {
		return nil, ClaimResult{}, fmt.Errorf("paths length %d exceeds limit %d", nonBlank, maxClaimPaths)
	}

	// Phase 4 budget gate. When config.usage_budget_usd is set and
	// the session has already spent >= 100%, refuse the claim — the
	// agent should stop work, not start more. At >= 80% (and below
	// 100%) we still grant the claim but attach a warning so a
	// well-behaved wrapper can prompt the operator. Zero budget
	// means "no limit" and short-circuits the whole block.
	repoRoot := s.state.RepoRoot()
	cfg, cfgErr := config.Load(repoRoot)
	// Config load failure is non-fatal for claims — the budget gate
	// is best-effort observability, and refusing claims because the
	// config didn't parse would be worse than letting them through.
	// We just skip the gate and log via the response.
	if cfgErr == nil && cfg.UsageBudgetUSD > 0 {
		spent, err := usage.ReadTotals(repoRoot, sessionName)
		if err == nil && spent.CostUSD >= cfg.UsageBudgetUSD {
			return nil, ClaimResult{}, fmt.Errorf(
				"%s has spent $%.2f of $%.2f budget — refusing claim. Increase config.usage_budget_usd or `bosun done` the session",
				sessionName, spent.CostUSD, cfg.UsageBudgetUSD,
			)
		}
	}

	if err := s.claims.Add(sessionName, args.Paths); err != nil {
		return nil, ClaimResult{}, fmt.Errorf("add claim: %w", err)
	}
	c, err := s.claims.Read(sessionName)
	if err != nil {
		return nil, ClaimResult{}, fmt.Errorf("read claim: %w", err)
	}
	count := 0
	if c != nil {
		count = len(c.Paths)
	}

	// Soft warning at >= 80% of budget. We compute it here (after
	// the hard-refusal block above) so callers that succeed know
	// they're nearing the cap.
	result := ClaimResult{Claimed: count}
	summary := fmt.Sprintf("%s now claims %d path(s)", sessionName, count)
	if cfgErr == nil && cfg.UsageBudgetUSD > 0 {
		spent, err := usage.ReadTotals(repoRoot, sessionName)
		if err == nil {
			pct := spent.CostUSD / cfg.UsageBudgetUSD
			if pct >= 0.8 {
				warning := fmt.Sprintf(
					"%s has spent $%.2f of $%.2f budget (%.0f%%) — consider wrapping up",
					sessionName, spent.CostUSD, cfg.UsageBudgetUSD, pct*100,
				)
				result.BudgetWarning = warning
				summary += " · " + warning
			}
		}
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
		},
	}, result, nil
}
