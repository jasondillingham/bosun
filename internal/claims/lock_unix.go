//go:build !windows

package claims

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// withStoreLock holds an exclusive POSIX flock on .bosun/claims/.lock for
// the duration of fn so two separate `bosun claim` (or `bosun remove`,
// or any cross-process claims mutator) invocations cannot interleave
// their read-modify-write cycles and silently drop each other's updates.
//
// The Store's sync.Mutex covers in-process callers; this flock covers the
// rest. Held only across the mutate path — read-only methods stay
// lock-free because the atomic temp+rename writer guarantees no torn JSON
// for readers.
func withStoreLock(claimsDir string, fn func() error) error {
	if err := os.MkdirAll(claimsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir claims dir: %w", err)
	}
	lockPath := filepath.Join(claimsDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open claims lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire claims lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
