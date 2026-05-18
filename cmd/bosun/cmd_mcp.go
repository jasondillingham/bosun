package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/spf13/cobra"
)

func newMcpCmd() *cobra.Command {
	var socketPath string

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run bosun's MCP server (foreground; agents call bosun_claim / bosun_done / bosun_spawn / etc.)",
		Long: `Run bosun's Model Context Protocol server. Agent sessions inside an
MCP-capable client (Claude Code, ...) call bosun tools — bosun_claim,
bosun_done, bosun_check, bosun_spawn, bosun_check_tree — directly instead
of shelling out to the bosun CLI.

The server binds to a Unix socket — by default <repo>/.bosun/mcp.sock — and
keeps running in the foreground until interrupted. ` + "`bosun init --launch`" + ` and
` + "`bosun launch`" + ` autostart this server and inject BOSUN_MCP_SOCK into the
agent's environment, so you rarely need to run ` + "`bosun mcp`" + ` directly.

Filesystem-based coordination remains the canonical source of truth;
sessions that don't connect to MCP keep working as before.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMcp(cmd, socketPath)
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (default: <repo>/.bosun/mcp.sock)")
	cmd.GroupID = "wiring"
	return cmd
}

func runMcp(_ *cobra.Command, socketPath string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	if socketPath == "" {
		// DefaultSocketPath handles the 100-byte limit by falling back to
		// /tmp/bosun-<hash>.sock for deeply-nested repos.
		socketPath = bosunmcp.DefaultSocketPath(rc.repoRoot)
	} else {
		// Resolve to absolute up-front so the printed log line is portable.
		abs, err := filepath.Abs(socketPath)
		if err != nil {
			return userErr("resolve socket path: %v", err)
		}
		socketPath = abs
		if len(socketPath) > bosunmcp.MaxSocketPathLen {
			return userErr("socket path is %d bytes (Unix-domain max ≈%d): %s\n  workaround: pass --socket /tmp/bosun-<repo>.sock or shorten the repo path", len(socketPath), bosunmcp.MaxSocketPathLen, socketPath)
		}
	}

	// Ensure the parent directory exists (default lives under .bosun/, which
	// might not exist yet on a freshly-cloned repo).
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return internalErr("mkdir socket parent", err)
	}

	// Unix-domain socket paths max out around 104 bytes on Darwin and 108
	// on Linux. A deeply-nested repo can blow past that, producing an
	// unhelpful "bind: invalid argument" deep in the stack. Bail early with
	// a guide toward a workaround.
	if len(socketPath) > 100 {
		return userErr("socket path is %d bytes (Unix-domain max ≈100): %s\n  workaround: pass --socket /tmp/bosun-<repo>.sock or shorten the repo path", len(socketPath), socketPath)
	}

	// Persist bosun_announce events under .bosun/events.log so the CLI-side
	// `bosun status` (a separate process) can tail recent records — the
	// in-memory ring buffer alone can't bridge process boundaries.
	bosunmcp.SetEventsLog(filepath.Join(rc.repoRoot, bosunmcp.EventLogRelative))

	srv := bosunmcp.NewServer(rc.claims, rc.state, rc.git)
	// v0.9: wire the spawn-tree + config so the bosun_spawn tool can
	// run. Off by default (config.AgentSpawn.Enabled gates the tool
	// itself); this just makes the dependencies available.
	srv.WithSpawnSupport(rc.cfg, spawntree.NewStore(rc.repoRoot))
	if err := srv.Listen(socketPath); err != nil {
		return userErr("bind socket: %v", err)
	}
	// Listener.Close() does not unlink the socket file on POSIX, so on
	// SIGINT/SIGTERM the file would otherwise linger and confuse the next
	// `bosun init --launch` (it'd see a regular file at the expected path
	// and refuse to overwrite via removeIfSocket). Defer an explicit
	// remove after the listener stops.
	defer func() {
		_ = srv.Stop()
		_ = os.Remove(socketPath)
	}()

	// Drop a pidfile so subsequent `bosun init --launch` runs can detect
	// us and reuse the socket instead of spawning a duplicate daemon.
	if err := writeMcpPidfile(rc.repoRoot, os.Getpid(), socketPath); err != nil {
		fmt.Fprintf(os.Stderr, "bosun mcp: warning: write pidfile: %v\n", err)
	}
	defer removeMcpPidfile(rc.repoRoot)

	fmt.Fprintf(os.Stdout, "bosun mcp: listening on %s\n", socketPath)
	fmt.Fprintf(os.Stdout, "bosun mcp: agents reach the server via %s=%s\n", bosunmcp.SocketEnv, socketPath)
	fmt.Fprintln(os.Stdout, "bosun mcp: ^C to stop")

	// SIGINT / SIGTERM → graceful shutdown. The server's Serve loop watches
	// its own context to break out of Accept().
	ctx, cancel := signal.NotifyContext(rc.ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Serve(ctx); err != nil {
		return internalErr("mcp serve", err)
	}
	fmt.Fprintln(os.Stdout, "\nbosun mcp: stopped")
	return nil
}
