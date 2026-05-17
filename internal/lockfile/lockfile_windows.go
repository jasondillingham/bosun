//go:build windows

package lockfile

// WithLockResult is a no-op pass-through on Windows. POSIX flock has
// no direct Windows equivalent; LockFileEx is the right primitive but
// not yet wired up (a known gap shared with bosun's other Windows-
// deferred locking spots). The practical failure mode is "two
// concurrent writers may interleave"; documented here so the gap is
// visible if it ever bites.
func WithLockResult[T any](_ string, fn func() (T, error)) (T, error) {
	return fn()
}
