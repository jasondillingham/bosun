package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name: "bosun_attach",
			Description: "Register an explicit liveness PID for a bosun session. " +
				"In-container equivalent of `bosun attach <session> --pid <pid>` — used " +
				"by wrapper scripts that can't shell out to the bosun binary (the binary " +
				"isn't installed inside containers). Writes " +
				".bosun/state/<session>.attached-pid so the liveness gate recognizes " +
				"workers the proc-scan can't see (sub-agents launched via Task, CI " +
				"runners, containerized workers). Refuses unknown sessions and " +
				"non-positive PIDs.",
		}, s.toolAttach)
	})
}

// AttachArgs is the input schema for bosun_attach.
type AttachArgs struct {
	Session string `json:"session" jsonschema:"the bosun session to register the PID against (e.g. session-2 or 2)"`
	PID     int    `json:"pid" jsonschema:"PID of the worker process to register as the live agent for the session — must be a positive integer"`
}

// AttachResult is the structured output for bosun_attach.
type AttachResult struct {
	Session string `json:"session" jsonschema:"canonical session label the PID was registered against"`
	PID     int    `json:"pid" jsonschema:"PID recorded in the attached-pid file"`
}

// toolAttach implements bosun_attach. Validates the session label, refuses
// labels that don't match a bosun-managed session (re-derived live so a
// typo can't create an orphan state file), refuses non-positive PIDs, then
// writes the attached-pid via state.Store.WriteAttachedPID — byte-identical
// to what `bosun attach --pid N` writes from the CLI.
//
// Heartbeat-clear semantics live elsewhere; this tool only writes.
func (s *Server) toolAttach(ctx context.Context, _ *mcp.CallToolRequest, args AttachArgs) (*mcp.CallToolResult, AttachResult, error) {
	raw := strings.TrimSpace(args.Session)
	if raw == "" {
		return errResult(errors.New("session is required")), AttachResult{}, nil
	}
	label, err := session.ParseLabel(raw)
	if err != nil {
		return errResult(err), AttachResult{}, nil
	}
	if args.PID <= 0 {
		return errResult(fmt.Errorf("pid must be a positive integer, got %d", args.PID)), AttachResult{}, nil
	}

	// Re-validate the session against the live worktree set — the same
	// "no orphan state files" gate cmd_attach.go uses. Without this gate
	// a typo against `bosun_attach session-99` would silently create
	// .bosun/state/session-99.attached-pid that session.Derive will
	// ignore but operators would have to clean up by hand.
	repoRoot := s.state.RepoRoot()
	cfg, err := config.Load(repoRoot)
	if err != nil {
		return errResult(fmt.Errorf("load config: %w", err)), AttachResult{}, nil
	}
	if s.gitClient == nil {
		// Without a git client we can't Derive — refuse rather than
		// silently skipping the auth gate. Production wiring
		// (cmd_mcp.go) always passes a git client; in-process tests
		// that exercise bosun_attach must do the same.
		return errResult(errors.New("bosun_attach requires a git client to validate the session")), AttachResult{}, nil
	}
	sess, err := findSessionByLabel(ctx, s, cfg, repoRoot, label)
	if err != nil {
		return errResult(err), AttachResult{}, nil
	}
	if sess == nil {
		return errResult(fmt.Errorf("%s not found", label)), AttachResult{}, nil
	}

	if err := s.state.WriteAttachedPID(label, args.PID); err != nil {
		return errResult(fmt.Errorf("write attached-pid: %w", err)), AttachResult{}, nil
	}
	summary := fmt.Sprintf("%s attached pid=%d", label, args.PID)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, AttachResult{Session: label, PID: args.PID}, nil
}
