//go:build !windows

package initstate

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// withInitLock holds an exclusive POSIX flock on `.bosun/init.lock` for
// the duration of fn so two cross-process writers (e.g. an operator
// running `bosun init --resume` while the MCP daemon happens to be
// inspecting the same file) cannot tear each other's writes.
//
// Mirrors internal/state.withStateLock — kept here rather than imported
// from internal/state because the lock files (and conceptual scopes)
// are distinct, and folding them into one package would introduce a
// dependency cycle through cmd_init.
func withInitLock(repoRoot string, fn func() error) error {
	dir := filepath.Join(repoRoot, dirRelative)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	lockPath := filepath.Join(repoRoot, lockRelative)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open init lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire init lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
