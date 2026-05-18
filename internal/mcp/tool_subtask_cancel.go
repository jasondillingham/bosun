package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/subtask"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// subtaskCancelToolDescription advertises bosun_subtask_cancel to the
// LLM. Lead with the "stop a runaway sub-task" use case so the model
// reaches for it on its own when a fan-out sub-task is misbehaving;
// the §4.5 spec layering (timeout < explicit cancel < hard kill) is
// invisible to the caller — they just write a single tool call.
var subtaskCancelToolDescription = "Cancel an in-flight sub-task spawned " +
	"by bosun_subtask. Writes a `.cancelled` marker the parent agent " +
	"checks while acting on the sub-task — cancellation is cooperative; " +
	"bosun does not kill processes. Only the parent that owns the " +
	"sub-task may cancel it. Returns cancelled=true on the first " +
	"successful marker write, cancelled=false with a detail string " +
	"for invalid ids, parent mismatches, or already-cancelled ids."

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        ToolSubtaskCancel,
			Description: subtaskCancelToolDescription,
		}, s.toolSubtaskCancel)
	})
}

// SubtaskCancelArgs is the input schema for bosun_subtask_cancel.
type SubtaskCancelArgs struct {
	Parent string `json:"parent" jsonschema:"calling session's own label (e.g. session-1); bosun verifies the id belongs to this parent"`
	ID     string `json:"id" jsonschema:"sub-task id returned by bosun_subtask (typically <parent>.sub.<seq>)"`
}

// SubtaskCancelResult mirrors the shape documented in the lane brief:
// the id echoed back so concurrent callers can correlate replies,
// `cancelled` true exactly when this call wrote the marker, and a
// human-readable `detail` explaining the refusal otherwise.
type SubtaskCancelResult struct {
	ID        string `json:"id"`
	Cancelled bool   `json:"cancelled"`
	Detail    string `json:"detail,omitempty"`
}

// Cancel-specific refusal gate identifiers. subtaskGateInvalidArgs
// is shared with the bosun_subtask creation path and defined in
// subtask_audit.go; these constants are additive for the cancel
// tool's own gates.
const (
	subtaskGateInvalidID        = "invalid-id"
	subtaskGateParentMismatch   = "parent-mismatch"
	subtaskGateAlreadyCancelled = "already-cancelled"
)

// toolSubtaskCancel implements bosun_subtask_cancel. The auth gates
// mirror bosun_spawn's pattern: a single refuse() helper keeps the
// audit-log call DRY across every return site. Refusals are logged
// via the same spawn-audit pipeline (one audit log per tool) so
// operators don't have to know which tool wrote a row to read it.
//
// Refusals — invalid-id, parent-mismatch, already-cancelled — return
// IsError=false so the parent agent sees the structured result and
// can branch on `cancelled` without a transport-level error. The
// shell-out semantics here are intentional: a model that asks "did
// the cancel land?" wants a boolean, not an exception.
func (s *Server) toolSubtaskCancel(_ context.Context, _ *mcp.CallToolRequest, args SubtaskCancelArgs) (*mcp.CallToolResult, SubtaskCancelResult, error) {
	repoRoot := s.state.RepoRoot()
	parentRaw := strings.TrimSpace(args.Parent)
	idRaw := strings.TrimSpace(args.ID)

	refuse := func(gate, detail string) (*mcp.CallToolResult, SubtaskCancelResult, error) {
		logSpawnAttempt(repoRoot, spawnAuditEntry{
			Parent:         parentRaw,
			RequestedLabel: idRaw,
			Outcome:        "refused",
			RefusalGate:    gate,
			RefusalMessage: detail,
		})
		res := SubtaskCancelResult{ID: idRaw, Cancelled: false, Detail: detail}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: detail}},
		}, res, nil
	}

	if parentRaw == "" {
		return refuse(subtaskGateInvalidArgs, "parent is required")
	}
	if idRaw == "" {
		return refuse(subtaskGateInvalidArgs, "id is required")
	}
	parent, err := session.ParseLabel(parentRaw)
	if err != nil {
		return refuse(subtaskGateInvalidArgs, err.Error())
	}

	outcome, err := subtask.Cancel(repoRoot, parent, idRaw)
	if err != nil {
		// Real I/O failure — return a transport-level error so the
		// caller sees this is bosun's fault, not a refusal.
		logSpawnAttempt(repoRoot, spawnAuditEntry{
			Parent:         parent,
			RequestedLabel: idRaw,
			Outcome:        "error",
			RefusalMessage: err.Error(),
		})
		return errResult(fmt.Errorf("cancel subtask: %w", err)), SubtaskCancelResult{ID: idRaw}, nil
	}

	switch {
	case !outcome.Found:
		return refuse(subtaskGateInvalidID, fmt.Sprintf("no sub-task with id %q in registry", idRaw))
	case !outcome.ParentMatches:
		return refuse(subtaskGateParentMismatch, fmt.Sprintf("sub-task %q does not belong to %s", idRaw, parent))
	case outcome.AlreadyCancelled:
		return refuse(subtaskGateAlreadyCancelled, fmt.Sprintf("sub-task %q is already cancelled", idRaw))
	}

	if !outcome.Wrote {
		// Belt-and-suspenders: if we reach here without writing or
		// surfacing a refusal, treat it as an unknown internal state
		// rather than silently returning success.
		return errResult(errors.New("cancel succeeded without writing marker")), SubtaskCancelResult{ID: idRaw}, nil
	}

	logSpawnAttempt(repoRoot, spawnAuditEntry{
		Parent:         parent,
		RequestedLabel: idRaw,
		Outcome:        "cancelled",
	})

	msg := fmt.Sprintf("%s: cancelled sub-task %s", parent, idRaw)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}, SubtaskCancelResult{ID: idRaw, Cancelled: true}, nil
}
