package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/state"
	"github.com/spf13/cobra"
)

// newAttachCmd builds the `bosun attach` subcommand. attach registers an
// explicit liveness PID for a session so `bosun status`'s liveness gate
// recognizes external workers (Claude Code `Task` sub-agents, CI
// runners, manually launched terminals) that wouldn't show up in the
// proc-scan because their basename isn't `claude` / `claude-code` /
// `code-cli`. The architect-mcp dogfood round saw the proc-scan flicker
// sessions CRASHED for exactly this reason; attach is the targeted fix
// (the broader `liveness_gate=external` switch in internal/config is
// the repo-wide one).
func newAttachCmd() *cobra.Command {
	var (
		pid   int
		clear bool
	)

	cmd := &cobra.Command{
		Use:   "attach <session>",
		Short: "Register an explicit liveness PID for a session",
		Long: `Write .bosun/state/<session>.attached-pid so the liveness gate
recognizes external workers that the proc-scan can't see.

Without --pid, attach uses the caller's own PID (os.Getpid()), and
requires the caller to be running inside the target session's worktree
— otherwise the registration would be meaningless. Pass --pid N to
attach an arbitrary process explicitly.

Pass --clear to remove the attached-pid file; the proc-scan fallback
resumes. Idempotent — clearing an already-absent registration is a
no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttach(cmd, args[0], attachOpts{pid: pid, clear: clear, pidExplicit: cmd.Flags().Changed("pid")})
		},
	}
	cmd.Flags().IntVar(&pid, "pid", 0, "explicit PID to register (default: caller's own PID)")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the attached-pid file instead of writing it")
	cmd.GroupID = "wiring"
	return cmd
}

type attachOpts struct {
	pid         int
	clear       bool
	pidExplicit bool
}

func runAttach(_ *cobra.Command, sessionArg string, opts attachOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	label, err := session.ParseLabel(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}

	// Confirm the session actually exists before writing — a typo
	// against `bosun attach session-9` shouldn't silently create a
	// .bosun/state/session-9.attached-pid file the next session.Derive
	// will ignore anyway (only labels backed by a real worktree make
	// it into the session list).
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	s := findSessionByLabel(sessions, label)
	if s == nil {
		return userErr("%s not found", label)
	}

	if opts.clear {
		if err := rc.state.ClearAttachedPID(label); err != nil {
			return internalErr("clear attached-pid", err)
		}
		printf("bosun: %s attached-pid cleared (proc-scan fallback resumes)\n", label)
		return nil
	}

	pid := opts.pid
	if !opts.pidExplicit || pid == 0 {
		// Implicit-PID path: only register if the caller is inside the
		// target worktree. Otherwise the PID we'd record refers to
		// whichever shell the operator happens to be in — useless for
		// liveness purposes and confusing later when it ages out.
		inside, err := callerInsideWorktree(s.Path)
		if err != nil {
			return internalErr("resolve cwd", err)
		}
		if !inside {
			return userErr("not inside the %s worktree — cd to %s or pass --pid N", label, s.Path)
		}
		pid = os.Getpid()
	}
	if pid <= 0 {
		return userErr("--pid must be a positive integer, got %d", pid)
	}

	if err := rc.state.WriteAttachedPID(label, pid); err != nil {
		return internalErr("write attached-pid", err)
	}
	printf("bosun: %s attached pid=%d (liveness gate now trusts this PID)\n", label, pid)
	return nil
}

// callerInsideWorktree reports whether the current process's cwd is
// inside (or equal to) worktreePath after symlink + abs canonicalization.
// macOS's /tmp → /private/tmp symlink is the common reason the naive
// equality check fails for tests using t.TempDir(); we canonicalize on
// both sides so the comparison survives that.
func callerInsideWorktree(worktreePath string) (bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return false, err
	}
	cwdC := canonicalize(cwd)
	targetC := canonicalize(worktreePath)
	if cwdC == targetC {
		return true, nil
	}
	// Treat cwd as inside the worktree only when the canonical target
	// is a true prefix of the canonical cwd. Adding the separator
	// avoids the `/repo-bosun-1` vs `/repo-bosun-10` false positive.
	if strings.HasPrefix(cwdC, targetC+string(filepath.Separator)) {
		return true, nil
	}
	return false, nil
}

// canonicalize returns an absolute, symlink-resolved form of path. Each
// failure step falls back to the previous best-effort value rather than
// returning an error — a path that doesn't exist still has *some*
// canonical-ish form we can compare against, and the worst case is a
// stricter false negative ("not inside the worktree, cd or pass --pid").
func canonicalize(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

// Compile-time check that state.Store still implements session.StateReader
// (Attached/Heartbeat/Read) after the v0.11 addition. Catches accidental
// signature drift in either package before the test suite runs.
var _ session.StateReader = (*state.Store)(nil)
