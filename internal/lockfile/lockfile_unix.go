//go:build !windows

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// WithLockResult acquires an exclusive POSIX flock on lockPath via a
// non-blocking poll loop bounded by DefaultTimeout. On timeout returns
// a *LockTimeoutError with the holder's PID + how long they've held
// (best-effort). See package docs.
func WithLockResult[T any](lockPath string, fn func() (T, error)) (T, error) {
	var zero T
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return zero, fmt.Errorf("mkdir lock parent: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return zero, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()

	// LOCK_EX | LOCK_NB returns EWOULDBLOCK instead of blocking when
	// the lock is held; the poll loop turns that into a bounded wait.
	timeout := DefaultTimeout
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return zero, fmt.Errorf("acquire lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			pid, heldFor := readLockHolder(lockPath)
			return zero, &LockTimeoutError{
				Path:      lockPath,
				Timeout:   timeout,
				HolderPID: pid,
				HeldFor:   heldFor,
			}
		}
		time.Sleep(pollInterval)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	writeLockHolder(f)
	return fn()
}
