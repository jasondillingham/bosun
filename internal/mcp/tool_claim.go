package mcp

import (
	"context"
	"fmt"
	"strings"

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
}

// toolClaim implements bosun_claim. Wraps claims.Store.Add — all
// normalization, dedupe, and merge semantics live in the claims package
// so the CLI and MCP paths stay byte-identical on disk.
func (s *Server) toolClaim(_ context.Context, _ *mcp.CallToolRequest, args ClaimArgs) (*mcp.CallToolResult, ClaimResult, error) {
	session := strings.TrimSpace(args.Session)
	if session == "" {
		return nil, ClaimResult{}, fmt.Errorf("session is required")
	}
	if err := s.claims.Add(session, args.Paths); err != nil {
		return nil, ClaimResult{}, fmt.Errorf("add claim: %w", err)
	}
	c, err := s.claims.Read(session)
	if err != nil {
		return nil, ClaimResult{}, fmt.Errorf("read claim: %w", err)
	}
	count := 0
	if c != nil {
		count = len(c.Paths)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("%s now claims %d path(s)", session, count)},
		},
	}, ClaimResult{Claimed: count}, nil
}
