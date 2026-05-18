//go:build windows

package proc

import (
	"golang.org/x/sys/windows"
)

// IsAlive reports whether pid names a process the current user can open.
// On Windows the equivalent of `kill(pid, 0)` is OpenProcess with
// PROCESS_QUERY_LIMITED_INFORMATION — if the OS hands back a valid
// handle, the PID is live. ERROR_ACCESS_DENIED means the process exists
// but belongs to another security context; for the liveness gate's
// purposes that still counts as alive.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err == nil {
		_ = windows.CloseHandle(h)
		return true
	}
	if err == windows.ERROR_ACCESS_DENIED {
		return true
	}
	return false
}
