// Package lockfile provides cross-process serialization via POSIX
// advisory file locks (flock).
//
// Bosun has four conceptually distinct lock files — .bosun/init.lock,
// .bosun/state/.lock, .bosun/claims/.lock, .bosun/mcp.lock — each
// serializing a different read-modify-write cycle (init.state, session
// markers, claim files, MCP daemon spawn). All route through WithLock
// or WithLockResult here.
//
// Acquisition has a bounded timeout (DefaultTimeout, 30s). A hung
// process holding a lock no longer pins every subsequent bosun command
// indefinitely; instead the second caller gets a LockTimeoutError with
// the holder's PID and how long they've held it (best-effort — see
// readLockHolder). Both Unix (flock) and Windows (LockFileEx) honor
// the timeout via a non-blocking poll loop.
package lockfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	// DefaultTimeout bounds how long WithLock/WithLockResult will wait
	// for the lock before returning a LockTimeoutError. var (not const)
	// so tests can collapse it without burning real wall-clock time;
	// callers should reset via t.Cleanup.
	DefaultTimeout = 30 * time.Second

	// pollInterval is how often the non-blocking acquire loop retries.
	// 50ms keeps lock-handoff latency under 100ms in the common case
	// while not spinning hot on a contended lock.
	pollInterval = 50 * time.Millisecond
)

// ErrLockTimeout is the sentinel a LockTimeoutError unwraps to.
// Callers can use errors.Is(err, lockfile.ErrLockTimeout) to detect
// lock-contention failures without depending on the concrete type.
var ErrLockTimeout = errors.New("bosun: lock acquisition timed out")

// LockTimeoutError is returned when DefaultTimeout elapses without
// the caller having acquired the lock. The Holder fields are
// best-effort: if the holder didn't manage to write its PID into the
// file (e.g., a writer that died mid-acquire), Holder fields are
// zero.
type LockTimeoutError struct {
	Path      string
	Timeout   time.Duration
	HolderPID int           // 0 if unknown
	HeldFor   time.Duration // 0 if unknown
}

func (e *LockTimeoutError) Error() string {
	if e.HolderPID > 0 && e.HeldFor > 0 {
		return fmt.Sprintf("bosun: lock %s held by PID %d for %s; timed out after %s", e.Path, e.HolderPID, e.HeldFor.Round(time.Second), e.Timeout)
	}
	if e.HolderPID > 0 {
		return fmt.Sprintf("bosun: lock %s held by PID %d; timed out after %s", e.Path, e.HolderPID, e.Timeout)
	}
	return fmt.Sprintf("bosun: lock %s contended; timed out after %s", e.Path, e.Timeout)
}

func (e *LockTimeoutError) Unwrap() error { return ErrLockTimeout }

// WithLock acquires an exclusive lock at lockPath for the duration of
// fn and returns whatever fn returns. Creates the parent directory of
// lockPath if missing (0o750). Releases the lock and closes the file
// handle before returning, regardless of fn's error.
//
// If the lock isn't acquired within DefaultTimeout, returns a
// *LockTimeoutError (which unwraps to ErrLockTimeout for sentinel-
// shaped checks).
func WithLock(lockPath string, fn func() error) error {
	_, err := WithLockResult(lockPath, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

// WithLockResult is the result-returning variant of WithLock. Callers
// like mcp_autostart's daemon-spawn dance need to return a payload
// from the locked section. Implementation lives in lockfile_unix.go
// / lockfile_windows.go.

// writeLockHolder stamps the current process's PID and the
// acquisition timestamp into the lock file from inside the locked
// section. The waiting caller reads this on timeout to surface "lock
// held by PID N for X seconds" without needing its own lock.
// Best-effort — a write failure here doesn't fail the acquire (the
// lock semantics are still correct; only the diagnostic surface
// degrades).
func writeLockHolder(f *os.File) {
	line := fmt.Sprintf("pid=%d ts=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	_ = f.Truncate(0)
	_, _ = f.Seek(0, io.SeekStart)
	_, _ = f.WriteString(line)
}

// readLockHolder reads the holder's PID and how long they've held
// the lock out of the lock file. Best-effort: any parse failure
// returns zero values (the timeout error then falls back to the
// "contended" message shape).
func readLockHolder(lockPath string) (pid int, heldFor time.Duration) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0, 0
	}
	line := strings.TrimSpace(string(data))
	// Shape: pid=12345 ts=2026-05-21T15:23:45.678Z
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	pidStr := strings.TrimPrefix(parts[0], "pid=")
	tsStr := strings.TrimPrefix(parts[1], "ts=")
	if pidStr == parts[0] || tsStr == parts[1] {
		return 0, 0
	}
	parsedPID, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, 0
	}
	ts, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return parsedPID, 0
	}
	return parsedPID, time.Since(ts)
}
