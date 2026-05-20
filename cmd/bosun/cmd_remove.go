package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/history"
	"github.com/jasondillingham/bosun/internal/hooks"
	"github.com/jasondillingham/bosun/internal/webhooks"
	"github.com/jasondillingham/bosun/internal/launcher"
	"github.com/jasondillingham/bosun/internal/proc"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/spf13/cobra"
)

// Register `pre-remove` here rather than in internal/hooks so each
// lifecycle owner declares its own event names alongside the call-site
// that fires them — keeps the hooks package free of churn while v0.5
// rounds out per-command events in parallel branches.
func init() {
	hooks.KnownEvents = append(hooks.KnownEvents, "pre-remove")
}

func newRemoveCmd() *cobra.Command {
	var (
		force         bool
		ignoreRunning bool
	)

	cmd := &cobra.Command{
		Use:   "remove <session>",
		Short: "Tear down a session's worktree + branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemove(cmd, args[0], force, ignoreRunning)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "remove even if dirty or unmerged")
	cmd.Flags().BoolVar(&ignoreRunning, "ignore-running", false, "bypass the live-agent safety gate (discards uncommitted work the agent is editing)")

	cmd.GroupID = "finishing"
	return cmd
}

func runRemove(cmd *cobra.Command, sessionArg string, force, ignoreRunning bool) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	label, err := session.ParseLabel(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}

	// Detect the v0.6.2 crash footprint (gitdir admin missing HEAD/commondir)
	// up front: in that state Derive and `git worktree remove` both fail
	// mid-flight. With --force we salvage anything left on disk first, then
	// fall back to an `rm -rf` + prune. Without --force we surface the
	// same recovery hint as `bosun rescue`. The path comes from `git
	// worktree list` so legacy `-bosun-N` and v0.10 UID-per-worktree
	// `-bosun-<ts>-N` shapes both resolve.
	if worktreePath, ok := lookupWorktreePathByLabel(rc, label); ok {
		if cerr := git.WorktreeGitdirCorruption(rc.repoRoot, worktreePath); cerr != nil {
			if !force {
				return userErr(
					"%s worktree gitdir is corrupted (%v) — pass --force to salvage and remove",
					label, cerr)
			}
			return removeCorruptedWorktree(rc, label, worktreePath)
		}
	}

	// Drop admin metadata for worktrees whose directory was manually
	// removed before deriving sessions — otherwise Derive (which now
	// skips prunable entries) would report this session as missing
	// and the leftover branch + state would linger.
	if err := rc.git.PruneWorktrees(rc.ctx, rc.repoRoot); err != nil {
		return gitErr("prune worktrees", err)
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	s := findSessionByLabel(sessions, label)
	if s == nil {
		// Session has no worktree, but its branch may still exist
		// (e.g. operator rm -rf'd the dir, prune cleaned admin files,
		// branch is the only thing left). Honor the remove request by
		// deleting that branch and any state/claims for the label.
		branch := rc.cfg.BranchForLabel(label)
		exists, berr := rc.git.BranchExists(rc.ctx, rc.repoRoot, branch)
		if berr != nil {
			return gitErr("check branch "+branch, berr)
		}
		if !exists {
			return userErr("%s not found", label)
		}
		if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, branch, true); err != nil {
			return gitErr("delete branch "+branch, err)
		}
		_ = rc.claims.Clear(label)
		_ = rc.state.Clear(label)
		printf("bosun: removed %s (worktree dir was already gone; cleaned up branch + state)\n", label)
		return nil
	}

	// Liveness gate: if an agent is actively editing this worktree AND
	// the tree is dirty, refuse before touching anything. The committed
	// work is recoverable from the branch; the uncommitted work the agent
	// is mid-edit on is what `rm -rf` would silently destroy. --force
	// alone is too coarse here (operators reach for it routinely) — the
	// --ignore-running opt-in is loud enough that no one trips it twice.
	if !ignoreRunning && s.Running && s.Dirty > 0 {
		return userErr("%s", liveAgentRemoveMessage(label, s.RunningPID))
	}

	// v0.9: spawn-tree refusal — same shape as cleanup. A parent with
	// live children must be reaped via `bosun cleanup --tree <parent>`
	// so the descendants get the same safety gates each, in dependency
	// order. Removing a parent here would orphan its children.
	children, _ := spawntree.NewStore(rc.repoRoot).ChildrenOf(label)
	if len(children) > 0 && !force {
		return userErr("%s has %d live sub-session(s): %s\n"+
			"       reap them first via `bosun cleanup --tree %s`,\n"+
			"       or pass --force to remove this session anyway (orphans the children in the spawn tree)",
			label, len(children), strings.Join(children, ", "), label)
	}

	// destructive controls whether we let git's own safety checks (`branch -d`,
	// `worktree remove` without --force) gate the operation. We bypass them when
	// the user passed --force, OR when patch-id analysis says the branch's
	// content is already on base (e.g. after `bosun merge` squashed it).
	destructive := force
	if !force {
		if s.Dirty > 0 {
			return userErr("%s has %d uncommitted change(s); commit or stash, or pass --force", label, s.Dirty)
		}
		if s.Ahead > 0 {
			unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, s.Branch)
			if err != nil {
				return gitErr("check unmerged patches for "+s.Branch, err)
			}
			if unmerged > 0 {
				return userErr("%s has %d commit(s) ahead of %s that aren't merged; pass --force to discard", label, unmerged, rc.cfg.BaseBranch)
			}
			// All ahead-commits are patch-equivalent to base — squash-merged.
			// git itself won't accept `branch -d` here because the tip SHA isn't
			// reachable from base, so escalate to force for the git calls only.
			destructive = true
		}
	}

	// Fire pre-remove BEFORE any destructive op so a hook with
	// `fail_open: false` can snapshot the worktree (or veto entirely)
	// while it still exists on disk. The orphan-branch fast-path above
	// skips this hook by design: there's no worktree to snapshot and
	// the safety-check env vars (ahead/dirty) aren't meaningful.
	hookEnv := map[string]string{
		"BOSUN_REPO_ROOT":     rc.repoRoot,
		"BOSUN_SESSION":       label,
		"BOSUN_BRANCH":        s.Branch,
		"BOSUN_WORKTREE_PATH": s.Path,
		"BOSUN_AHEAD":         strconv.Itoa(s.Ahead),
		"BOSUN_DIRTY":         strconv.Itoa(s.Dirty),
	}
	if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "pre-remove", hookEnv); err != nil {
		return userErr("%v", err)
	}
	_ = webhooks.Fire(rc.ctx, rc.cfg.Webhooks, "pre-remove", hookEnv)

	// Archive the session before the destructive ops wipe it. Best-effort:
	// a failure here logs to stderr but never blocks remove, since history
	// is observability rather than load-bearing state.
	detail := ""
	if force {
		detail = "--force"
	}
	if ignoreRunning {
		if detail != "" {
			detail += ", "
		}
		detail += "--ignore-running"
	}
	if _, err := history.Archive(rc.ctx, history.ArchiveInput{
		RepoRoot:     rc.repoRoot,
		Label:        label,
		Branch:       s.Branch,
		WorktreePath: s.Path,
		EndReason:    history.ReasonRemoved,
		Detail:       detail,
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: archive history for %s: %v\n", label, err)
	}

	// Salvage uncommitted content before destruction whenever the operator
	// is forcing past safety checks: `--force` is exactly the case where
	// `git worktree remove --force` would otherwise discard dirty files
	// with no recovery path. Empty snapshots (nothing dirty) are cleaned
	// up; failures here are warnings, not fatal — the salvage is
	// belt-and-suspenders.
	if destructive {
		salvagePath, n, salvageErr := salvageWorktreeContent(rc, s.Label, s.Path)
		if salvageErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: salvage %s: %v\n", s.Label, salvageErr)
		} else if salvagePath != "" {
			printf("bosun: salvaged %d file(s) from %s → %s\n", n, s.Label, salvagePath)
		}
	}

	// Phase 3 lane 4: stop the remote container first if the session
	// was launched against a remote Docker daemon — the local PID
	// (when present) is just the SSH/docker-cli wrapper, and ignoring
	// the remote leaves an orphaned container running on the host.
	// Best-effort: log-and-continue on a failed stop so the local
	// teardown still proceeds when the host is unreachable.
	stopRemoteContainer(s.DockerHost, s.Label)

	// Terminate any agent still running in this worktree before the
	// dir disappears. Mirrors the cleanup path; the operator chose to
	// remove the session, so a still-running agent should go too.
	if s.Running && s.RunningPID > 0 {
		if err := proc.Terminate(s.RunningPID, proc.DefaultTerminateGrace); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: terminate agent pid %d for %s: %v\n", s.RunningPID, label, err)
		} else {
			printf("bosun: terminated agent (pid %d) in %s\n", s.RunningPID, label)
		}
	}

	// Close the tmux window opened by launchTmux, if any. Same
	// log-and-continue contract as the cleanup path.
	if err := launcher.CloseTmuxWindow(label); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: close tmux window %s: %v\n", label, err)
	}

	if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, s.Path, destructive); err != nil {
		return gitErr("remove worktree "+s.Path, err)
	}
	if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, s.Branch, destructive); err != nil {
		return gitErr("delete branch "+s.Branch, err)
	}
	_ = rc.claims.Clear(label)
	_ = rc.state.Clear(label)
	_ = spawntree.NewStore(rc.repoRoot).Remove(label)

	printf("bosun: removed %s (worktree + branch + state)\n", label)
	return nil
}

