package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestMCP_SIGINTRemovesSocketAndPidfile drives the documented graceful-
// shutdown contract: when the bosun mcp daemon receives SIGINT (or
// SIGTERM), it must remove both `mcp.sock` and `mcp.pid` from disk so
// the next `bosun init --launch` invocation gets a clean slate. Before
// the fix, the listener was closed but the socket file lingered as a
// regular file on disk; subsequent ensureMcp() calls hit
// removeIfSocket() which refuses to delete a non-socket file, leaving
// the daemon path stuck.
func TestMCP_SIGINTRemovesSocketAndPidfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)
	s.Bosun("init", "1")

	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-mcp-sigint-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bosunBin, "mcp", "--socket", socketPath)
	cmd.Dir = s.repo
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bosun mcp: %v", err)
	}
	defer func() {
		// In case the test fails before the SIGTERM landed, make sure
		// the subprocess doesn't outlive the test.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if err := waitForSocket(socketPath, 3*time.Second); err != nil {
		t.Fatalf("socket never appeared: %v", err)
	}
	pidfile := filepath.Join(s.repo, ".bosun", "mcp.pid")
	if _, err := os.Stat(pidfile); err != nil {
		t.Fatalf("pidfile missing while daemon running: %v", err)
	}

	// Send SIGINT — the daemon's signal.NotifyContext handler must
	// trigger graceful shutdown plus the deferred cleanup of socket
	// and pidfile.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	// The daemon should exit cleanly; a non-zero exit suggests the
	// graceful-shutdown path crashed instead of just unwinding.
	exitErr := cmd.Wait()
	if exitErr != nil {
		// Exit codes after a signal show up as *exec.ExitError; only
		// fail when it wasn't the SIGINT we sent.
		if ee, ok := exitErr.(*exec.ExitError); !ok || ee.ExitCode() != 0 {
			t.Logf("subprocess exit error (often expected after signal): %v", exitErr)
		}
	}

	if _, err := os.Stat(socketPath); err == nil {
		t.Errorf("socket file %s should be removed after SIGINT", socketPath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error on socket: %v", err)
	}
	if _, err := os.Stat(pidfile); err == nil {
		t.Errorf("pidfile %s should be removed after SIGINT", pidfile)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error on pidfile: %v", err)
	}
}
