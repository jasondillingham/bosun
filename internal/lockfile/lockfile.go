// Package lockfile provides cross-process serialization via POSIX
// advisory file locks (flock).
//
// Bosun has four conceptually distinct lock files — .bosun/init.lock,
// .bosun/state/.lock, .bosun/claims/.lock, .bosun/mcp.lock — each
// serializing a different read-modify-write cycle (init.state, session
// markers, claim files, MCP daemon spawn). Before this package they
// each had their own withXLock helper that opened the file, called
// syscall.Flock, ran a callback, and released. Same eight lines four
// times.
//
// All four now route through WithLock or WithLockResult here.
//
// On Windows, both helpers are no-op pass-throughs; LockFileEx is a
// TODO mirrored in the four call-site comments. The practical failure
// mode on Windows is "two concurrent writers may interleave" — same
// risk shape as bosun's other Windows-deferred primitives.
package lockfile

// WithLock acquires an exclusive lock at lockPath for the duration of
// fn and returns whatever fn returns. Creates the parent directory of
// lockPath if missing (0o755). Releases the lock and closes the file
// handle before returning, regardless of fn's error.
//
// Lock acquisition is blocking — callers that need a timeout should
// run WithLock in a goroutine and select on a deadline.
func WithLock(lockPath string, fn func() error) error {
	_, err := WithLockResult(lockPath, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

// WithLockResult is the result-returning variant of WithLock. Callers
// like mcp_autostart's daemon-spawn dance need to return a payload
// from the locked section. Go generics avoid the alternative of
// passing in a pointer-typed output parameter.
//
// Implementation lives in lockfile_unix.go / lockfile_windows.go.

// (WithLockResult is defined per-platform below.)
