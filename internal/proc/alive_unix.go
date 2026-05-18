//go:build !windows

package proc

import (
	"errors"
	"syscall"
)

// IsAlive reports whether pid names a process the current user can signal.
// On Unix the canonical "is this PID alive?" test is `kill(pid, 0)` — a
// no-op signal that returns ESRCH if the process is gone and EPERM if it
// exists but belongs to another user (still "alive" for our purposes;
// what matters is that the explicit registration's referent still exists).
//
// pid <= 0 is rejected here rather than passed through, because kill(0, …)
// targets the whole process group on POSIX and kill(-1, …) is
// "everything you have permission to signal" — both surprising answers
// to "is this PID alive?". Callers seeing a recorded PID of 0 should
// have already routed to the proc-scan fallback.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but we can't signal it (different
	// uid). For the liveness gate's purposes that still counts as alive.
	return errors.Is(err, syscall.EPERM)
}
