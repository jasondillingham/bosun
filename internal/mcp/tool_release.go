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
			Name:        "bosun_release",
			Description: "Release path claims held by the calling bosun session. With paths, only those entries are dropped; without paths, the entire claim file is cleared. Returns the number of paths released.",
		}, s.toolRelease)
	})
}

// ReleaseArgs is the input schema for bosun_release. An empty Paths slice
// means "release everything this session has claimed".
type ReleaseArgs struct {
	Session string   `json:"session" jsonschema:"the bosun session name (e.g. session-2) — read from BOSUN_BRIEF.md or the worktree path"`
	Paths   []string `json:"paths,omitempty" jsonschema:"optional: specific paths to release. Empty means release all claims for the session."`
}

// ReleaseResult is the structured output for bosun_release.
type ReleaseResult struct {
	Released int `json:"released" jsonschema:"number of paths released (or the count cleared when no paths were given)"`
}

// toolRelease implements bosun_release. With explicit paths it delegates
// to claims.Store.Remove; without paths it clears the session's entire
// claim file via claims.Store.Clear and reports the count that was
// dropped. All normalization/persistence lives in the claims package.
func (s *Server) toolRelease(_ context.Context, _ *mcp.CallToolRequest, args ReleaseArgs) (*mcp.CallToolResult, ReleaseResult, error) {
	session := strings.TrimSpace(args.Session)
	if session == "" {
		return nil, ReleaseResult{}, fmt.Errorf("session is required")
	}

	if len(args.Paths) == 0 {
		c, err := s.claims.Read(session)
		if err != nil {
			return nil, ReleaseResult{}, fmt.Errorf("read claim: %w", err)
		}
		count := 0
		if c != nil {
			count = len(c.Paths)
		}
		if err := s.claims.Clear(session); err != nil {
			return nil, ReleaseResult{}, fmt.Errorf("clear claim: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("%s released all %d claim(s)", session, count)},
			},
		}, ReleaseResult{Released: count}, nil
	}

	removed, err := s.claims.Remove(session, args.Paths)
	if err != nil {
		return nil, ReleaseResult{}, fmt.Errorf("remove claim paths: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("%s released %d path(s)", session, removed)},
		},
	}, ReleaseResult{Released: removed}, nil
}