// removeCorruptedWorktree handles `bosun remove <label> --force` against a
// worktree whose gitdir admin files are missing (v0.6.2 crash footprint).
// Standard git ops fail in this state, so we salvage everything still on
// disk, `rm -rf` the worktree dir, and let `git worktree prune` clean up
// the (now-orphan) admin metadata. State + claims are cleared regardless;
// branch deletion is best-effort because it may already be unreachable.
func removeCorruptedWorktree(rc *runCtx, label, worktreePath string) error {
	salvagePath, n, salvageErr := salvageWorktreeContent(rc, label, worktreePath)
	if salvageErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: salvage %s: %v\n", label, salvageErr)
	}

	if err := os.RemoveAll(worktreePath); err != nil {
		return internalErr("remove worktree dir "+worktreePath, err)
	}
	if err := rc.git.PruneWorktrees(rc.ctx, rc.repoRoot); err != nil {
		// Best-effort: the orphan admin dir will be cleaned next prune cycle.
		_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: prune worktree admin: %v\n", err)
	}
	branch := rc.cfg.BranchForLabel(label)
	if exists, _ := rc.git.BranchExists(rc.ctx, rc.repoRoot, branch); exists {
		if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, branch, true); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: delete branch %s: %v\n", branch, err)
		}
	}
	_ = rc.claims.Clear(label)
	_ = rc.state.Clear(label)
	_ = spawntree.NewStore(rc.repoRoot).Remove(label)

	if salvagePath != "" {
		printf("bosun: removed %s (gitdir was corrupted) — %d file(s) salvaged to %s\n", label, n, salvagePath)
	} else {
		printf("bosun: removed %s (gitdir was corrupted; nothing salvageable on disk)\n", label)
	}
	return nil
}

