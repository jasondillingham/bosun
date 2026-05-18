package main

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/launcher"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/jasondillingham/bosun/internal/tui/control"
	"github.com/spf13/cobra"
)

func newTuiCmd() *cobra.Command {
	var noColor bool

	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive control center for the session fleet",
		Long: `Open a long-running terminal UI showing every bosun session and
keybinds for the common operator actions (merge, cleanup, remove, launch,
brief preview). The display auto-refreshes every 2 seconds.

Keys: j/k move · m merge selected (must be DONE) · M merge all ready ·
c cleanup · r remove (asks to confirm) · l launch · s toggle brief preview ·
R manual refresh · q / Ctrl-C quit.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTui(noColor)
		},
	}

	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable color even on a TTY")

	cmd.GroupID = "wiring"
	return cmd
}

func runTui(noColor bool) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	services := buildTuiServices(rc)
	model := control.New(services, noColor)
	prog := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		return internalErr("tui", err)
	}
	return nil
}

// buildTuiServices wires the control.Services callbacks to the real
// bosun command helpers (mergeOne, cleanupOne, launcher.Launch, etc.).
// Pulling this out makes runTui short and lets tests stub services
// independently if we ever need to (today the unit tests construct
// fake Services directly in the control package).
func buildTuiServices(rc *runCtx) control.Services {
	return control.Services{
		Refresh: func() ([]session.Session, []status.Event, error) {
			sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
			if err != nil {
				return nil, nil, fmt.Errorf("derive sessions: %w", err)
			}
			// Mirror cmd_status's enrichment so the TUI shows the
			// same tree shape + sub-task counter the CLI does.
			enrichWithSpawnTree(rc.repoRoot, sessions)
			enrichWithSubtasks(rc.repoRoot, sessions)
			return sessions, recentEvents(rc.repoRoot), nil
		},
		MergeOne: func(s session.Session) (string, string, error) {
			return mergeOne(rc, &s, mergeOpts{})
		},
		MergeAllReady: func() ([]control.ActionResult, error) {
			return tuiMergeAllReady(rc)
		},
		CleanupAll: func() ([]control.ActionResult, error) {
			return tuiCleanupAll(rc)
		},
		Remove: func(s session.Session) error {
			return tuiRemove(rc, &s)
		},
		Launch: func(s session.Session) error {
			return tuiLaunch(rc, &s)
		},
		ReadBrief: func(worktreePath string) (string, error) {
			return brief.ReadFromWorktree(worktreePath)
		},
	}
}

// tuiMergeAllReady mirrors the policy of `bosun merge` (no args): only
// DONE sessions are attempted, dependencies are honored, the first
// conflict stops the run. Returns one ActionResult per session it
// considered so the TUI can render a summary.
func tuiMergeAllReady(rc *runCtx) ([]control.ActionResult, error) {
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return nil, gitErr("derive sessions", err)
	}
	depMap, err := brief.LoadArchivedDeps(rc.repoRoot)
	if err != nil {
		return nil, internalErr("load archived deps", err)
	}
	if cycle := brief.FindDependencyCycle(depMap); cycle != nil {
		return nil, userErr("dependency cycle detected: %s — edit the plan, run `bosun init`, then retry", strings.Join(cycle, " → "))
	}
	sessions = topoOrderForMerge(sessions, depMap)
	mergedThisRun := make(map[string]bool, len(sessions))

	var results []control.ActionResult
	for _, s := range sessions {
		if s.State != session.StateDone {
			continue
		}
		if blocker, ok := blockingDep(s.Label, depMap, sessions, mergedThisRun, rc); ok {
			results = append(results, control.ActionResult{
				Session: s.Name, Status: mergeStatusSkipped,
				Reason: fmt.Sprintf("depends on %s", blocker),
			})
			continue
		}
		statusStr, reason, err := mergeOne(rc, &s, mergeOpts{})
		if err != nil {
			return results, err
		}
		results = append(results, control.ActionResult{
			Session: s.Name, Status: statusStr, Reason: reason,
		})
		if statusStr == mergeStatusMerged {
			mergedThisRun[s.Label] = true
		}
		if statusStr == mergeStatusConflict {
			break
		}
	}
	return results, nil
}

// tuiCleanupAll mirrors `bosun cleanup` (no flags): removes DONE/empty/
// squash-merged sessions, skips ones with in-flight work.
func tuiCleanupAll(rc *runCtx) ([]control.ActionResult, error) {
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return nil, gitErr("derive sessions", err)
	}
	var results []control.ActionResult
	for _, s := range sessions {
		action, reason, err := cleanupOne(rc, &s, cleanupOpts{})
		if err != nil {
			return results, err
		}
		statusStr := "skipped"
		if action == cleanupRemove {
			statusStr = "removed"
		}
		results = append(results, control.ActionResult{
			Session: s.Name, Status: statusStr, Reason: reason,
		})
	}
	return results, nil
}

// tuiRemove uses the same logic as `bosun remove <session>` without
// --force, so dirty/unmerged sessions refuse instead of silently
// destroying work. The TUI's confirm prompt is for the "I meant this
// session" question, not "are you sure you want to discard data."
func tuiRemove(rc *runCtx, s *session.Session) error {
	if s.Dirty > 0 {
		return fmt.Errorf("%d uncommitted change(s) — commit/stash from terminal first", s.Dirty)
	}
	if s.Ahead > 0 {
		unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, s.Branch)
		if err != nil {
			return fmt.Errorf("check unmerged: %w", err)
		}
		if unmerged > 0 {
			return fmt.Errorf("%d unmerged commit(s) — merge or --force from terminal first", unmerged)
		}
	}
	forceWT, forceBranch := true, true
	if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, s.Path, forceWT); err != nil {
		return fmt.Errorf("remove worktree %s: %w", s.Path, err)
	}
	if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, s.Branch, forceBranch); err != nil {
		return fmt.Errorf("delete branch %s: %w", s.Branch, err)
	}
	_ = rc.claims.Clear(s.Name)
	_ = rc.state.Clear(s.Name)
	return nil
}

// tuiLaunch opens a launcher window for the session. Mirrors the
// `bosun launch <session>` defaults — same command (claude), no
// initial prompt, no cache isolation. The MCP socket is reused/spawned
// like the CLI does so the launched agent gets bosun_claim et al.
func tuiLaunch(rc *runCtx, s *session.Session) error {
	env := map[string]string{}
	if info, err := ensureMcp(rc.repoRoot); err == nil && info.socketPath != "" {
		env[bosunmcp.SocketEnv] = info.socketPath
	}
	if _, err := launcher.Launch(launcher.Options{
		Strategy:     launcher.Strategy(rc.cfg.Launcher),
		WorktreePath: s.Path,
		SessionName:  s.Name,
		Command:      "claude",
		Env:          env,
		Out:          os.Stdout,
	}); err != nil {
		return fmt.Errorf("launch %s: %w", s.Name, err)
	}
	return nil
}

