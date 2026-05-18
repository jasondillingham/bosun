package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jasondillingham/bosun/internal/web"
	"github.com/spf13/cobra"
)

// servePidfileRelative is the path (under the main worktree's repo root)
// where `bosun serve` records its pid and bound address so `bosun events
// --tail` can auto-detect a running dashboard without flag-juggling.
const servePidfileRelative = ".bosun/serve.pid"

func newServeCmd() *cobra.Command {
	var (
		port     int
		bind     string
		interval int
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run an HTTP dashboard server (JSON + SSE; loopback-only by default)",
		Long: `Run a long-lived HTTP server that exposes the same data the TUI shows
over JSON (/api/status) and Server-Sent Events (/api/events). A minimal
HTML page at / consumes both, giving a browser-based view of the fleet.

Defaults to binding 127.0.0.1 — there is no authentication, so binding to
a non-loopback address exposes the dashboard to anyone on that network at
your own risk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if port < 0 || port > 65535 {
				return userErr("--port must be 0..65535")
			}
			if interval < 1 {
				return userErr("--interval must be >= 1 (seconds)")
			}
			if bind == "" {
				return userErr("--bind must not be empty")
			}
			return runServe(bind, port, time.Duration(interval)*time.Second)
		},
	}

	cmd.Flags().IntVar(&port, "port", 8765, "TCP port to bind")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "address to bind (non-loopback exposes the dashboard with no auth)")
	cmd.Flags().IntVar(&interval, "interval", 2, "seconds between /api/status recomputes")

	cmd.GroupID = "wiring"
	return cmd
}

func runServe(bind string, port int, interval time.Duration) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	srv := web.New(web.Config{
		RepoRoot: rc.repoRoot,
		Git:      rc.git,
		Cfg:      rc.cfg,
		Claims:   rc.claims,
		State:    rc.state,
		Bind:     bind,
		Port:     port,
		Interval: interval,
	})

	fmt.Fprintf(os.Stdout, "bosun serve: listening on http://%s:%d\n", bind, port)
	fmt.Fprintln(os.Stdout, "bosun serve: ^C to stop")

	ctx, cancel := signal.NotifyContext(rc.ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Write the serve pidfile once the listener is up so `bosun events
	// --tail` can find us. The pidfile lives at .bosun/serve.pid and is
	// removed on shutdown — a stale file pointing at a dead pid is
	// recoverable (the events client falls through to an error), but
	// removing it eagerly keeps the happy path clean.
	pidfileCtx, pidfileCancel := context.WithCancel(ctx)
	defer pidfileCancel()
	go writeServePidfileWhenReady(pidfileCtx, rc.repoRoot, srv)
	defer removeServePidfile(rc.repoRoot)

	if err := srv.Start(ctx); err != nil {
		return internalErr("serve", err)
	}
	fmt.Fprintln(os.Stdout, "\nbosun serve: stopped")
	return nil
}

// writeServePidfileWhenReady polls srv.Addr() until the listener has
// bound (Addr() is "" until Start runs net.Listen), then writes the
// pidfile. Runs as a goroutine because Start blocks for the lifetime of
// the server. Cancellation (ctx Done) stops the wait without writing —
// covers the early-shutdown case where Start fails before binding.
func writeServePidfileWhenReady(ctx context.Context, repoRoot string, srv *web.Server) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if addr := srv.Addr(); addr != "" {
			if err := writeServePidfile(repoRoot, os.Getpid(), addr); err != nil {
				fmt.Fprintf(os.Stderr, "bosun serve: warning: write pidfile: %v\n", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// writeServePidfile records the dashboard's pid + bound address (host:port)
// at .bosun/serve.pid so `bosun events --tail` can locate the running
// instance without the user typing `--url`. Format mirrors the MCP
// pidfile: `<pid>\n<addr>\n`.
//
// Writes atomically via temp + rename so a concurrent reader (a
// fast-launched `bosun events --tail`) never observes a half-written
// file.
func writeServePidfile(repoRoot string, pid int, addr string) error {
	final := filepath.Join(repoRoot, servePidfileRelative)
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
	if _, err := fmt.Fprintf(tmp, "%d\n%s\n", pid, addr); err != nil {
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

// removeServePidfile clears .bosun/serve.pid on shutdown. Best-effort:
// a stale pidfile pointing at a dead pid is recoverable, so we don't
// surface errors here.
func removeServePidfile(repoRoot string) {
	_ = os.Remove(filepath.Join(repoRoot, servePidfileRelative))
}
