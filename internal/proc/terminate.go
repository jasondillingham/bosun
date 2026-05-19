package proc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// DefaultTerminateGrace is the wall-clock window Terminate gives a
// process to exit after SIGTERM before escalating to SIGKILL. Tuned for
// Claude Code's typical shutdown path (flushes session state, closes
// any open file handles, exits — usually well under 1s).
const DefaultTerminateGrace = 2 * time.Second

// Terminate sends SIGTERM to pid, waits up to grace for the process to
// exit cleanly, then escalates to SIGKILL if it's still alive. Returns
// nil if the process exited (either response), or an error if neither
// signal could be delivered.
//
// Safety: callers are responsible for verifying the PID belongs to a
// known agent (e.g. via proc.Running) BEFORE calling — PID reuse is a
// real risk on long-running systems and Terminate does no re-check.
//
// Windows note: os.Process.Signal(syscall.SIGTERM) is not supported on
// Windows (TerminateProcess has no graceful path). The SIGTERM step
// errors fast and we fall through to Kill() directly.
func Terminate(pid int, grace time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	// SIGTERM the process. On Unix this is the graceful shutdown
	// signal; the agent gets a chance to flush state. Errors from
	// Signal(SIGTERM) on Windows are expected and not fatal — we'll
	// escalate to Kill() below.
	termErr := p.Signal(syscall.SIGTERM)
	if termErr == nil {
		// Poll until the process is gone or the grace period elapses.
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			if !isProcessAliveProc(p) {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
	} else if errors.Is(termErr, os.ErrProcessDone) {
		return nil
	}

	// Still alive (or SIGTERM unsupported) — hard-kill.
	if err := p.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return nil
}

// isProcessAliveProc checks aliveness on an already-resolved Process
// handle via signal-0. Mirrors mcp_autostart.go's pattern; kept here so
// proc has no dependency on cmd/bosun.
func isProcessAliveProc(p *os.Process) bool {
	return p.Signal(syscall.Signal(0)) == nil
}
