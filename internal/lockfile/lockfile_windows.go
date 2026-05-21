//go:build windows

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// WithLockResult acquires an exclusive Windows file lock on lockPath
// via LockFileEx and runs fn while holding it. Uses a non-blocking
// LOCKFILE_FAIL_IMMEDIATELY poll loop bounded by DefaultTimeout —
// matching the Unix path's flock(LOCK_EX|LOCK_NB) shape — so a hung
// holder doesn't pin every subsequent bosun command indefinitely.
// On timeout returns *LockTimeoutError with the holder's PID +
// how long they've held (best-effort).
//
// 2026-05 follow-up grind #94: replaces the no-op stub that used to
// let two concurrent writers race the same .lock file on Windows.
// 2026-05-21 (v0.12 M5): added DefaultTimeout via poll loop.
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

	handle := windows.Handle(f.Fd())
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY = non-
	// blocking exclusive lock. The kernel returns ERROR_LOCK_VIOLATION
	// when the range is already locked; the poll loop turns that into
	// a bounded wait.
	overlapped := &windows.Overlapped{}
	const (
		lockExclusive       uint32 = 2
		lockFailImmediately uint32 = 1
	)
	const flags = lockExclusive | lockFailImmediately

	timeout := DefaultTimeout
	deadline := time.Now().Add(timeout)
	for {
		err := windows.LockFileEx(handle, flags, 0, 0xFFFFFFFF, 0xFFFFFFFF, overlapped)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
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
	defer func() {
		// UnlockFileEx needs the same range as LockFileEx. The kernel
		// releases on handle close anyway, so a silent unlock-
		// failure plus the deferred Close remains correct.
		_ = windows.UnlockFileEx(handle, 0, 0xFFFFFFFF, 0xFFFFFFFF, overlapped)
	}()

	writeLockHolder(f)
	return fn()
}
