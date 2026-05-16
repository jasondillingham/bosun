//go:build windows

package claims

// withStoreLock is a no-op stub on Windows. The cross-process serialization
// the POSIX path uses requires flock; the LockFileEx equivalent is a TODO
// (same status as cmd/bosun/mcp_autostart_lock_windows.go). On Windows the
// failure mode is "two concurrent bosun claim invocations may lose one
// other's updates" — same risk shape as bosun's other Windows-deferred
// primitives. Documented here so the gap is visible if it ever bites.
func withStoreLock(_ string, fn func() error) error {
	return fn()
}
