//go:build windows

package lockfile

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// WithLockResult acquires an exclusive Windows file lock on lockPath
// via LockFileEx and runs fn while holding it. Mirrors the POSIX
// flock semantics the Unix path uses — blocking acquire, exclusive
// mode, automatic release on close. Locks the entire file (range 0
// to maxUint64), which matches what flock(LOCK_EX) effectively
// provides on a 0-byte lock file.
//
// 2026-05 follow-up grind #94: replaces the no-op stub that used to
// let two concurrent writers race the same .lock file on Windows.
// Same four lock-files the Unix path covers (claims/.lock,
// state/.lock, init.lock, mcp.lock) now serialize cross-process on
// both OSes.
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
	// Lock the full possible range. LockFileEx takes a (low, high)
	// pair for the 64-bit length and a windows.Overlapped that
	// carries the start offset (both fields zero here = lock from
	// byte 0). LOCKFILE_EXCLUSIVE_LOCK = 2 in the Windows headers;
	// omitting LOCKFILE_FAIL_IMMEDIATELY (1) makes the call block
	// until the lock is available, matching flock's blocking shape.
	overlapped := &windows.Overlapped{}
	const lockExclusive uint32 = 2
	if err := windows.LockFileEx(handle, lockExclusive, 0, 0xFFFFFFFF, 0xFFFFFFFF, overlapped); err != nil {
		return zero, fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	defer func() {
		// UnlockFileEx needs the same range as LockFileEx. Errors
		// here are non-recoverable in any useful way and would
		// shadow fn's return — log via panic? No: the kernel
		// releases on handle close anyway, so a silent unlock-
		// failure plus the deferred Close is correct.
		_ = windows.UnlockFileEx(handle, 0, 0xFFFFFFFF, 0xFFFFFFFF, overlapped)
	}()
	return fn()
}
