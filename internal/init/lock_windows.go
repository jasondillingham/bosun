//go:build windows

package initstate

// withInitLock is a no-op stub on Windows. The cross-process serialization
// the POSIX path uses requires flock; the LockFileEx equivalent is a TODO
// (same status as cmd/bosun/mcp_autostart_lock_windows.go and
// internal/state/lock_windows.go). On Windows the failure mode is: two
// concurrent `bosun init` invocations could tear the state file; in
// practice a second init aborts on the Exists() check before reaching
// the writer, so the window is small. Documented here so the gap is
// visible if it ever bites.
func withInitLock(_ string, fn func() error) error {
	return fn()
}
