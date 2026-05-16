package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/spf13/cobra"
)

// defaultSocketRelative is where bosun keeps its MCP socket when --socket
// isn't passed explicitly. Lives under .bosun/ so it's auto-gitignored.
const defaultSocketRelative = ".bosun/mcp.sock"

func newMcpCmd() *cobra.Command {
	var socketPath string

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run bosun's MCP server (foreground; experimental, v0.2.0-alpha)",
		Long: `Run bosun's Model Context Protocol server. Agent sessions inside an
MCP-capable client can call bosun tools (bosun_check today; bosun_claim /
bosun_done / etc. in round 1) directly instead of shelling out to the bosun CLI.

The server binds to a Unix socket — by default <repo>/.bosun/mcp.sock — and
keeps running in the foreground until interrupted. Configure the agent to
point at the socket via the BOSUN_MCP_SOCK environment variable.

Status: v0.2.0-alpha. Filesystem-based coordination remains the canonical
source of truth; sessions that don't connect to MCP keep working as before.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMcp(cmd, socketPath)
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (default: <repo>/.bosun/mcp.sock)")
	return cmd
}

func runMcp(_ *cobra.Command, socketPath string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	if socketPath == "" {
		socketPath = filepath.Join(rc.repoRoot, defaultSocketRelative)
	} else {
		// Resolve to absolute up-front so the printed log line is portable.
		abs, err := filepath.Abs(socketPath)
		if err != nil {
			return userErr("resolve socket path: %v", err)
		}
		socketPath = abs
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

	srv := bosunmcp.NewServer(rc.claims, rc.state, rc.git)
	if err := srv.Listen(socketPath); err != nil {
		return userErr("bind socket: %v", err)
	}
	defer srv.Stop()

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
