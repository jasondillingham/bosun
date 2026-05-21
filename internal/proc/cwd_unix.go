//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Cwd returns the working directory of pid as the OS reports it.
//
// Linux: reads /proc/<pid>/cwd (a symlink the kernel maintains).
// macOS: /proc doesn't exist by default. Returns ErrCwdUnsupported.
//
// pid <= 0 returns an error rather than a useful path — kill(0)
// semantics target the process group and we don't want callers
// silently feeding negative PIDs into the lookup.
func Cwd(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("proc.Cwd: pid must be positive, got %d", pid)
	}
	// macOS has no /proc by default — bail before attempting the
	// per-pid lookup so callers see ErrCwdUnsupported, not a
	// "/proc/1234/cwd: no such file or directory" that's
	// indistinguishable from "the PID is gone."
	if _, err := os.Stat("/proc"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrCwdUnsupported
		}
		return "", fmt.Errorf("proc.Cwd: stat /proc: %w", err)
	}
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", fmt.Errorf("proc.Cwd: readlink: %w", err)
	}
	return cwd, nil
}
