package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/launcher"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

// rescueDirRelative is the on-disk location bosun rescue writes snapshots
// into. Sibling of state/ + claims/ under .bosun/ so the existing gitignore
// for .bosun/ keeps rescued files out of commits.
const rescueDirRelative = ".bosun/rescues"

func newRescueCmd() *cobra.Command {
	var (
		launch bool
	)

	cmd := &cobra.Command{
		Use:   "rescue <session>",
		Short: "Snapshot or relaunch a CRASHED session for manual recovery",
		Long: `Rescue is a narrow recovery tool for sessions that bosun has flagged
CRASHED — i.e. the agent process is gone but the worktree still has
uncommitted changes that would otherwise be lost on ` + "`bosun remove`" + `.

Default mode (snapshot): copies the modified and untracked files out of
the worktree to ` + "`.bosun/rescues/<session>-<timestamp>/`" + ` so the operator
can inspect the abandoned work before deciding what to do with the
session. The worktree itself is left untouched.

--launch mode: opens a fresh terminal window in the worktree so the
operator can take over manually (commit, continue editing, or
` + "`bosun done`" + `). Uses the same launcher logic as ` + "`bosun launch`" + `.

Rescue refuses any session that isn't CRASHED — for a general-purpose
launcher, use ` + "`bosun launch`" + ` instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRescue(args[0], rescueOpts{launch: launch})
		},
	}

	cmd.Flags().BoolVar(&launch, "launch", false, "open a terminal in the worktree instead of snapshotting")

	cmd.GroupID = "finishing"
	return cmd
}

type rescueOpts struct {
	launch bool
}

func runRescue(sessionArg string, opts rescueOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	label, err := session.ParseLabel(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}

	// Guard against the v0.6.2 crash footprint: an agent that died under
	// load can leave the worktree's gitdir admin files missing, in which
	// case every subsequent git op (including the Derive below) fails
	// with "not a git repository" mid-snapshot. Detect that up front so
	// the operator sees an actionable recovery hint instead of a confusing
	// git error from inside Derive. The path comes from `git worktree
	// list` rather than reconstructed via the suffix pattern so both
	// legacy `-bosun-N` dirs and the v0.10 UID-per-worktree
	// `-bosun-<ts>-N` form resolve correctly.
	if worktreePath, ok := lookupWorktreePathByLabel(rc, label); ok {
		if cerr := git.WorktreeGitdirCorruption(rc.repoRoot, worktreePath); cerr != nil {
			return userErr(
				"%s's worktree gitdir is corrupted (%v).\n"+
					"       The agent likely crashed before writing the gitdir cleanly.\n"+
					"       Recovery:\n"+
					"       - bosun remove %s --force  (preserves uncommitted files via rescue snapshot)\n"+
					"       - then re-init the session from scratch.",
				label, cerr, label)
		}
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	s := findSessionByLabel(sessions, label)
	if s == nil {
		return userErr("%s not found (use `bosun list` to see active sessions)", label)
	}
	if s.State != session.StateCrashed {
		return userErr("%s is %s, not CRASHED — rescue is for the crash-recovery case only (use `bosun launch` for a general-purpose window)", label, s.State)
	}

	if opts.launch {
		return rescueLaunch(rc, s)
	}
	return rescueSnapshot(rc, s)
}

// rescueSnapshot copies the dirty + untracked files from the crashed
// worktree into .bosun/rescues/<session>-<timestamp>/, preserving the
// repo-relative path layout. The original worktree is left untouched so
// the operator can compare or re-rescue if needed.
func rescueSnapshot(rc *runCtx, s *session.Session) error {
	lines, err := rc.git.Status(rc.ctx, s.Path)
	if err != nil {
		return gitErr("status "+s.Label, err)
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(rc.repoRoot, rescueDirRelative, s.Label+"-"+ts)
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return internalErr("create rescue dir", err)
	}

	copied, err := copyRescueFiles(s.Path, dest, lines)
	if err != nil {
		return internalErr("snapshot files", err)
	}

	if copied == 0 {
		// No files had content to save — likely all dirty entries were
		// deletions or rename-removals. Still leave the (empty) dir so the
		// operator can see rescue ran, but flag it in output.
		printf("bosun: %s rescued (no copyable files; deletions only) → %s\n", s.Label, dest)
		return nil
	}
	printf("bosun: %s rescued — %d file(s) copied to %s\n", s.Label, copied, dest)
	return nil
}

// copyRescueFiles iterates parsed `git status --porcelain` lines and copies
// each entry that still has content on disk into dest, preserving relative
// paths. Returns the count actually copied. Pure deletions (D) and other
// content-less entries are skipped silently.
func copyRescueFiles(worktree, dest string, lines []git.PorcelainStatusLine) (int, error) {
	copied := 0
	for _, line := range lines {
		rel := line.Path
		if rel == "" {
			continue
		}
		// Strip the trailing slash git reports for untracked directories
		// so filepath.Join below doesn't produce a doubled separator.
		rel = filepath.Clean(rel)
		src := filepath.Join(worktree, rel)
		info, err := os.Lstat(src)
		if err != nil {
			continue
		}
		out := filepath.Join(dest, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			target, terr := os.Readlink(src)
			if terr != nil {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return copied, err
			}
			if err := os.Symlink(target, out); err != nil {
				return copied, err
			}
			copied++
			continue
		}
		if info.IsDir() {
			n, err := copyTree(src, out)
			if err != nil {
				return copied, err
			}
			copied += n
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return copied, err
		}
		if err := copyFile(src, out, info.Mode().Perm()); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}

// copyTree walks src and copies every regular file (and symlink) under it
// to a mirrored path under dest. Returns the count of files written.
func copyTree(src, dest string) (int, error) {
	n := 0
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		out := filepath.Join(dest, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(out, 0o750)
		case info.Mode()&os.ModeSymlink != 0:
			target, terr := os.Readlink(path)
			if terr != nil {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return err
			}
			if err := os.Symlink(target, out); err != nil {
				return err
			}
			n++
		default:
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return err
			}
			if err := copyFile(path, out, info.Mode().Perm()); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// copyFile is a small file-copy helper that preserves mode. Not using
// io.Copy with os.O_EXCL since rescue dirs are created fresh per
// invocation and overwriting inside the dest is fine.
func copyFile(src, dest string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// rescueLaunch opens a terminal window inside the crashed worktree so the
// operator can take over manually. Mirrors cmd_launch.go's runLaunch with
// no isolate-cache and no initial-prompt — the operator is driving.
func rescueLaunch(rc *runCtx, s *session.Session) error {
	env := map[string]string{}
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

	strategy, err := launcher.Launch(launcher.Options{
		Strategy:     launcher.Strategy(rc.cfg.Launcher),
		WorktreePath: s.Path,
		SessionName:  s.Label,
		Command:      "bash",
		Env:          env,
	})
	if err != nil {
		return internalErr("rescue launch "+s.Label, err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "bosun: rescued %s via %s — terminal is yours\n", s.Label, strategy)
	return nil
}
