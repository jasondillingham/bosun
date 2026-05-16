package main

import (
	"fmt"
	"os"

	"github.com/jasondillingham/bosun/internal/launcher"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newLaunchCmd() *cobra.Command {
	var (
		isolateCache  bool
		initialPrompt string
		openAsTab     bool
		command       string
	)

	cmd := &cobra.Command{
		Use:   "launch <session>",
		Short: "Spawn an agent window for an existing session",
		Long: `Open a launcher window for one bosun-managed session — useful when a
window got closed accidentally, you want to relaunch under a different
command, or you're testing the launcher without re-running ` + "`bosun init`" + `.

The session must already exist; this is a launcher-only operation and
does not create worktrees, branches, or briefs. Use ` + "`bosun init`" + ` for that.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLaunch(args[0], launchOpts{
				isolateCache:  isolateCache,
				initialPrompt: initialPrompt,
				openAsTab:     openAsTab,
				command:       command,
			})
		},
	}

	cmd.Flags().BoolVar(&isolateCache, "isolate-cache", false, "set per-worktree build-cache env vars")
	cmd.Flags().StringVar(&initialPrompt, "initial-prompt", "", "first message passed to the launched session")
	cmd.Flags().BoolVar(&openAsTab, "tab", false, "open as a tab in an existing window (terminal-dependent)")
	cmd.Flags().StringVar(&command, "command", "claude", "command to run in the launched window")

	return cmd
}

type launchOpts struct {
	isolateCache  bool
	initialPrompt string
	openAsTab     bool
	command       string
}

// runLaunch opens a launcher window for one existing bosun session. It
// mirrors the per-session loop inside `bosun init --launch` but skips
// worktree/branch creation and runs against just the one named session.
func runLaunch(sessionArg string, opts launchOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	n, err := session.ParseName(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	var s *session.Session
	for i := range sessions {
		if sessions[i].Number == n {
			s = &sessions[i]
			break
		}
	}
	if s == nil {
		return userErr("session-%d not found (use `bosun list` to see active sessions)", n)
	}

	// Reuse the MCP daemon when one's already up, otherwise spawn one so
	// the launched session can talk to bosun_claim / bosun_done without
	// falling back to filesystem coordination. Failure to bring it up is
	// non-fatal — same policy as `bosun init --launch`.
	env := map[string]string{}
	if opts.isolateCache {
		env = launcher.IsolateCacheEnv(s.Path)
	}
	if info, err := ensureMcp(rc.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: MCP autostart failed: %v\n", err)
	} else {
		env[bosunmcp.SocketEnv] = info.socketPath
		switch {
		case info.spawned:
			fmt.Fprintf(os.Stdout, "Started MCP server (pid %d) on %s\n", info.pid, info.socketPath)
		case info.pid != 0:
			fmt.Fprintf(os.Stdout, "Reusing MCP server (pid %d) on %s\n", info.pid, info.socketPath)
		}
	}

	strategy, err := launcher.Launch(launcher.Options{
		Strategy:      launcher.Strategy(rc.cfg.Launcher),
		WorktreePath:  s.Path,
		SessionName:   s.Name,
		Command:       opts.command,
		InitialPrompt: opts.initialPrompt,
		OpenAsTab:     opts.openAsTab,
		Env:           env,
	})
	if err != nil {
		return internalErr("launch "+s.Name, err)
	}
	fmt.Fprintf(os.Stdout, "Launched %s via %s\n", s.Name, strategy)
	return nil
}
