package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        "bosun_done",
			Description: "Mark this session ready to merge. Refuses if the worktree is dirty or has no commits ahead of base, unless force=true.",
		}, s.toolDone)
	})
}

// DoneArgs is the input schema for bosun_done.
type DoneArgs struct {
	Session string `json:"session" jsonschema:"the session to mark (e.g. session-1 or 1)"`
	Message string `json:"message,omitempty" jsonschema:"optional note stored alongside the marker"`
	Force   bool   `json:"force,omitempty" jsonschema:"bypass dirty/ahead validation"`
}

// DoneResult is the structured output for bosun_done.
type DoneResult struct {
	State   string `json:"state" jsonschema:"final session state (DONE)"`
	Message string `json:"message" jsonschema:"human-readable summary of what changed"`
}

func (s *Server) toolDone(ctx context.Context, _ *mcp.CallToolRequest, args DoneArgs) (*mcp.CallToolResult, DoneResult, error) {
	label, err := session.ParseLabel(args.Session)
	if err != nil {
		return errResult(err), DoneResult{}, nil
	}

	repoRoot := s.state.RepoRoot()
	cfg, err := config.Load(repoRoot)
	if err != nil {
		return errResult(fmt.Errorf("load config: %w", err)), DoneResult{}, nil
	}

	if !args.Force {
		// Validation needs git to compute Dirty/Ahead. force=true skips
		// validation entirely, so a missing git client is only fatal here.
		if s.gitClient == nil {
			return errResult(errors.New("bosun_done requires a git client to validate; pass force=true to skip")), DoneResult{}, nil
		}
		sess, err := findSessionByLabel(ctx, s, cfg, repoRoot, label)
		if err != nil {
			return errResult(err), DoneResult{}, nil
		}
		if sess == nil {
			return errResult(fmt.Errorf("%s not found", label)), DoneResult{}, nil
		}
		if err := session.ValidateDoneable(*sess, cfg.BaseBranch); err != nil {
			return errResult(err), DoneResult{}, nil
		}
	}

	if err := s.state.MarkDone(label, args.Message); err != nil {
		return errResult(fmt.Errorf("mark done: %w", err)), DoneResult{}, nil
	}

	summary := fmt.Sprintf("%s marked DONE", label)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, DoneResult{State: string(session.StateDone), Message: summary}, nil
}

// findSessionByLabel returns the bosun-managed Session with the given
// canonical label, or nil if it doesn't exist. Shared between tool_done.go
// and any future tool that needs a session's git-derived state.
func findSessionByLabel(ctx context.Context, s *Server, cfg config.Config, repoRoot string, label string) (*session.Session, error) {
	sessions, err := session.Derive(ctx, s.gitClient, cfg, repoRoot, s.state, s.claims)
	if err != nil {
		return nil, fmt.Errorf("derive sessions: %w", err)
	}
	for i := range sessions {
		if sessions[i].Label == label {
			return &sessions[i], nil
		}
	}
	return nil, nil
}

// errResult wraps an error as an MCP CallToolResult so that callers see a
// structured failure instead of a transport-level error. The SDK treats
// `IsError: true` results as tool-level failures the model can read.
func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}
