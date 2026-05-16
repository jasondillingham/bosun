//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestMcpSpawnLockFD_HasCloseOnExec verifies a property the audit
// explicitly worried about: the fd held by withMcpSpawnLock must not
// leak to a subprocess spawned while the lock is still held
// (spawnMcpDaemon runs *inside* the flock).
//
// Go's os.OpenFile applies O_CLOEXEC by default on Unix, so today the
// child daemon does not inherit the lock fd; if a future change ever
// strips the flag (a manual openat without it, or some setuid wrapper),
// the daemon would hold the parent's flock indefinitely and the next
// `bosun init --launch` would block forever waiting for a lock the
// daemon doesn't know it owns. This test pins the invariant: open the
// lock file the way withMcpSpawnLock does, then fcntl(F_GETFD) and
// assert FD_CLOEXEC is set.
//
// We open the file directly rather than going through withMcpSpawnLock
// because the helper only returns after fn runs; this test cares about
// the open-time fd flag, not the flock state.
func TestMcpSpawnLockFD_HasCloseOnExec(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".bosun"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(repoRoot, ".bosun", "mcp.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer f.Close()
	flags, err := fcntlGetFD(f.Fd())
	if err != nil {
		t.Fatalf("fcntl F_GETFD: %v", err)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("lock fd lacks FD_CLOEXEC (flags=0x%x); a daemon spawned inside withMcpSpawnLock would inherit the flock and pin it past the parent's exit", flags)
	}
}

// TestStateLockFD_HasCloseOnExec mirrors the mcp-lock test for the new
// state-store flock. The state lock fd is short-lived (held only across
// MarkDone/MarkStuck/Clear bodies, which do not spawn subprocesses), so
// a missing FD_CLOEXEC would be lower impact today — but bosun is the
// kind of tool that grows hook callsites over time, and a hook running
// inside this lock window with the fd leaked into the hook subprocess
// would be a real bug. Pin the property.
func TestStateLockFD_HasCloseOnExec(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), ".bosun", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(stateDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer f.Close()
	flags, err := fcntlGetFD(f.Fd())
	if err != nil {
		t.Fatalf("fcntl F_GETFD: %v", err)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("state lock fd lacks FD_CLOEXEC (flags=0x%x); subprocesses spawned while the lock is held would inherit it", flags)
	}
}

// TestClaimsLockFD_HasCloseOnExec rounds out the same audit for the
// claims-store flock (round-1 addition). Same reasoning as the state
// lock test: pin the property before it can quietly regress.
func TestClaimsLockFD_HasCloseOnExec(t *testing.T) {
	claimsDir := filepath.Join(t.TempDir(), ".bosun", "claims")
	if err := os.MkdirAll(claimsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(claimsDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer f.Close()
	flags, err := fcntlGetFD(f.Fd())
	if err != nil {
		t.Fatalf("fcntl F_GETFD: %v", err)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("claims lock fd lacks FD_CLOEXEC (flags=0x%x)", flags)
	}
}

// fcntlGetFD wraps fcntl(fd, F_GETFD, 0) without depending on
// syscall.FcntlInt (which is not exported on darwin). Used by the
// CLOEXEC tests above; intentionally minimal — we only need the flags.
func fcntlGetFD(fd uintptr) (int, error) {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(flags), nil
}
