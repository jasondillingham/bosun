package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/usage"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// usageHappyClient wires a bosun MCP server with a single bosun-managed
// session at tmp + "-bosun-1" so bosun_usage's session-validation gate
// finds it. Returns the connected client.
func usageHappyClient(t *testing.T, tmp string) *mcpsdk.ClientSession {
	t.Helper()
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "0\n",
	}}
	return startInProcServer(t, tmp, c)
}

// TestServer_UsageToolHappyPath: a valid call appends to the ledger
// and the structured result reports the running totals (= this call's
// values since the ledger was empty).
func TestServer_UsageToolHappyPath(t *testing.T) {
	tmp := t.TempDir()
	client := usageHappyClient(t, tmp)

	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_usage",
		Arguments: map[string]any{
			"session":    "session-1",
			"tokens_in":  1234,
			"tokens_out": 567,
			"cost_usd":   0.0234,
			"model":      "claude-sonnet-4-6",
			"turn_label": "design",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_usage: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_usage IsError: %+v", result)
	}

	var got UsageResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &got)
	}
	if got.Session != "session-1" {
		t.Errorf("result.Session = %q, want session-1", got.Session)
	}
	if got.TotalIn != 1234 || got.TotalOut != 567 {
		t.Errorf("token totals: in=%d out=%d, want 1234/567", got.TotalIn, got.TotalOut)
	}
	if got.TotalCost < 0.0233 || got.TotalCost > 0.0235 {
		t.Errorf("total cost = %f, want ~0.0234", got.TotalCost)
	}
	if got.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", got.TurnCount)
	}

	// On-disk shape: one JSON-encoded line.
	body, err := os.ReadFile(filepath.Join(tmp, ".bosun", "state", "session-1.usage"))
	if err != nil {
		t.Fatalf("read usage ledger: %v", err)
	}
	if string(body) == "" {
		t.Error("usage ledger empty after append")
	}
}

// TestServer_UsageToolAccumulates: multiple turns recorded back-to-back
// produce summed totals. Pins the running-totals contract for the
// agent caller — second call reports the SUM, not the second turn's
// values in isolation.
func TestServer_UsageToolAccumulates(t *testing.T) {
	tmp := t.TempDir()
	client := usageHappyClient(t, tmp)

	calls := []map[string]any{
		{"session": "session-1", "tokens_in": 100, "tokens_out": 50, "cost_usd": 0.01, "model": "opus"},
		{"session": "session-1", "tokens_in": 200, "tokens_out": 100, "cost_usd": 0.03, "model": "opus"},
		{"session": "session-1", "tokens_in": 50, "tokens_out": 25, "cost_usd": 0.005, "model": "haiku"},
	}
	for i, args := range calls {
		result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
			Name:      "bosun_usage",
			Arguments: args,
		})
		if err != nil || result.IsError {
			t.Fatalf("call #%d: err=%v isErr=%v", i, err, result)
		}
	}

	// Cumulative via the package API to double-check the ledger.
	totals, err := usage.ReadTotals(tmp, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if totals.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", totals.TurnCount)
	}
	if totals.TokensIn != 350 || totals.TokensOut != 175 {
		t.Errorf("tokens: in=%d out=%d, want 350/175", totals.TokensIn, totals.TokensOut)
	}
	if totals.CostUSD < 0.044 || totals.CostUSD > 0.046 {
		t.Errorf("cost = %f, want ~0.045", totals.CostUSD)
	}
	// LastModel is whatever the most-recent entry recorded.
	if totals.LastModel != "haiku" {
		t.Errorf("LastModel = %q, want haiku", totals.LastModel)
	}
}

// TestServer_UsageToolAcceptsNumericSession: the bare "2" shortcut
// canonicalizes to "session-2" — same parser bosun_attach uses, so
// wrapper scripts can pass either form.
func TestServer_UsageToolAcceptsNumericSession(t *testing.T) {
	tmp := t.TempDir()
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-2\nHEAD bbb\nbranch refs/heads/bosun/session-2\n\n",
		revCount: "0\n",
	}}
	client := startInProcServer(t, tmp, c)

	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_usage",
		Arguments: map[string]any{
			"session":   "2",
			"tokens_in": 10,
			"cost_usd":  0.001,
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.IsError {
		t.Fatalf("numeric session form should resolve to session-2, got IsError: %+v", result)
	}
	// File must land under the canonical session-2 name.
	if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-2.usage")); err != nil {
		t.Errorf("expected ledger at session-2.usage, stat err: %v", err)
	}
}

// TestServer_UsageToolRefusesUnknownSession: orphan-state guard. A
// typo against `bosun_usage session-99` must NOT silently create a
// .bosun/state/session-99.usage file — no live worktree for it
// means no renderer will ever surface the data.
func TestServer_UsageToolRefusesUnknownSession(t *testing.T) {
	tmp := t.TempDir()
	client := usageHappyClient(t, tmp)

	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_usage",
		Arguments: map[string]any{
			"session":   "session-99",
			"tokens_in": 100,
			"cost_usd":  0.01,
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error for unknown session, got success")
	}
	// And no orphan ledger file.
	if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-99.usage")); !os.IsNotExist(err) {
		t.Errorf("orphan ledger should not exist, stat err: %v", err)
	}
}

// TestServer_UsageToolRefusesNegativeValues guards the operator-side
// error of feeding bosun_usage parsed-incorrectly token counts (e.g.
// a wrapper that subtracts cached tokens and goes negative). Reject
// at the boundary so the ledger stays clean.
func TestServer_UsageToolRefusesNegativeValues(t *testing.T) {
	tmp := t.TempDir()
	client := usageHappyClient(t, tmp)

	for _, bad := range []map[string]any{
		{"session": "session-1", "tokens_in": -1, "cost_usd": 0.0},
		{"session": "session-1", "tokens_out": -1, "cost_usd": 0.0},
		{"session": "session-1", "tokens_in": 0, "cost_usd": -0.001},
	} {
		result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
			Name:      "bosun_usage",
			Arguments: bad,
		})
		if err != nil {
			t.Fatalf("call (%v): %v", bad, err)
		}
		if !result.IsError {
			t.Errorf("expected error for negative value %v, got success", bad)
		}
	}
}

// TestServer_UsageToolRefusesEmptySession surfaces the operator-side
// bug of forgetting to thread the label through.
func TestServer_UsageToolRefusesEmptySession(t *testing.T) {
	tmp := t.TempDir()
	client := usageHappyClient(t, tmp)

	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_usage",
		Arguments: map[string]any{
			"session":   "",
			"tokens_in": 10,
			"cost_usd":  0.01,
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for blank session")
	}
}

// TestServer_UsageToolListed: the tool appears in ListTools. Catches
// a missed `init()` registration before any other test does.
func TestServer_UsageToolListed(t *testing.T) {
	tmp := t.TempDir()
	client := usageHappyClient(t, tmp)

	tools, err := client.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "bosun_usage" {
			return
		}
	}
	t.Errorf("bosun_usage not advertised in ListTools")
}
