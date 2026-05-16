package mcp

import (
	"context"
	"fmt"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        "bosun_stuck",
			Description: "Mark this session STUCK (blocked, needs the operator's attention). No validation — stuck is meant to be the escape hatch when you can't make progress.",
		}, s.toolStuck)
	})
}

// StuckArgs is the input schema for bosun_stuck.
type StuckArgs struct {
	Session string `json:"session" jsonschema:"the session to mark (e.g. session-1 or 1)"`
	Message string `json:"message,omitempty" jsonschema:"reason you're stuck (shown in bosun status)"`
}

// StuckResult is the structured output for bosun_stuck.
type StuckResult struct {
	State string `json:"state" jsonschema:"final session state (STUCK)"`
}

func (s *Server) toolStuck(_ context.Context, _ *mcp.CallToolRequest, args StuckArgs) (*mcp.CallToolResult, StuckResult, error) {
	label, err := session.ParseLabel(args.Session)
	if err != nil {
		return errResult(err), StuckResult{}, nil
	}

	if err := s.state.MarkStuck(label, args.Message); err != nil {
		return errResult(fmt.Errorf("mark stuck: %w", err)), StuckResult{}, nil
	}

	summary := fmt.Sprintf("%s marked STUCK", label)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, StuckResult{State: string(session.StateStuck)}, nil
}
