package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/git"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServer_AttachToolHappyPath drives the bosun_attach happy path: a
// known session label plus a positive PID writes
// .bosun/state/<session>.attached-pid with the supplied PID in decimal
// form. The on-disk byte shape must match what `bosun attach --pid N`
// produces from the CLI — the same liveness gate reads both.
func TestServer_AttachToolHappyPath(t *testing.T) {
	tmp := t.TempDir()
	// A single-session fake git tree so session.Derive sees session-1
	// as a real bosun-managed worktree.
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "0\n",
	}}
	client := startInProcServer(t, tmp, c)

	const pid = 12345
	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_attach",
		Arguments: map[string]any{
			"session": "session-1",
			"pid":     pid,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_attach: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_attach IsError: %+v", result)
	}

	var got AttachResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &got)
	}
	if got.Session != "session-1" {
		t.Errorf("result.Session = %q, want session-1", got.Session)
	}
	if got.PID != pid {
		t.Errorf("result.PID = %d, want %d", got.PID, pid)
	}

	// On-disk shape: decimal PID + newline, byte-identical to what
	// `bosun attach --pid N` writes from the CLI.
	body, err := os.ReadFile(filepath.Join(tmp, ".bosun", "state", "session-1.attached-pid"))
	if err != nil {
		t.Fatalf("read attached-pid: %v", err)
	}
	gotPID, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("attached-pid body = %q, want decimal pid", body)
	}
	if gotPID != pid {
		t.Errorf("on-disk pid = %d, want %d", gotPID, pid)
	}
}

// TestServer_AttachToolAcceptsNumericSession exercises the bare-number
// shortcut ParseLabel allows — "1" canonicalizes to "session-1" the same
// way the CLI does, so wrapper scripts can pass either form.
func TestServer_AttachToolAcceptsNumericSession(t *testing.T) {
	tmp := t.TempDir()
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "0\n",
	}}
	client := startInProcServer(t, tmp, c)

	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_attach",
		Arguments: map[string]any{
			"session": "1",
			"pid":     999,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_attach: %v", err)
	}
	if result.IsError {
		t.Fatalf("numeric session form should resolve to session-1, got IsError: %+v", result)
	}

	// File must land under the canonical session-1 name, not "1".
	if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-1.attached-pid")); err != nil {
		t.Fatalf("expected canonical session-1.attached-pid file: %v", err)
	}
}

// TestServer_AttachToolUnknownSessionRefuses: typo against an out-of-range
// session must not silently create an orphan attached-pid file under
// .bosun/state/. Mirrors cmd_attach.go's "unknown session" gate so the CLI
// and MCP paths share the same operator contract.
func TestServer_AttachToolUnknownSessionRefuses(t *testing.T) {
	tmp := t.TempDir()
	// Fake git advertises only session-1; "session-9" is not bosun-managed.
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "0\n",
	}}
	client := startInProcServer(t, tmp, c)

	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_attach",
		Arguments: map[string]any{
			"session": "session-9",
			"pid":     12345,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_attach: %v", err)
	}
	if !result.IsError {
		t.Fatalf("unknown session should be refused, got success: %+v", result)
	}
	// And no orphan state file under .bosun/state/session-9.*.
	stateDir := filepath.Join(tmp, ".bosun", "state")
	entries, _ := os.ReadDir(stateDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "session-9.") {
			t.Errorf("attach refusal left orphan state file %s", e.Name())
		}
	}
}

// TestServer_AttachToolInvalidPIDRefuses: a non-positive PID is nonsense
// (PIDs start at 1 on every supported OS) — refuse before touching disk.
func TestServer_AttachToolInvalidPIDRefuses(t *testing.T) {
	tmp := t.TempDir()
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "0\n",
	}}
	client := startInProcServer(t, tmp, c)

	cases := []struct {
		name string
		pid  int
	}{
		{"zero", 0},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
				Name: "bosun_attach",
				Arguments: map[string]any{
					"session": "session-1",
					"pid":     tc.pid,
				},
			})
			if err != nil {
				t.Fatalf("call bosun_attach: %v", err)
			}
			if !result.IsError {
				t.Fatalf("pid=%d should be refused, got success: %+v", tc.pid, result)
			}
			if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-1.attached-pid")); err == nil {
				t.Errorf("invalid pid=%d should not have written attached-pid", tc.pid)
			}
		})
	}
}

// TestServer_AttachToolEmptySessionRefuses: a missing/blank session field
// is a structured IsError, not a transport-level failure.
func TestServer_AttachToolEmptySessionRefuses(t *testing.T) {
	client := startInProcServer(t, t.TempDir(), nil)

	result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_attach",
		Arguments: map[string]any{
			"session": "   ",
			"pid":     1,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_attach: %v", err)
	}
	if !result.IsError {
		t.Fatalf("blank session should be refused, got success: %+v", result)
	}
}

// TestServer_AttachToolAdvertised: ListTools advertises bosun_attach so a
// fresh agent connecting to the daemon discovers the capability.
func TestServer_AttachToolAdvertised(t *testing.T) {
	client := startInProcServer(t, t.TempDir(), nil)

	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !hasTool(tools.Tools, "bosun_attach") {
		t.Fatalf("bosun_attach not advertised, got: %v", toolNames(tools.Tools))
	}
}