// salvageWorktreeContent snapshots uncommitted content from worktreePath
// to `.bosun/rescues/<label>-<timestamp>/`. Tries the git-driven path
// first (snapshot only dirty + untracked entries via `git status`); if
// git fails — the corrupted-gitdir case — falls back to copying every
// file in the worktree dir except `.git` itself. Returns the snapshot
// directory path, the file count, and any error. An empty snapshot
// (nothing to salvage) returns ("", 0, nil) and the empty dir is removed.
func salvageWorktreeContent(rc *runCtx, label, worktreePath string) (string, int, error) {
	if _, err := os.Stat(worktreePath); err != nil {
		return "", 0, nil
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(rc.repoRoot, rescueDirRelative, label+"-"+ts)

	lines, statusErr := rc.git.Status(rc.ctx, worktreePath)
	if statusErr == nil {
		if err := os.MkdirAll(dest, 0o750); err != nil {
			return "", 0, err
		}
		n, err := copyRescueFiles(worktreePath, dest, lines)
		if err != nil {
			return "", 0, err
		}
		if n == 0 {
			_ = os.Remove(dest)
			return "", 0, nil
		}
		return dest, n, nil
	}

	// Git is broken (corrupted gitdir, repo metadata torched, …) — copy
	// everything except `.git` so the operator can sort it out by hand.
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return "", 0, err
	}
	n, skipped, err := copyWorktreeBestEffort(worktreePath, dest)
	if err != nil {
		return "", 0, err
	}
	// Surface every dropped file individually + a summary line. In a
	// corrupted-gitdir rescue the operator NEEDS to know which files
	// didn't make it into the snapshot; the silent "salvaged N" count
	// the previous shape produced was load-bearing misinformation.
	for _, s := range skipped {
		_, _ = fmt.Fprintf(os.Stderr, "bosun: rescue: skipped %s (%s)\n", s.rel, s.reason)
	}
	if len(skipped) > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "bosun: rescue: %d file(s) couldn't be salvaged — see warnings above\n", len(skipped))
	}
	if n == 0 {
		_ = os.Remove(dest)
		return "", 0, nil
	}
	return dest, n, nil
}

