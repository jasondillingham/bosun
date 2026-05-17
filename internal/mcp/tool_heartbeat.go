package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name: "bosun_heartbeat",
			Description: "Record a liveness heartbeat for this session. Call periodically " +
				"(every minute or two is fine) so the operator can distinguish a hung " +
				"agent from a quietly-working one. A WORKING session whose heartbeat " +
				"goes stale (>5 min) is surfaced as STALE in `bosun status`.",
		}, s.toolHeartbeat)
	})
}

// HeartbeatArgs is the input schema for bosun_heartbeat.
type HeartbeatArgs struct {
	Session string `json:"session" jsonschema:"the bosun session name (e.g. session-2) — read from BOSUN_BRIEF.md or the worktree path"`
}

// HeartbeatResult is the structured output for bosun_heartbeat.
type HeartbeatResult struct {
	Session string `json:"session"`
	At      string `json:"at" jsonschema:"the recorded heartbeat timestamp (RFC3339)"`
}

// toolHeartbeat implements bosun_heartbeat. Canonicalizes the session
// label, then writes the current timestamp into
// .bosun/state/<session>.heartbeat via the state store's flock path so
// concurrent heartbeats from racing processes cannot tear the file.
func (s *Server) toolHeartbeat(_ context.Context, _ *mcp.CallToolRequest, args HeartbeatArgs) (*mcp.CallToolResult, HeartbeatResult, error) {
	raw := strings.TrimSpace(args.Session)
	if raw == "" {
		return errResult(errors.New("session is required")), HeartbeatResult{}, nil
	}
	label, err := session.ParseLabel(raw)
	if err != nil {
		return errResult(err), HeartbeatResult{}, nil
	}
	if err := s.state.WriteHeartbeat(label); err != nil {
		return errResult(fmt.Errorf("write heartbeat: %w", err)), HeartbeatResult{}, nil
	}
	at, ok, err := s.state.Heartbeat(s.state.RepoRoot(), label)
	if err != nil || !ok {
		// We just wrote it — surface what we have instead of failing the
		// call. The operator's status render is the consumer; if the
		// heartbeat round-trip failed here, the next bosun_heartbeat
		// will fix it.
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("%s heartbeat recorded", label)},
			},
		}, HeartbeatResult{Session: label}, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("%s heartbeat recorded at %s", label, at.Format("15:04:05"))},
		},
	}, HeartbeatResult{Session: label, At: at.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")}, nil
}
