package mcp

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRegisterCustomTools_HappyPath exercises a successful exec via
// the operator-defined tool path. We pick `echo` because it's POSIX-
// guaranteed (skip on Windows) and trivially deterministic. The
// agent's view is what we assert on: stdout flows through, the call
// is not IsError, and the structured result carries the same data.
func TestRegisterCustomTools_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX echo; cmd.exe quoting is a separate concern")
	}
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	names := RegisterCustomTools(srv, []config.MCPToolDef{{
		Name:        "bosun_echo",
		Description: "Echoes back its argv",
		Command:     []string{"echo", "hello"},
	}})
	if len(names) != 1 || names[0] != "bosun_echo" {
		t.Fatalf("RegisterCustomTools returned %v, want [bosun_echo]", names)
	}

	session, cancel, done := startTestSession(t, srv)
	defer cancel()
	defer session.Close()

	// Sanity: the tool appears in tools/list alongside built-ins.
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !hasTool(tools.Tools, "bosun_echo") {
		t.Fatalf("bosun_echo not advertised, got: %v", toolNames(tools.Tools))
	}

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_echo",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool reported IsError; content=%+v", result.Content)
	}

	// Text content should carry stdout (trimmed of trailing newline).
	if len(result.Content) == 0 {
		t.Fatalf("no content returned")
	}
	tc, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("first content is %T, want *TextContent", result.Content[0])
	}
	if strings.TrimSpace(tc.Text) != "hello" {
		t.Errorf("text = %q, want \"hello\"", tc.Text)
	}

	// Structured result should match.
	var sr CustomToolResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &sr)
	}
	if strings.TrimSpace(sr.Stdout) != "hello" {
		t.Errorf("structured Stdout = %q, want \"hello\"", sr.Stdout)
	}
	if sr.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", sr.ExitCode)
	}

	session.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit")
	}
}

// TestRegisterCustomTools_AgentArgsAppended confirms args from the
// agent's call are appended to the configured command — so an
// operator's "bosun_lint" can be passed `--fix` by the agent.
func TestRegisterCustomTools_AgentArgsAppended(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX echo")
	}
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	RegisterCustomTools(srv, []config.MCPToolDef{{
		Name:        "bosun_echo_concat",
		Description: "echo with an operator prefix",
		Command:     []string{"echo", "prefix"},
	}})

	session, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer session.Close()

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_echo_concat",
		Arguments: map[string]any{"args": []string{"agentArg1", "agentArg2"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError; content=%+v", result.Content)
	}
	text := strings.TrimSpace(result.Content[0].(*mcpsdk.TextContent).Text)
	if text != "prefix agentArg1 agentArg2" {
		t.Errorf("text = %q, want \"prefix agentArg1 agentArg2\"", text)
	}
}

// TestRegisterCustomTools_NonZeroExit confirms a tool that exits
// non-zero surfaces IsError with stderr in the message and the exit
// code in the structured result. This is the path operators rely on
// for "tool failed, here's why" decision-making by the agent.
func TestRegisterCustomTools_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	RegisterCustomTools(srv, []config.MCPToolDef{{
		Name:        "bosun_fail",
		Description: "always fails",
		Command:     []string{"sh", "-c", "echo bad-news >&2; exit 7"},
	}})

	session, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer session.Close()

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "bosun_fail"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError for exit 7, got success")
	}
	var sr CustomToolResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &sr)
	}
	if sr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", sr.ExitCode)
	}
	if !strings.Contains(sr.Stderr, "bad-news") {
		t.Errorf("Stderr = %q, want to contain \"bad-news\"", sr.Stderr)
	}
}

// TestRegisterCustomTools_MissingBinaryIsError confirms a typo in
// the operator's config (binary not on PATH) surfaces as IsError
// instead of silently doing nothing. The error message should
// include the tool name so the operator can find the bad def.
func TestRegisterCustomTools_MissingBinaryIsError(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	RegisterCustomTools(srv, []config.MCPToolDef{{
		Name:        "bosun_typo",
		Description: "binary does not exist",
		Command:     []string{"/this/binary/does/not/exist-bosun-test"},
	}})

	session, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer session.Close()

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "bosun_typo"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError for missing binary")
	}
	// The error text should name the tool so the operator's eye
	// catches the bad def in their config.
	combined := ""
	for _, c := range result.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			combined += tc.Text
		}
	}
	if !strings.Contains(combined, "bosun_typo") {
		t.Errorf("error text didn't mention tool name; got %q", combined)
	}
}

// TestRegisterCustomTools_TimeoutCancels confirms a tool that runs
// longer than its timeout gets killed and surfaces an explicit
// timeout error — not a "succeeded silently" false negative that
// would leave the operator confused why nothing happened.
func TestRegisterCustomTools_TimeoutCancels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep")
	}
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	RegisterCustomTools(srv, []config.MCPToolDef{{
		Name:           "bosun_slow",
		Description:    "sleeps too long",
		Command:        []string{"sleep", "30"},
		TimeoutSeconds: 1,
	}})

	session, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer session.Close()

	start := time.Now()
	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "bosun_slow"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError on timeout")
	}
	if elapsed > 5*time.Second {
		t.Errorf("call took %v, expected ~1s (timeout cancel)", elapsed)
	}
	combined := ""
	for _, c := range result.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			combined += tc.Text
		}
	}
	if !strings.Contains(combined, "timeout") && !strings.Contains(combined, "exceeded") {
		t.Errorf("error should mention timeout, got %q", combined)
	}
}

// TestRegisterCustomTools_SkipsEmptyCommand defensively confirms a
// corrupt def (empty Command slice — shouldn't pass Validate but
// the in-process API allows it) is silently skipped rather than
// crashing the server. Validate is the primary defense; this is the
// belt to its suspenders.
func TestRegisterCustomTools_SkipsEmptyCommand(t *testing.T) {
	tmp := t.TempDir()
	srv := NewServer(claims.NewStore(tmp), state.NewStore(tmp), nil)
	names := RegisterCustomTools(srv, []config.MCPToolDef{
		{Name: "bosun_empty", Description: "no cmd", Command: nil},
		{Name: "bosun_blank", Description: "blank cmd", Command: []string{"  "}},
	})
	if len(names) != 0 {
		t.Errorf("expected 0 registered tools for corrupt defs, got %v", names)
	}
}