// skippedItem records one file the best-effort salvage couldn't copy.
// In a corrupted-gitdir crisis, the operator needs to know which files
// were preserved vs lost — silently returning a "salvaged N files"
// count without naming the rest is a foot-gun.
type skippedItem struct {
	rel    string
	reason string
}

// copyWorktreeBestEffort walks src and copies every regular file (and
// symlink) under it to dest, skipping `.git` (a stale pointer file that
// would make the snapshot itself look like a broken worktree). Used as
// the fallback when `git status` fails because the gitdir is corrupted.
//
// Returns (copied, skipped, walkErr). Per-file errors (unreadable
// symlinks, copyFile failures) are not fatal — the salvage is
// best-effort — but they ARE recorded in skipped so the caller can
// surface the list to the operator instead of pretending the snapshot
// is complete.
func copyWorktreeBestEffort(src, dest string) (int, []skippedItem, error) {
	n := 0
	var skipped []skippedItem
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			rel = path
		}
		if err != nil {
			skipped = append(skipped, skippedItem{rel: rel, reason: fmt.Sprintf("walk error: %v", err)})
			return nil
		}
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			// `.git` can be a directory (main worktree) or a pointer file
			// (linked worktree). Either way, skip it: in the directory case
			// SkipDir skips contents; in the file case SkipDir would skip
			// the rest of the parent — return nil so we just don't copy it.
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		out := filepath.Join(dest, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(out, 0o750)
		case info.Mode()&os.ModeSymlink != 0:
			target, terr := os.Readlink(path)
			if terr != nil {
				skipped = append(skipped, skippedItem{rel: rel, reason: fmt.Sprintf("readlink: %v", terr)})
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return err
			}
			if err := os.Symlink(target, out); err != nil {
				skipped = append(skipped, skippedItem{rel: rel, reason: fmt.Sprintf("symlink: %v", err)})
				return nil
			}
			n++
		default:
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return err
			}
			if err := copyFile(path, out, info.Mode().Perm()); err != nil {
				skipped = append(skipped, skippedItem{rel: rel, reason: fmt.Sprintf("copy: %v", err)})
				return nil
			}
			n++
		}
		return nil
	})
	return n, skipped, err
}

// liveAgentRemoveMessage is the user-facing error returned when the
// liveness gate fires on `bosun remove`. The recovery hint names both
// the "let it finish" path (the safe default) and the --ignore-running
// escape hatch so the operator never has to grep our docs to recover.
// Returned as a single multi-line string — userErr stitches the
// "bosun: " prefix on the first line via its formatter.
func liveAgentRemoveMessage(label string, pid int) string {
	return fmt.Sprintf(
		"%s has a live agent (pid %d) and uncommitted changes — refusing remove\n"+
			"       to avoid losing in-flight work. Recovery:\n"+
			"       - let the agent finish, OR\n"+
			"       - bosun remove %s --ignore-running (discards uncommitted work)",
		label, pid, label)
}
