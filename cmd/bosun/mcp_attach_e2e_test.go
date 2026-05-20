package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCP_AttachToolEndToEnd spawns the real `bosun mcp` subcommand,
// connects a client over the Unix socket, and exercises the bosun_attach
// tool against a session-1 worktree created with `bosun init`. The test
// asserts the attached-pid file lands at
// .bosun/state/session-1.attached-pid with the supplied PID — the same
// liveness signal the CLI `bosun attach --pid N` produces.
//
// Phase 3 lane-0 acceptance: wrapper scripts running inside containers
// can't shell out to the bosun binary (it isn't installed in-container),
// so this MCP path is the only way for those workers to register a
// liveness PID with the daemon.
func TestMCP_AttachToolEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)
	s.Bosun("init", "1")

	// Use /tmp for the socket path — t.TempDir() returns deeply-nested
	// paths under /var/folders/.../T on macOS that routinely exceed the
	// 104-byte Unix-domain socket limit.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-mcp-attach-e2e-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bosunBin, "mcp", "--socket", socketPath)
	cmd.Dir = s.repo
	var subOut subprocessTail
	cmd.Stdout = &subOut
	cmd.Stderr = &subOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun mcp: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if err := waitForSocket(socketPath, 3*time.Second); err != nil {
		t.Fatalf("socket never appeared: %v\nsubprocess output:\n%s", err, subOut.String())
	}

	netConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer netConn.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "bosun-attach-e2e-client",
		Version: "test",
	}, nil)
	clientSess, err := client.Connect(ctx, &netConnTransport{conn: netConn}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSess.Close()

	tools, err := clientSess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !mcpToolNamed(tools.Tools, "bosun_attach") {
		t.Fatalf("bosun_attach not advertised by daemon: %v", tools.Tools)
	}

	const pid = 54321
	result, err := clientSess.CallTool(ctx, &mcpsdk.CallToolParams{
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

	// On-disk shape: decimal PID + newline, byte-identical to the CLI path.
	body, err := os.ReadFile(filepath.Join(s.repo, ".bosun", "state", "session-1.attached-pid"))
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

// TestMCP_AttachToolRefusesUnknownSession asserts the daemon refuses
// bosun_attach calls against sessions that don't exist on disk — the
// MCP path must preserve the same "no orphan state files" gate the CLI
// (cmd_attach.go) enforces. Without this, a typo in a wrapper script
// would silently litter .bosun/state/ with files for sessions that
// session.Derive ignores.
func TestMCP_AttachToolRefusesUnknownSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)
	s.Bosun("init", "1") // only session-1 exists

	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-mcp-attach-deny-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bosunBin, "mcp", "--socket", socketPath)
	cmd.Dir = s.repo
	var subOut subprocessTail
	cmd.Stdout = &subOut
	cmd.Stderr = &subOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun mcp: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if err := waitForSocket(socketPath, 3*time.Second); err != nil {
		t.Fatalf("socket never appeared: %v\nsubprocess output:\n%s", err, subOut.String())
	}

	netConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer netConn.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "bosun-attach-deny-client",
		Version: "test",
	}, nil)
	clientSess, err := client.Connect(ctx, &netConnTransport{conn: netConn}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSess.Close()

	result, err := clientSess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_attach",
		Arguments: map[string]any{
			"session": "session-9", // not bosun-managed
			"pid":     12345,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_attach: %v", err)
	}
	if !result.IsError {
		t.Fatalf("attach against unknown session should be IsError, got success: %+v", result)
	}

	// No orphan state file under .bosun/state/session-9.*.
	stateDir := filepath.Join(s.repo, ".bosun", "state")
	entries, _ := os.ReadDir(stateDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "session-9.") {
			t.Errorf("attach refusal left orphan state file %s", e.Name())
		}
	}
}
