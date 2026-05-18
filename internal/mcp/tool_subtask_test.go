package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/state"
)

// TestSubtask_UnconfiguredServerRefuses pins the same safety default
// the spawn tool has: a server built without any cfg refuses every
// bosun_subtask call. Without this gate the handler would NPE on
// s.cfg.AgentSubtask, surfacing as a transport error.
func TestSubtask_UnconfiguredServerRefuses(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	// No WithSpawnSupport call → s.cfg is nil.

	args := SubtaskArgs{Parent: "session-1", Description: "audit"}
	result, _, err := srv.toolSubtask(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolSubtask returned Go error (want tool-result error): %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result; got success")
	}
	if msg := toolResultText(result); !strings.Contains(msg, "not configured") {
		t.Errorf("expected 'not configured' in error; got %q", msg)
	}

	// Audit log must capture the refusal — operators need to see why
	// every call was rejected even when the cause is "the operator
	// hasn't wired the daemon at all."
	requireAuditEntry(t, tmp, "refused", "config-disabled")
}

// TestSubtask_DisabledRefuses pins the operator-opt-in gate: even with
// cfg wired, the tool refuses while agent_subtask.enabled=false
// (the default).
func TestSubtask_DisabledRefuses(t *testing.T) {
	tmp := t.TempDir()
	srv := newSubtaskServer(t, tmp, false)

	args := SubtaskArgs{Parent: "session-1", Description: "audit"}
	result, _, err := srv.toolSubtask(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolSubtask returned Go error: %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result; got success")
	}
	if msg := toolResultText(result); !strings.Contains(msg, "agent_subtask is not enabled") {
		t.Errorf("expected 'agent_subtask is not enabled' in error; got %q", msg)
	}
	requireAuditEntry(t, tmp, "refused", "config-disabled")
}

// TestSubtask_ParentLivenessRefuses pins gate #2 — the calling agent
// must be running inside the parent's worktree. The same runningFn
// injection point the spawn tool's tests use; here we make every
// worktree look dead.
func TestSubtask_ParentLivenessRefuses(t *testing.T) {
	tmp := t.TempDir()
	srv := newSubtaskServer(t, tmp, true)
	srv.runningFn = func(string) (int, bool) { return 0, false }

	args := SubtaskArgs{Parent: "session-1", Description: "audit"}
	result, _, err := srv.toolSubtask(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolSubtask returned Go error: %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result; got success")
	}
	if msg := toolResultText(result); !strings.Contains(msg, "no live agent") {
		t.Errorf("expected 'no live agent' in refusal; got %q", msg)
	}
	requireAuditEntry(t, tmp, "refused", "parent-liveness")
}

// TestSubtask_InvalidArgsRefuses pins the validation surface — an
// empty parent and an invalid label both land in the invalid-args
// gate, so the audit log has a single bucket for "bad request."
func TestSubtask_InvalidArgsRefuses(t *testing.T) {
	tmp := t.TempDir()
	srv := newSubtaskServer(t, tmp, true)
	srv.runningFn = func(string) (int, bool) { return 1, true }

	for _, tc := range []struct {
		name string
		args SubtaskArgs
	}{
		{"empty parent", SubtaskArgs{Parent: "", Description: "desc"}},
		{"empty description", SubtaskArgs{Parent: "session-1", Description: ""}},
		{"whitespace description", SubtaskArgs{Parent: "session-1", Description: "   "}},
		{"bad label", SubtaskArgs{Parent: "Session 1!", Description: "desc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := srv.toolSubtask(context.Background(), nil, tc.args)
			if err != nil {
				t.Fatalf("toolSubtask Go error: %v", err)
			}
			if !isErrToolResult(result) {
				t.Fatalf("expected refusal for %v; got success", tc.args)
			}
		})
	}
}

