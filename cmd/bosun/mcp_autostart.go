package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jasondillingham/bosun/internal/lockfile"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
)

// mcpPidfileRelative is where bosun records the running MCP daemon's pid
// and bound socket path. Lives under .bosun/ so the existing gitignore
// pattern picks it up.
const mcpPidfileRelative = ".bosun/mcp.pid"

// mcpServerInfo describes a running (or just-started) MCP daemon.
type mcpServerInfo struct {
	socketPath string
	pid        int
	spawned    bool // true when ensureMcp had to start a new daemon
}

// ensureMcp guarantees a running bosun MCP server for repoRoot and
// returns the socket path agent sessions should set BOSUN_MCP_SOCK to.
// Reuse order: env var (only if it belongs to this repo) → pidfile →
// fresh spawn.
//
// The pidfile-check + spawn sequence is wrapped in a process-wide flock
// on .bosun/mcp.lock so two near-simultaneous `bosun init --launch`
// invocations don't both spawn a daemon (which would race on socket
// bind and clobber each other's pidfile entries). The env-var fast
// path runs outside the lock — if the operator pre-exported a socket,
// no on-disk coordination is needed.
func ensureMcp(repoRoot string) (mcpServerInfo, error) {
	// Operator may have an inherited BOSUN_MCP_SOCK from a parent process
	// that's running bosun against a *different* repo. Reusing that
	// socket sends our agents to the wrong daemon. Validate the socket
	// belongs to this repo before honoring the env var: it's the default
	// socket path for repoRoot, or this repo's pidfile already records
	// that exact socket path.
	if sock := os.Getenv(bosunmcp.SocketEnv); sock != "" {
		if inheritedSocketBelongsToRepo(sock, repoRoot) && isSocketAlive(sock) {
			return mcpServerInfo{socketPath: sock}, nil
		}
	}
	return lockfile.WithLockResult(filepath.Join(repoRoot, ".bosun", "mcp.lock"), func() (mcpServerInfo, error) {
		// Re-check inside the lock: another caller may have spawned a
		// daemon while we were waiting for the flock.
		if info, ok := readMcpPidfile(repoRoot); ok && isProcessAlive(info.pid) && isSocketAlive(info.socketPath) {
			return mcpServerInfo{socketPath: info.socketPath, pid: info.pid}, nil
		}
		return spawnMcpDaemon(repoRoot)
	})
}

// inheritedSocketBelongsToRepo reports whether an inherited BOSUN_MCP_SOCK
// value plausibly belongs to repoRoot's daemon. Accepts the value if it
// equals the default socket path for this repo (covering both the
// normal `<repo>/.bosun/mcp.sock` and the `/tmp/bosun-<hash>.sock`
// fallback) or if repoRoot's pidfile records that exact path.
func inheritedSocketBelongsToRepo(sock, repoRoot string) bool {
	if sock == bosunmcp.DefaultSocketPath(repoRoot) {
		return true
	}
	if info, ok := readMcpPidfile(repoRoot); ok && info.socketPath == sock {
		return true
	}
	return false
}

// spawnMcpDaemon launches `bosun mcp --socket <default>` as a detached
// background process and waits for the socket to become connectable.
func spawnMcpDaemon(repoRoot string) (mcpServerInfo, error) {
	self, err := os.Executable()
	if err != nil {
		return mcpServerInfo{}, fmt.Errorf("locate bosun binary: %w", err)
	}
	socketPath := bosunmcp.DefaultSocketPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return mcpServerInfo{}, fmt.Errorf("mkdir socket parent: %w", err)
	}

	cmd := exec.Command(self, "mcp", "--socket", socketPath)
	cmd.Dir = repoRoot
	// Detach stdio — the daemon needs to survive bosun init's exit, so
	// keeping the parent's tty hooked up would surface noise (and a
	// SIGHUP would kill it).
	if devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	}
	applyMcpDaemonAttrs(cmd)

	if err := cmd.Start(); err != nil {
		return mcpServerInfo{}, fmt.Errorf("spawn bosun mcp: %w", err)
	}
	// Reap the child if it dies while bosun init is still running.
	// Once bosun init exits, the OS reparents the daemon to init/launchd
	// which handles reaping from there.
	go func() { _ = cmd.Wait() }()

	// Wait for the socket to come up. The daemon binds before it prints
	// its banner, so dial-then-close is a reliable readiness probe.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if isSocketAlive(socketPath) {
			return mcpServerInfo{socketPath: socketPath, pid: cmd.Process.Pid, spawned: true}, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return mcpServerInfo{}, fmt.Errorf("MCP daemon spawned but socket %s did not appear within 3s", socketPath)
}

// readMcpPidfile parses the pidfile format `<pid>\n<socket-path>\n`. A
// missing or malformed file returns ok=false (silently — pidfiles are
// best-effort discovery, not a hard contract).
func readMcpPidfile(repoRoot string) (mcpServerInfo, bool) {
	data, err := os.ReadFile(filepath.Join(repoRoot, mcpPidfileRelative))
	if err != nil {
		return mcpServerInfo{}, false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		return mcpServerInfo{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return mcpServerInfo{}, false
	}
	socketPath := strings.TrimSpace(lines[1])
	if socketPath == "" {
		return mcpServerInfo{}, false
	}
	return mcpServerInfo{pid: pid, socketPath: socketPath}, true
}

// writeMcpPidfile records the daemon's pid + socket path so other bosun
// invocations can find it. Called from `bosun mcp` (the daemon writes
// its own pidfile on startup).
//
// Writes atomically via temp + rename so a concurrent readMcpPidfile —
// e.g. a second `bosun init --launch` running outside the spawn flock,
// or a `bosun status` reading the daemon's coordinates — never observes
// a half-written pidfile and treats it as "no daemon, spawn one." A
// non-atomic write (the prior os.WriteFile path) racing a reader was
// not catastrophic (the reader fell through to a fresh spawn attempt
// which would have raced on socket bind), but it was avoidable.
func writeMcpPidfile(repoRoot string, pid int, socketPath string) error {
	final := filepath.Join(repoRoot, mcpPidfileRelative)
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(final)+".tmp-*")
	if err != nil {
		return fmt.Errorf("temp pidfile: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write([]byte(fmt.Sprintf("%d\n%s\n", pid, socketPath))); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp pidfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp pidfile: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		cleanup()
		return fmt.Errorf("rename pidfile: %w", err)
	}
	return nil
}

// removeMcpPidfile deletes the pidfile, used as a defer in `bosun mcp`.
// Errors are swallowed — leaving a stale pidfile behind is recoverable
// (the next reader will detect the dead process and start a new one).
func removeMcpPidfile(repoRoot string) {
	_ = os.Remove(filepath.Join(repoRoot, mcpPidfileRelative))
}

// isProcessAlive reports whether pid names a live process. Uses
// signal-0, which checks for existence without delivering anything.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// isSocketAlive probes whether a Unix-domain socket at path accepts
// connections. A short timeout keeps the check snappy when the socket
// is stale (file exists but nothing listening).
func isSocketAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
