package mcp

import (
	"context"
	"errors"
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
			Name: "bosun_usage",
			Description: "Record one agent-turn's token + cost usage against a bosun session. " +
				"Agent wrappers call this after each turn (or once per session at exit) so " +
				"`bosun status` can show per-session cost, `bosun merge` can summarize round " +
				"spend, and the budget gate can refuse claims when a session exceeds its " +
				"config.usage_budget_usd. Append-only ledger at " +
				".bosun/state/<session>.usage. Refuses unknown sessions, negative tokens " +
				"or cost, and empty session labels.",
		}, s.toolUsage)
	})
}

// UsageArgs is the input schema for bosun_usage. Mirrors usage.Entry
// shape minus the timestamp (server-side stamped at append time).
type UsageArgs struct {
	Session   string  `json:"session" jsonschema:"the bosun session to record usage against (e.g. session-2 or 2)"`
	TokensIn  int     `json:"tokens_in,omitempty" jsonschema:"input tokens for this turn — optional, default 0, must be >= 0 when set. Wrappers that only know total cost can leave this and tokens_out at zero"`
	TokensOut int     `json:"tokens_out,omitempty" jsonschema:"output tokens for this turn — optional, default 0, must be >= 0 when set"`
	CostUSD   float64 `json:"cost_usd" jsonschema:"USD cost of this turn — must be >= 0. Operator's wrapper computes this from the agent runtime's reported token-pricing. Always required so the ledger can roll up a meaningful total even when tokens are unknown"`
	Model     string  `json:"model,omitempty" jsonschema:"model identifier (e.g. claude-sonnet-4-6 or ollama/llama3.1:8b) — optional but recommended"`
	TurnLabel string  `json:"turn_label,omitempty" jsonschema:"optional human-readable phase tag (e.g. design / implementation / test-fix) for breaking down cost by stage"`
}

// UsageResult is the structured output for bosun_usage. Returns the
// session's running totals after the append so the agent can decide
// whether to slow down without an extra read call.
type UsageResult struct {
	Session   string  `json:"session" jsonschema:"canonical session label the usage was recorded against"`
	TotalIn   int     `json:"total_tokens_in" jsonschema:"cumulative input tokens across all turns recorded for this session"`
	TotalOut  int     `json:"total_tokens_out" jsonschema:"cumulative output tokens"`
	TotalCost float64 `json:"total_cost_usd" jsonschema:"cumulative USD cost"`
	TurnCount int     `json:"turn_count" jsonschema:"number of turns recorded (entries in the ledger)"`
}

// toolUsage implements bosun_usage. Same validation shape as
// bosun_attach: refuses unknown sessions, requires a git client to
// re-validate the session label live. Appends to the per-session
// JSON-lines ledger via internal/usage, then returns running totals.
func (s *Server) toolUsage(ctx context.Context, _ *mcp.CallToolRequest, args UsageArgs) (*mcp.CallToolResult, UsageResult, error) {
	raw := strings.TrimSpace(args.Session)
	if raw == "" {
		return errResult(errors.New("session is required")), UsageResult{}, nil
	}
	label, err := session.ParseLabel(raw)
	if err != nil {
		return errResult(err), UsageResult{}, nil
	}
	if args.TokensIn < 0 || args.TokensOut < 0 {
		return errResult(fmt.Errorf("token counts must be >= 0 (got in=%d, out=%d)", args.TokensIn, args.TokensOut)), UsageResult{}, nil
	}
	if args.CostUSD < 0 {
		return errResult(fmt.Errorf("cost_usd must be >= 0, got %f", args.CostUSD)), UsageResult{}, nil
	}

	// Re-validate the session against the live worktree set — same
	// "no orphan state files" gate bosun_attach uses. Without it, a
	// typo against `bosun_usage session-99` would silently create a
	// .usage ledger for a non-existent session that no renderer ever
	// reads.
	repoRoot := s.state.RepoRoot()
	cfg, err := config.Load(repoRoot)
	if err != nil {
		return errResult(fmt.Errorf("load config: %w", err)), UsageResult{}, nil
	}
	if s.gitClient == nil {
		return errResult(errors.New("bosun_usage requires a git client to validate the session")), UsageResult{}, nil
	}
	sess, err := findSessionByLabel(ctx, s, cfg, repoRoot, label)
	if err != nil {
		return errResult(err), UsageResult{}, nil
	}
	if sess == nil {
		return errResult(fmt.Errorf("%s not found", label)), UsageResult{}, nil
	}

	entry := usage.Entry{
		TokensIn:  args.TokensIn,
		TokensOut: args.TokensOut,
		CostUSD:   args.CostUSD,
		Model:     strings.TrimSpace(args.Model),
		TurnLabel: strings.TrimSpace(args.TurnLabel),
	}
	if err := usage.Append(repoRoot, label, entry); err != nil {
		return errResult(fmt.Errorf("append usage: %w", err)), UsageResult{}, nil
	}

	totals, err := usage.ReadTotals(repoRoot, label)
	if err != nil {
		// Append succeeded but read-back failed — surface the partial
		// success so the caller knows the entry landed even if we
		// can't report aggregates back.
		return errResult(fmt.Errorf("read usage totals: %w", err)), UsageResult{
			Session: label,
		}, nil
	}

	summary := fmt.Sprintf("%s recorded: +$%.4f (in=%d, out=%d) · session total $%.4f over %d turns",
		label, args.CostUSD, args.TokensIn, args.TokensOut, totals.CostUSD, totals.TurnCount)
	return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: summary}},
		}, UsageResult{
			Session:   label,
			TotalIn:   totals.TokensIn,
			TotalOut:  totals.TokensOut,
			TotalCost: totals.CostUSD,
			TurnCount: totals.TurnCount,
		}, nil
}
