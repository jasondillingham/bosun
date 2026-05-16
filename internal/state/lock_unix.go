//go:build !windows

package state

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// withStateLock holds an exclusive POSIX flock on .bosun/state/.lock for
// the duration of fn so two separate `bosun done` (or any cross-process
// state mutator — e.g. CLI vs. the MCP daemon's bosun_done tool) cannot
// interleave their write-then-remove cycles and wipe both markers.
//
// Without this lock, MarkDone and MarkStuck race shape is:
//
//	A.write(.done)      B.write(.stuck)
//	A.remove(.stuck)    ← wipes B's marker
//	B.remove(.done)     ← wipes A's marker
//	final: no markers, session reads as WORKING.
//
// The Store's sync.Mutex covers in-process callers; this flock covers the
// cross-process surface that mutex cannot see. Held only across the
// mutate path — Read stays lock-free since each marker is a single
// os.WriteFile and Read only checks existence + reads tiny bodies.
//
// Do not call into other bosun packages that take their own flocks
// from inside fn. There is no nested-flock acquisition anywhere in
// bosun today; preserving that property keeps the lock order trivial
// and the deadlock surface empty.
func withStateLock(stateDir string, fn func() error) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	lockPath := filepath.Join(stateDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire state lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
