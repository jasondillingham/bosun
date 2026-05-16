package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/mcp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCP_EndToEndOverUnixSocket spawns the real `bosun mcp` subcommand,
// connects a client over the Unix socket it binds, and exercises the
// bosun_check tool. Round-0 acceptance test for the daemon-mode lifecycle.
func TestMCP_EndToEndOverUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)

	// Seed a claim via the CLI so bosun_check has something to find. The
	// MCP server reads the same .bosun/claims/ the CLI writes — that's the
	// filesystem-compat contract documented in docs/mcp-protocol.md.
	s.Bosun("init", "1")
	s.Bosun("claim", "session-1", "internal/auth/handler.go")

	// Use /tmp for the socket path — t.TempDir() returns a deeply-nested
	// path under /var/folders/.../T/ on macOS, which routinely exceeds the
	// 104-byte Unix-domain socket limit. The socket file is named per-test
	// to avoid collisions when scenarios run in parallel.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-mcp-e2e-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start `bosun mcp --socket <socketPath>` as a subprocess. Capture
	// its stdio so a startup failure surfaces in the test log instead of
	// silently producing a "timeout waiting for socket".
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
		Name:    "bosun-e2e-client",
		Version: "test",
	}, nil)
	session, err := client.Connect(ctx, &netConnTransport{conn: netConn}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !mcpToolNamed(tools.Tools, "bosun_check") {
		t.Fatalf("bosun_check not in tools list: %+v", tools.Tools)
	}

	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_check",
		Arguments: map[string]any{
			"paths": []string{"internal/auth/handler.go"},
		},
	})
	if err != nil {
		t.Fatalf("call bosun_check: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_check IsError: %+v", result)
	}

	var got mcp.CheckResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &got)
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %+v", got)
	}
	if got.Conflicts[0].Path != "internal/auth/handler.go" {
		t.Errorf("conflict path = %q", got.Conflicts[0].Path)
	}
	if len(got.Conflicts[0].Sessions) != 1 || got.Conflicts[0].Sessions[0] != "session-1" {
		t.Errorf("conflict sessions = %v, want [session-1]", got.Conflicts[0].Sessions)
	}
}

// mcpToolNamed reports whether the tool list advertises a tool with name.
// Kept here rather than at use-site so adding a tool doesn't force this
// e2e file to track expected counts.
func mcpToolNamed(tools []*mcpsdk.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// waitForSocket polls until a Unix socket accepts connections at path, or
// timeout. The server binds the listener before printing its banner, so
// dial-then-close is a reliable readiness probe.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket %s", path)
}

// netConnTransport adapts a *net.Conn (Unix socket client side) into the
// mcp.Transport that the SDK's client expects. Mirrors the server-side
// connTransport in internal/mcp/transport.go.
type netConnTransport struct {
	conn net.Conn
}

func (t *netConnTransport) Connect(_ context.Context) (mcpsdk.Connection, error) {
	return &netConnConn{
		conn: t.conn,
		r:    bufio.NewReader(t.conn),
	}, nil
}

type netConnConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func (c *netConnConn) Read(_ context.Context) (jsonrpc.Message, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return jsonrpc.DecodeMessage(line[:len(line)-1])
}

func (c *netConnConn) Write(_ context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(data); err != nil {
		return err
	}
	_, err = c.conn.Write([]byte{'\n'})
	return err
}

func (c *netConnConn) Close() error      { return c.conn.Close() }
func (c *netConnConn) SessionID() string { return "" }

// subprocessTail buffers everything the subprocess writes so the test can
// dump it into the failure message if something goes wrong at startup.
type subprocessTail struct {
	buf []byte
}

func (t *subprocessTail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	return len(p), nil
}

func (t *subprocessTail) String() string { return string(t.buf) }
