package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jasondillingham/bosun/internal/launcher"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

// defaultBriefPrompt is the initial-prompt string injected into a launched
// session when no explicit --initial-prompt is passed and a BOSUN_BRIEF.md
// is available in the worktree. Shared between `bosun init --launch` and
// `bosun launch` so the two paths stay in lockstep.
const defaultBriefPrompt = "Read BOSUN_BRIEF.md in this directory — it's your assignment. Read it in full, then follow the workflow it describes."

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
	cmd.Flags().StringVar(&command, "command", "", "agent command to run (defaults to the session's persisted command, or config.agent_command, or `claude`)")

	cmd.GroupID = "wiring"
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
	label, err := session.ParseLabel(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	s := findSessionByLabel(sessions, label)
	if s == nil {
		return userErr("%s not found (use `bosun list` to see active sessions)", label)
	}

	// When the caller didn't pass --initial-prompt, mirror `bosun init
	// --launch`: if the worktree has a BOSUN_BRIEF.md, default to pointing
	// the agent at it. Otherwise leave the prompt empty so the launched
	// session opens silently (bare `claude`).
	prompt := opts.initialPrompt
	if prompt == "" {
		if _, err := os.Stat(filepath.Join(s.Path, "BOSUN_BRIEF.md")); err == nil {
			prompt = defaultBriefPrompt
		}
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
		_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: MCP autostart failed: %v\n", err)
	} else {
		env[bosunmcp.SocketEnv] = info.socketPath
		switch {
		case info.spawned:
			_, _ = fmt.Fprintf(os.Stdout, "Started MCP server (pid %d) on %s\n", info.pid, info.socketPath)
		case info.pid != 0:
			_, _ = fmt.Fprintf(os.Stdout, "Reusing MCP server (pid %d) on %s\n", info.pid, info.socketPath)
		}
	}

	// Resolve agent command with the documented precedence:
	// CLI flag > session's persisted override > config default.
	// Persisted overrides land in Session.AgentCommand via Derive.
	command := opts.command
	if command == "" {
		command = s.AgentCommand
	}
	if command == "" {
		command = rc.cfg.AgentCommand
	}

	strategy, err := launcher.Launch(launcher.Options{
		Strategy:      launcher.Strategy(rc.cfg.Launcher),
		WorktreePath:  s.Path,
		SessionName:   s.Label,
		Command:       command,
		InitialPrompt: prompt,
		OpenAsTab:     opts.openAsTab,
		Env:           env,
	})
	if err != nil {
		return internalErr("launch "+s.Label, err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "Launched %s via %s\n", s.Label, strategy)
	return nil
}
