//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// withMcpSpawnLock holds an exclusive flock on .bosun/mcp.lock for the
// duration of fn so two near-simultaneous `bosun init --launch` (or any
// other ensureMcp caller) cannot both spawn a daemon. Without the lock
// they each see no live pidfile, both fork a daemon, and the second
// daemon's Listen() unlinks the first's socket — leaving an orphaned
// listener and the pidfile pointing at whichever lost the spawn race.
//
// The lock is process-wide (advisory POSIX flock); held only across the
// pidfile check + spawn + readiness wait, then released. Subsequent
// callers that find a healthy pidfile take the lock for a microsecond
// and bypass the spawn entirely.
func withMcpSpawnLock(repoRoot string, fn func() (mcpServerInfo, error)) (mcpServerInfo, error) {
	lockPath := filepath.Join(repoRoot, ".bosun", "mcp.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return mcpServerInfo{}, fmt.Errorf("mkdir lock parent: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return mcpServerInfo{}, fmt.Errorf("open mcp lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return mcpServerInfo{}, fmt.Errorf("acquire mcp lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
