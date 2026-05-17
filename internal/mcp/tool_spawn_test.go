package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSpawn_UnconfiguredServerRefuses pins the safety default: a
// server built without WithSpawnSupport refuses every bosun_spawn
// call cleanly, no matter how the args look. Operators who never
// flip the spawn gate get zero exposure.
func TestSpawn_UnconfiguredServerRefuses(t *testing.T) {
	tmp := t.TempDir()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, nil)
	// NO WithSpawnSupport call.

	args := SpawnArgs{Parent: "session-1", Brief: "## auth\n\nbody\n", Launch: false}
	result, _, err := srv.toolSpawn(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolSpawn returned a Go error (should be a tool-result error): %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result; got success")
	}
	msg := toolResultText(result)
	if !strings.Contains(msg, "not configured") {
		t.Errorf("expected 'not configured' in error; got %q", msg)
	}
}

// TestSpawn_DisabledRefuses pins the operator-opt-in gate: even with
// WithSpawnSupport wired, the tool refuses while config has
// agent_spawn.enabled=false (the default).
func TestSpawn_DisabledRefuses(t *testing.T) {
	tmp := t.TempDir()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, nil)

	cfg := config.Defaults()
	// Explicit: enabled remains false.
	cfg.AgentSpawn.Enabled = false
	srv.WithSpawnSupport(cfg, spawntree.NewStore(tmp))

	args := SpawnArgs{Parent: "session-1", Brief: "## auth\n\nbody\n", Launch: false}
	result, _, err := srv.toolSpawn(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolSpawn returned a Go error: %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result; got success")
	}
	msg := toolResultText(result)
	if !strings.Contains(msg, "agent_spawn is not enabled") {
		t.Errorf("expected 'agent_spawn is not enabled' in error; got %q", msg)
	}
}

// TestSpawn_WhitelistRefusesNonAllowed pins the per-session
// whitelist when configured. If allowed_for_sessions is set and the
// caller's parent isn't in it, the tool refuses before any tree or
// proc check fires.
func TestSpawn_WhitelistRefusesNonAllowed(t *testing.T) {
	tmp := t.TempDir()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, nil)

	cfg := config.Defaults()
	cfg.AgentSpawn.Enabled = true
	cfg.AgentSpawn.AllowedForSessions = []string{"session-only-this-one"}
	srv.WithSpawnSupport(cfg, spawntree.NewStore(tmp))

	args := SpawnArgs{Parent: "session-1", Brief: "## auth\n\nbody\n", Launch: false}
	result, _, err := srv.toolSpawn(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolSpawn returned a Go error: %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result; got success")
	}
	msg := toolResultText(result)
	if !strings.Contains(msg, "whitelist") {
		t.Errorf("expected 'whitelist' in error; got %q", msg)
	}
}

// TestIsParentAllowedToSpawn_EmptyListMeansAny pins the documented
// default: an empty AllowedForSessions list is the "any session may
// spawn" sentinel.
func TestIsParentAllowedToSpawn_EmptyListMeansAny(t *testing.T) {
	cfg := config.AgentSpawnConfig{Enabled: true}
	for _, label := range []string{"session-1", "auth", "session-1.sub"} {
		if !isParentAllowedToSpawn(label, cfg) {
			t.Errorf("empty whitelist should allow %q", label)
		}
	}
}

// --- test helpers ---

func isErrToolResult(r *mcpsdk.CallToolResult) bool {
	if r == nil {
		return false
	}
	return r.IsError
}

func toolResultText(r *mcpsdk.CallToolResult) string {
	if r == nil {
		return ""
	}
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
