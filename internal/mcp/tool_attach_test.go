package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/proc"
	"github.com/jasondillingham/bosun/internal/state"
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
		name      string
		pid       int
		wantMatch string
	}{
		{"zero", 0, "positive integer"},
		{"negative", -1, "positive integer"},
		// v0.12 L2: PID 1 (init/launchd) is refused even though it's
		// "positive and alive" — bosun workers are never PID 1, and
		// proc.IsAlive can't disprove PID 1 so the false-positive
		// would be permanent.
		{"init_pid", 1, "init/launchd"},
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
			if tc.wantMatch != "" && !strings.Contains(toolResultText(result), tc.wantMatch) {
				t.Errorf("error message missing %q\n  got: %s", tc.wantMatch, toolResultText(result))
			}
		})
	}
}

// TestCwdInsideWorktree pins the helper's contract. Lives close to
// the function it tests; the integration-level check below
// exercises the call site indirectly.
func TestCwdInsideWorktree(t *testing.T) {
	cases := []struct {
		name     string
		cwd      string
		worktree string
		want     bool
	}{
		{"exact match", "/repo/wt-1", "/repo/wt-1", true},
		{"descendant", "/repo/wt-1/sub", "/repo/wt-1", true},
		{"deep descendant", "/repo/wt-1/a/b/c", "/repo/wt-1", true},
		{"trailing slash in worktree", "/repo/wt-1/sub", "/repo/wt-1/", true},
		{"sibling false-match guard", "/repo/wt-10", "/repo/wt-1", false},
		{"unrelated path", "/other", "/repo/wt-1", false},
		{"parent of worktree", "/repo", "/repo/wt-1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cwdInsideWorktree(tc.cwd, tc.worktree); got != tc.want {
				t.Errorf("cwdInsideWorktree(%q, %q) = %v, want %v", tc.cwd, tc.worktree, got, tc.want)
			}
		})
	}
}

// TestServer_AttachToolCwdValidationRefusesMismatch pins the v0.12
// L2 fix: when proc.Cwd can resolve the registered PID's working
// directory (Linux today; macOS/Windows return ErrCwdUnsupported and
// bypass the check), the handler refuses if the cwd doesn't sit
// inside the session worktree. Whitebox-style — fakes pidCwdFn so
// the test doesn't need a real worker process and works on macOS CI.
func TestServer_AttachToolCwdValidationRefusesMismatch(t *testing.T) {
	tmp := t.TempDir()
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "0\n",
	}}
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, c)
	srv.pidCwdFn = func(pid int) (string, error) {
		return "/somewhere/else/entirely", nil
	}

	args := AttachArgs{Session: "session-1", PID: 4242}
	result, _, err := srv.toolAttach(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolAttach returned a Go error: %v", err)
	}
	if !isErrToolResult(result) {
		t.Fatal("expected error tool result for cwd-outside-worktree; got success")
	}
	msg := toolResultText(result)
	for _, want := range []string{"4242", "not inside", "worktree"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\n  got: %s", want, msg)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-1.attached-pid")); err == nil {
		t.Error("cwd-mismatch refusal should not have written attached-pid")
	}
}

// TestServer_AttachToolCwdValidationSkipsWhenUnsupported pins the
// fallback path: macOS and Windows return ErrCwdUnsupported and the
// handler must NOT refuse — the check is best-effort by design.
func TestServer_AttachToolCwdValidationSkipsWhenUnsupported(t *testing.T) {
	tmp := t.TempDir()
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "0\n",
	}}
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, c)
	srv.pidCwdFn = func(pid int) (string, error) {
		return "", proc.ErrCwdUnsupported
	}

	args := AttachArgs{Session: "session-1", PID: 4242}
	result, _, err := srv.toolAttach(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("toolAttach returned a Go error: %v", err)
	}
	if isErrToolResult(result) {
		t.Fatalf("ErrCwdUnsupported should fall through, not refuse; got: %s", toolResultText(result))
	}
	// Attached-pid file should have been written despite the unsupported lookup.
	if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-1.attached-pid")); err != nil {
		t.Errorf("attached-pid missing after ErrCwdUnsupported fall-through: %v", err)
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
