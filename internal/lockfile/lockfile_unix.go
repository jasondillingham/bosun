//go:build !windows

package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// WithLockResult acquires an exclusive POSIX flock on lockPath and
// runs fn while holding it. See package docs for context on the four
// call sites this consolidated.
func WithLockResult[T any](lockPath string, fn func() (T, error)) (T, error) {
	var zero T
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return zero, fmt.Errorf("mkdir lock parent: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return zero, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return zero, fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
