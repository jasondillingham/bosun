package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name: "bosun_announce",
			Description: "Push an operator-visible status message from this session. " +
				"Use for periodic progress updates ('starting on storage layer', " +
				"'tests still running but not stuck'). Kind defaults to 'info'; pass " +
				"'progress' or 'warn' to color the message on the operator side.",
		}, s.toolAnnounce)
	})
}

// AnnounceArgs is the input schema for bosun_announce.
type AnnounceArgs struct {
	Session string `json:"session" jsonschema:"the session pushing this announcement (e.g. session-2)"`
	Message string `json:"message" jsonschema:"human-readable status text"`
	Kind    string `json:"kind,omitempty" jsonschema:"info | progress | warn — defaults to info"`
}

// AnnounceResult is the structured output for bosun_announce.
type AnnounceResult struct {
	Recorded bool `json:"recorded"`
}

// maxAnnounceMessageBytes caps an announcement's message length. The
// events log is a JSONL stream the operator greps from a terminal — a
// runaway 1MB diff dump from an over-eager agent would shove every prior
// announcement off-screen and bloat .bosun/events.log without bound. 4KB
// is comfortable for "starting on storage layer", short stack traces, etc.
const maxAnnounceMessageBytes = 4096

// toolAnnounce implements bosun_announce. Validates the input, normalizes
// Kind to "info" when blank, and pushes onto the shared events buffer (which
// also persists to .bosun/events.log when the MCP daemon was started inside
// a repo).
func (s *Server) toolAnnounce(_ context.Context, _ *mcp.CallToolRequest, args AnnounceArgs) (*mcp.CallToolResult, AnnounceResult, error) {
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		return nil, AnnounceResult{}, errors.New("message is required")
	}
	if len(msg) > maxAnnounceMessageBytes {
		return nil, AnnounceResult{}, fmt.Errorf("message length %d exceeds limit %d bytes", len(msg), maxAnnounceMessageBytes)
	}
	sess := strings.TrimSpace(args.Session)
	if sess == "" {
		// "unknown" rather than rejecting — agents sometimes legitimately
		// don't know their session name (e.g. spawned outside a worktree).
		// Surfacing the announcement with a placeholder is more useful than
		// dropping it.
		sess = "unknown"
	}
	kind := strings.TrimSpace(args.Kind)
	if kind == "" {
		kind = "info"
	}

	e := Event{Session: sess, Kind: kind, Message: msg, At: time.Now().UTC()}
	if err := Push(e); err != nil {
		return nil, AnnounceResult{}, fmt.Errorf("push event: %w", err)
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("recorded [%s] %s: %s", kind, sess, msg)},
		},
	}, AnnounceResult{Recorded: true}, nil
}
