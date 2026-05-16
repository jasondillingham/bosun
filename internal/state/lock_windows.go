//go:build windows

package state

// withStateLock is a no-op stub on Windows. The cross-process serialization
// the POSIX path uses requires flock; the LockFileEx equivalent is a TODO
// (same status as cmd/bosun/mcp_autostart_lock_windows.go and
// internal/claims/lock_windows.go). On Windows the failure mode is:
// two concurrent `bosun done` invocations (one MarkDone, one MarkStuck)
// may interleave write-then-remove and leave the session with no marker —
// same risk shape as bosun's other Windows-deferred primitives.
// Documented here so the gap is visible if it ever bites.
func withStateLock(_ string, fn func() error) error {
	return fn()
}