// TestSubtask_HappyPath is the load-bearing positive test: with all
// gates wired open, a Create call returns a populated ID + RFC3339
// timestamp, the on-disk record exists, and the audit log records the
// success. Mirrors the spec's "done when an agent can call
// bosun_subtask … and get a result back" criterion.
func TestSubtask_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	srv := newSubtaskServer(t, tmp, true)
	srv.runningFn = func(string) (int, bool) { return 42, true }

	args := SubtaskArgs{
		Parent:      "session-1",
		Description: "audit internal/git for nil-pointer risks",
		Files:       []string{"internal/git/"},
	}
	result, payload, err := srv.toolSubtask(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolSubtask Go error: %v", err)
	}
	if isErrToolResult(result) {
		t.Fatalf("expected success; got refusal: %s", toolResultText(result))
	}
	if payload.ID == "" {
		t.Error("expected non-empty ID")
	}
	if !strings.HasPrefix(payload.ID, "session-1.") {
		t.Errorf("ID %q should be <parent>.<suffix>", payload.ID)
	}
	if payload.Started == "" {
		t.Error("expected non-empty Started timestamp")
	}

	// .bosun/subtasks/<parent>/<id>.json exists on disk per the brief.
	recordPath := filepath.Join(tmp, ".bosun", "subtasks", "session-1", payload.ID+".json")
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("expected record at %s: %v", recordPath, err)
	}

	requireAuditEntry(t, tmp, "success", "")
}

// TestSubtask_QuotaEnforced is the 5+1 test the brief explicitly
// calls out: with max_concurrent=5, five Create calls succeed and
// the sixth refuses on the concurrent-quota gate. Pins that the
// quota math is "active < cap allowed" not "<=".
func TestSubtask_QuotaEnforced(t *testing.T) {
	tmp := t.TempDir()
	srv := newSubtaskServer(t, tmp, true)
	srv.runningFn = func(string) (int, bool) { return 1, true }

	// MaxConcurrent is the default (5) from config.Defaults() — pin
	// the assumption locally so a future default change here surfaces
	// as a test failure.
	if srv.cfg.AgentSubtask.MaxConcurrent != config.DefaultAgentSubtaskMaxConcurrent {
		t.Fatalf("test assumes default cap of %d; got %d",
			config.DefaultAgentSubtaskMaxConcurrent, srv.cfg.AgentSubtask.MaxConcurrent)
	}

	for i := 0; i < config.DefaultAgentSubtaskMaxConcurrent; i++ {
		args := SubtaskArgs{Parent: "session-1", Description: "desc"}
		result, _, err := srv.toolSubtask(context.Background(), nil, args)
		if err != nil {
			t.Fatalf("call #%d Go error: %v", i, err)
		}
		if isErrToolResult(result) {
			t.Fatalf("call #%d unexpectedly refused: %s", i, toolResultText(result))
		}
	}

	// Sixth call should hit the cap.
	args := SubtaskArgs{Parent: "session-1", Description: "one too many"}
	result, _, err := srv.toolSubtask(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("6th call Go error: %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("6th call should hit quota; got success")
	}
	if msg := toolResultText(result); !strings.Contains(msg, "max_concurrent=5") {
		t.Errorf("expected 'max_concurrent=5' in refusal; got %q", msg)
	}

	requireAuditEntry(t, tmp, "refused", "concurrent-quota")
}

// --- helpers ---

// newSubtaskServer assembles a Server with cfg wired (so s.cfg is
// non-nil) and AgentSubtask.Enabled set per the flag. Reuses
// WithSpawnSupport because that's the production wiring point — the
// MCP daemon sets up cfg + spawn-tree together; subtask piggybacks on
// the cfg.
func newSubtaskServer(t *testing.T, repoRoot string, enabled bool) *Server {
	t.Helper()
	srv := NewServer(claims.NewStore(repoRoot), state.NewStore(repoRoot), nil)
	cfg := config.Defaults()
	cfg.AgentSubtask.Enabled = enabled
	srv.WithSpawnSupport(cfg, spawntree.NewStore(repoRoot))
	return srv
}

// requireAuditEntry asserts that .bosun/audit/subtask.log contains at
// least one JSON line whose outcome matches `outcome` and (if non-empty)
// whose refusal_gate matches `gate`. Reading the file line-by-line keeps
// the assertion robust against unrelated entries from earlier calls in
// the same test — the log is append-only and never reset between calls.
func requireAuditEntry(t *testing.T, repoRoot, outcome, gate string) {
	t.Helper()
	path := filepath.Join(repoRoot, ".bosun", "audit", "subtask.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	type entry struct {
		Outcome     string `json:"outcome"`
		RefusalGate string `json:"refusal_gate"`
		Parent      string `json:"parent"`
	}
	scanner := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for _, line := range scanner {
		if line == "" {
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse audit line %q: %v", line, err)
		}
		if e.Outcome != outcome {
			continue
		}
		if gate != "" && e.RefusalGate != gate {
			continue
		}
		return // match found
	}
	t.Errorf("expected audit entry outcome=%q gate=%q; got log:\n%s", outcome, gate, string(data))
}

