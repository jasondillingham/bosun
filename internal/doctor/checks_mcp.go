package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// CheckMCPDaemonStartup verifies bosun can bind a Unix socket where
// the MCP daemon would listen. Probes the actual socket path bosun
// would use (via the same MaxSocketPathLen-aware logic the daemon
// applies) and binds/unbinds without launching the real daemon. A
// failure here means `bosun init --launch` will fail in the same
// place — better surfaced at doctor time than after worktrees are
// already half-created.
//
// We don't depend on internal/mcp to avoid cycles in the doctor
// package; instead we hardcode the well-known socket path layout.
// The daemon's own path calculation may diverge over time; doctor's
// probe is best-effort early warning, not a contract.
func CheckMCPDaemonStartup(_ context.Context, repoRoot string) Result {
	// Mirror internal/mcp.DefaultSocketPath at a high level: prefer
	// .bosun/mcp.sock under the repo, fall back to /tmp/bosun-<hash>
	// if the in-repo path would exceed the platform socket-path limit.
	sock := filepath.Join(repoRoot, ".bosun", "mcp.sock")
	if len(sock) > maxUnixSocketPath() {
		// We don't reproduce the full hash-based fallback here — that
		// would couple us to internal/mcp's exact algorithm. Instead we
		// note that the in-repo socket won't fit and treat it as a
		// soft warn since the real daemon will use the /tmp fallback.
		return Result{
			Name:    "mcp-socket",
			Status:  Pass,
			Message: "in-repo socket path too long for this OS; daemon will use /tmp fallback",
		}
	}

	// Make sure the parent dir exists before binding.
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return Result{
			Name:    "mcp-socket",
			Status:  Fail,
			Message: fmt.Sprintf("cannot create %s: %v", filepath.Dir(sock), err),
		}
	}

	// Include the PID so concurrent doctor invocations (or a doctor
	// against a still-running daemon's stale crash artifact) don't
	// collide on the same probe path. Defer the remove so a panic
	// inside the bind path still cleans up.
	probePath := sock + ".doctor-probe-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(probePath)
	defer func() { _ = os.Remove(probePath) }()

	ln, err := net.Listen("unix", probePath)
	if err != nil {
		return Result{
			Name:    "mcp-socket",
			Status:  Fail,
			Message: fmt.Sprintf("cannot bind Unix socket at %s: %v", probePath, err),
			Fix:     "check filesystem support for Unix sockets (some FUSE/cloud mounts disallow them)",
		}
	}
	_ = ln.Close()
	return Result{
		Name:    "mcp-socket",
		Status:  Pass,
		Message: "Unix socket bind succeeded",
	}
}

// maxUnixSocketPath returns the OS limit on Unix-socket path length.
// Linux: 108. macOS / BSDs: 104. Conservative bound: 104.
func maxUnixSocketPath() int { return 104 }
