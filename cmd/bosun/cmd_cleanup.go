package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/hooks"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/spf13/cobra"
)

func newCleanupCmd() *cobra.Command {
	var (
		dryRun        bool
		force         bool
		purge         bool
		orphansArg    int
		orphanDirs    bool
		ignoreRunning bool
		tree          string
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Batch-remove DONE or empty sessions",
		Long: `Remove every bosun-managed session that is either marked DONE (and its
content is on base) or has no work in it (no commits ahead of base, no
uncommitted changes). Sessions with in-flight work are skipped with a
reason.

Pass --force to also remove sessions with uncommitted-only work. A session
whose commits aren't yet on base ("DONE-but-unmerged" — the v0.5 round-1
incident shape) refuses --force; the operator must either ` + "`bosun merge`" + `
the session first or pass --purge to explicitly drop the commits.

Pass --dry-run to print what would happen without changing anything.

Pass --orphans=N to instead clean up sessions whose number is greater than
N — typical after ` + "`bosun init --force`" + ` shrinks the session count and leaves
the trailing worktrees behind. ` + "`--orphans`" + ` without a value defaults to the
configured default_session_count.

Pass --orphan-dirs to also scan the repo's parent directory for sibling
worktree directories whose git admin metadata is already gone — the shape
the v0.3 corruption left behind. Independent of --orphans (which filters by
session number).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := cleanupOpts{dryRun: dryRun, force: force, purge: purge, orphanDirs: orphanDirs, ignoreRunning: ignoreRunning}
			if cmd.Flags().Changed("orphans") {
				opts.orphansMode = true
				opts.orphansKeep = orphansArg
			}
			if tree != "" {
				return runCleanupTree(cmd, tree, opts)
			}
			return runCleanup(cmd, opts)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen, don't act")
	cmd.Flags().BoolVar(&force, "force", false, "also remove sessions with uncommitted-only changes")
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove sessions whose committed work isn't on base yet — discards those commits")
	cmd.Flags().IntVar(&orphansArg, "orphans", 0, "only act on sessions whose number is greater than this (0 means use config.default_session_count)")
	cmd.Flags().BoolVar(&orphanDirs, "orphan-dirs", false, "also scan parent dir for worktree directories git no longer tracks and remove them")
	cmd.Flags().BoolVar(&ignoreRunning, "ignore-running", false, "bypass the live-agent safety gate for every session in the batch (discards uncommitted work agents are editing)")
	cmd.Flags().StringVar(&tree, "tree", "", "cascade-cleanup a parent session and all its sub-sessions (children first, parent last). Mutually exclusive with --orphans.")
	// NoOptDefVal lets `--orphans` work without a value (cobra parses it as
	// `--orphans=0`), at which point runCleanup falls back to the config
	// default. The string form is what cobra requires here.
	cmd.Flags().Lookup("orphans").NoOptDefVal = "0"

	cmd.GroupID = "finishing"
	return cmd
}

type cleanupOpts struct {
	dryRun bool
	// force permits removal of sessions with uncommitted-only work.
	// Sessions whose commits aren't on base by patch-id AND whose tree
	// diverges from base ("would-discard-commits") are NOT covered — they
	// require purge. The v0.5 round-1 incident proved silent loss of
	// committed work is the worst outcome here, so the two flags split.
	force bool
	// purge explicitly opts in to discarding committed work that isn't
	// on base yet. Loud and rarely needed; the recovery hint in skip
	// reasons points operators here when they really meant it.
	purge bool
	// orphansMode flips planCleanup into "act on sessions with number >
	// orphansKeep only" — for cleaning up the trailing worktrees that
	// linger when a later `bosun init` shrinks the session count.
	orphansMode bool
	// orphansKeep is the highest session number to keep when orphansMode
	// is set. Zero means "fall back to cfg.DefaultSessionCount in
	// runCleanup." A negative value would remove every session, which we
	// reject up front.
	orphansKeep int
	// orphanDirs additionally scans the repo's parent directory for
	// worktree-shaped directories that git's worktree list doesn't track
	// (the v0.3 corruption case: data on disk, admin metadata pruned).
	// Acts in addition to the normal session sweep.
	orphanDirs bool
	// ignoreRunning bypasses the live-agent safety gate for every
	// session in the batch. The gate skips a session when an agent
	// process is alive in the worktree AND the worktree is dirty,
	// because removing it would silently destroy work the agent is
	// mid-edit on. --force/--purge are orthogonal: those handle the
	// committed-work-not-on-base case, this handles the in-flight
	// uncommitted case.
	ignoreRunning bool
}

type cleanupAction int

const (
	cleanupSkip cleanupAction = iota
	cleanupRemove
)

type cleanupPlan struct {
	s      *session.Session
	action cleanupAction
	reason string
}

// squashCheck reports whether all of branch's commits ahead of base are
// already on base by patch-id (i.e. squash-merged). Returning (true, nil)
// means the branch's content is on main even though git still shows it ahead.
type squashCheck func(branch string) (bool, error)

// discardCheck reports whether removing branch would lose committed work:
// the branch has commits not present on base by patch-id AND its tip tree
// differs from base. A branch tree-equal to base has had its content land
// on base by some other route (manual conflict resolution after a botched
// squash, say) and is safe to drop even if patch-id comparison disagrees.
//
// This is the parallel to squashCheck that powers the v0.5 cleanup safety
// gate: --force no longer bypasses "irrecoverable on remove" because
// silent loss of committed work is the worst outcome we ship.
type discardCheck func(branch string) (bool, error)

func planCleanup(sessions []session.Session, opts cleanupOpts, isSquashed squashCheck, wouldDiscardCommits discardCheck) ([]cleanupPlan, error) {
	plans := make([]cleanupPlan, 0, len(sessions))
	for i := range sessions {
		s := &sessions[i]

		// Liveness gate: a live agent + dirty worktree means the agent
		// is mid-edit on uncommitted work. Removing it would silently
		// destroy that work. Skip with a clear reason — the global
		// --ignore-running flag is the documented override. Runs before
		// the committed-work checks so the operator sees the liveness
		// reason instead of a confusing "would discard" message.
		if !opts.ignoreRunning && s.Running && s.Dirty > 0 {
			plans = append(plans, cleanupPlan{
				s:      s,
				action: cleanupSkip,
				reason: fmt.Sprintf("live agent (pid %d) with dirty work (use --ignore-running to override)", s.RunningPID),
			})
			continue
		}

		// Compute the danger signal once. Sessions with no commits ahead
		// can't lose anything on removal — skip the git calls entirely.
		// For the rest: patch-id check first (cheap, also gives us the
		// "squash-merged" label), then a tree compare (catches manual
		// conflict resolution after a botched squash).
		squashed := false
		discard := false
		if s.Ahead > 0 {
			sq, err := isSquashed(s.Branch)
			if err != nil {
				return nil, fmt.Errorf("check unmerged patches for %s: %w", s.Branch, err)
			}
			squashed = sq
			if !sq {
				d, err := wouldDiscardCommits(s.Branch)
				if err != nil {
					return nil, fmt.Errorf("check tree divergence for %s: %w", s.Branch, err)
				}
				discard = d
			}
		}

		// Gate: would-discard-commits is the v0.5 cleanup safety check
		// that cuts across state. --force alone is no longer enough;
		// --purge or a `bosun merge <session>` first is the recovery.
		if discard && !opts.purge {
			plans = append(plans, cleanupPlan{
				s:      s,
				action: cleanupSkip,
				reason: fmt.Sprintf("would discard %s — run `bosun merge %s` first, or pass --purge to drop it", describeWork(s), s.Name),
			})
			continue
		}

		switch {
		case s.State == session.StateDone:
			reason := "DONE"
			if discard && opts.purge {
				reason = "DONE, --purge discards " + describeWork(s)
			} else if squashed {
				reason = "DONE, squash-merged"
			}
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: reason})
		case s.State == session.StateWorking && s.Ahead == 0 && s.Dirty == 0:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "empty"})
		case s.State == session.StateWorking && s.Ahead > 0 && s.Dirty == 0:
			switch {
			case squashed:
				plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "squash-merged"})
			case discard && opts.purge:
				plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "--purge discards " + describeWork(s)})
			default:
				// !discard reaches here: unmerged > 0 but tree-equal to
				// base, i.e. the branch's content effectively landed on
				// base via some non-patch-id path. Removing loses
				// nothing.
				plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "already on base"})
			}
		case opts.purge:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "--purge, " + describeWork(s)})
		case opts.force:
			plans = append(plans, cleanupPlan{s: s, action: cleanupRemove, reason: "force-remove, " + describeWork(s)})
		default:
			plans = append(plans, cleanupPlan{s: s, action: cleanupSkip, reason: describeWork(s)})
		}
	}
	return plans, nil
}

// filterOrphans returns only those sessions whose number is greater than
// keep — the sessions that should not exist after the operator's most
// recent `bosun init N`. Named sessions (Number == 0) are passed through
// untouched: --orphans is a numeric-mode concept ("trim the trailing
// numbered sessions"), so when a named session co-exists with numbered
// ones we leave it alone unless the operator removes it explicitly.
// Caller-supplied keep < 0 is treated as a usage error upstream; here we
// trust it's non-negative.
func filterOrphans(sessions []session.Session, keep int) []session.Session {
	out := make([]session.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.Number == 0 {
			continue
		}
		if s.Number > keep {
			out = append(out, s)
		}
	}
	return out
}

func describeWork(s *session.Session) string {
	parts := make([]string, 0, 3)
	if s.Dirty > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted", s.Dirty))
	}
	if s.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d ahead", s.Ahead))
	}
	if s.State == session.StateStuck {
		parts = append(parts, "STUCK")
	}
	if len(parts) == 0 {
		return "in-progress"
	}
	return strings.Join(parts, ", ")
}

// executeCleanupOne removes one session's worktree and branch and clears
// its bosun state/claims. The caller is responsible for having decided the
// session is safe to remove (DONE / empty / squash-merged / --force) — this
// helper bypasses git's own safety gates so untracked bosun metadata
// (BOSUN_BRIEF.md, .claude/CLAUDE.md) and patch-id-equivalent branches
// don't block the removal.
func executeCleanupOne(rc *runCtx, p cleanupPlan) error {
	// v0.9: refuse to reap a parent that still has sub-sessions tracked
	// in .bosun/spawn-tree.json. The cascade requires `--tree` or the
	// children gone first; otherwise the spawn-tree's records become
	// orphans referencing a missing parent (status renders them in the
	// orphan-pass at the bottom — usable, but not what the operator
	// intended).
	children, _ := spawntree.NewStore(rc.repoRoot).ChildrenOf(p.s.Name)
	if len(children) > 0 {
		return userErr("session %s has %d live sub-session(s): %s\n"+
			"       reap them first via `bosun cleanup --tree %s` (cascades),\n"+
			"       or individually before retrying this command",
			p.s.Name, len(children), strings.Join(children, ", "), p.s.Name)
	}

	forceWT := true
	forceBranch := true
	if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, p.s.Path, forceWT); err != nil {
		return gitErr("remove worktree "+p.s.Path, err)
	}
	if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, p.s.Branch, forceBranch); err != nil {
		return gitErr("delete branch "+p.s.Branch, err)
	}
	// Clear .bosun/ metadata. We log-and-continue rather than fail the
	// cleanup: the worktree and branch (the load-bearing artifacts) are
	// already gone, so any subsequent `bosun status` won't show the
	// session. But surface the failure (via clearSessionMetadata) so the
	// operator knows to investigate — silently swallowed errors here
	// meant a permission-denied claims dir could leave stale entries
	// forever.
	clearSessionMetadata(rc, p.s.Name)
	// Also drop the session from the spawn tree so quotas and renderers
	// stop reporting it. Best-effort: a missing tree file is fine.
	_ = spawntree.NewStore(rc.repoRoot).Remove(p.s.Name)
	return nil
}

// cleanupOne plans and (unless dry-run) executes cleanup for one session.
// Used by the TUI control center's `c` keybind so it doesn't reimplement
// the policy from runCleanup. Returns the action taken and a short reason.
// err is non-nil only for unexpected git failures.
func cleanupOne(rc *runCtx, s *session.Session, opts cleanupOpts) (cleanupAction, string, error) {
	isSquashed, wouldDiscard := buildCleanupChecks(rc)
	plans, err := planCleanup([]session.Session{*s}, opts, isSquashed, wouldDiscard)
	if err != nil {
		return cleanupSkip, "", gitErr("plan cleanup", err)
	}
	p := plans[0]
	if p.action == cleanupSkip {
		return cleanupSkip, p.reason, nil
	}
	if opts.dryRun {
		return cleanupRemove, p.reason, nil
	}
	if err := executeCleanupOne(rc, p); err != nil {
		return cleanupSkip, "", err
	}
	return cleanupRemove, p.reason, nil
}

// buildCleanupChecks wires the squashCheck + discardCheck callbacks used by
// planCleanup against the live git client. Pulled out so cleanupOne and
// runCleanup share the same implementation — including the wouldDiscard
// "tree-equal counts as safe" rule.
func buildCleanupChecks(rc *runCtx) (squashCheck, discardCheck) {
	isSquashed := func(branch string) (bool, error) {
		unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, branch)
		if err != nil {
			return false, err
		}
		return unmerged == 0, nil
	}
	wouldDiscard := func(branch string) (bool, error) {
		unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, branch)
		if err != nil {
			return false, err
		}
		if unmerged == 0 {
			return false, nil
		}
		treeEqual, err := rc.git.TreeEqualsBase(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, branch)
		if err != nil {
			return false, err
		}
		return !treeEqual, nil
	}
	return isSquashed, wouldDiscard
}

func runCleanup(cmd *cobra.Command, opts cleanupOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	// Drop git metadata for worktrees whose directory has been manually
	// removed (e.g. `rm -rf` on a session dir). Without this, Derive
	// would skip the stale entries (good for status) but the on-disk
	// admin files would linger until the user notices. Doing it here
	// keeps cleanup's promise of "nothing left behind."
	if err := rc.git.PruneWorktrees(rc.ctx, rc.repoRoot); err != nil {
		return gitErr("prune worktrees", err)
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	if len(sessions) == 0 && !opts.orphanDirs {
		println("bosun: no sessions to clean up")
		return nil
	}

	if opts.orphansMode {
		keep := opts.orphansKeep
		if keep == 0 {
			keep = rc.cfg.DefaultSessionCount
		}
		if keep < 0 {
			return userErr("--orphans must be >= 0, got %d", keep)
		}
		sessions = filterOrphans(sessions, keep)
		if len(sessions) == 0 && !opts.orphanDirs {
			printf("bosun: no sessions beyond session-%d to clean up\n", keep)
			return nil
		}
		// Orphan candidates with no work are removed cleanly; ones with
		// ahead/dirty/STUCK state stay subject to the same --force gate the
		// regular cleanup uses. The operator can re-run with --force to
		// nuke them if they really want.
	}

	isSquashed, wouldDiscard := buildCleanupChecks(rc)
	plans, err := planCleanup(sessions, opts, isSquashed, wouldDiscard)
	if err != nil {
		return gitErr("plan cleanup", err)
	}

	// Count what the planner staged so pre-cleanup hooks can know whether
	// this invocation is actually going to remove anything before the work
	// starts. Orphan-dirs candidates aren't included — they're a separate
	// sweep below and we don't run their scan twice just for the hook env.
	plannedRemovals := 0
	for _, p := range plans {
		if p.action == cleanupRemove {
			plannedRemovals++
		}
	}

	// Fire pre-cleanup once per invocation (not per session — operators
	// want one Slack message, not five). Skipped in dry-run so the preview
	// path stays side-effect-free. A non-fail-open pre-cleanup error
	// aborts before any worktree is touched.
	if !opts.dryRun {
		preEnv := map[string]string{
			"BOSUN_REPO_ROOT":      rc.repoRoot,
			"BOSUN_CLEANUP_COUNT":  strconv.Itoa(plannedRemovals),
			"BOSUN_CLEANUP_REASON": cleanupReason(opts),
		}
		if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "pre-cleanup", preEnv); err != nil {
			return userErr("%v", err)
		}
	}

	removed, skipped := 0, 0
	for _, p := range plans {
		if p.action == cleanupSkip {
			skipped++
			printf("  ⏭ %s: skipped — %s\n", p.s.Name, p.reason)
			continue
		}
		if opts.dryRun {
			removed++
			printf("  ▸ %s: would remove (%s)\n", p.s.Name, p.reason)
			continue
		}
		if err := executeCleanupOne(rc, p); err != nil {
			return err
		}
		removed++
		printf("  ✓ %s: removed (%s)\n", p.s.Name, p.reason)
	}

	if opts.orphanDirs {
		r, s, err := sweepOrphanDirs(rc, opts.dryRun)
		if err != nil {
			return err
		}
		removed += r
		skipped += s
	}

	if opts.dryRun {
		printf("\nbosun: dry-run — would remove %d, skip %d (no changes made)\n", removed, skipped)
	} else {
		printf("\nbosun: removed %d, skipped %d\n", removed, skipped)
	}

	// post-cleanup is non-fatal: by the time we'd report a failure the
	// worktrees are gone and aborting wouldn't undo anything. Drop to a
	// warning so the operator can see the hook misbehaved without losing
	// the cleanup signal. Reason mirrors pre-cleanup so operators can
	// filter by it in Slack/etc.
	if !opts.dryRun {
		postEnv := map[string]string{
			"BOSUN_REPO_ROOT":      rc.repoRoot,
			"BOSUN_CLEANUP_COUNT":  strconv.Itoa(removed),
			"BOSUN_CLEANUP_REASON": cleanupReason(opts),
		}
		if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "post-cleanup", postEnv); err != nil {
			printf("bosun: warning: post-cleanup hook: %v\n", err)
		}
	}
	return nil
}

// cleanupReason classifies the invocation for the hook env. Operators
// wiring `pre-cleanup` over Slack want to distinguish "manual sweep"
// from "post-init shrink trim" from "v0.3 corruption recovery." The
// flags compose, but reporting one canonical reason avoids ambiguity in
// the hook command. Priority: orphans-mode > orphan-dirs-mode > manual.
func cleanupReason(opts cleanupOpts) string {
	switch {
	case opts.orphansMode:
		return "orphans-mode"
	case opts.orphanDirs:
		return "orphan-dirs-mode"
	default:
		return "manual"
	}
}

// sweepOrphanDirs implements the --orphan-dirs path: scan the repo's
// parent directory for sibling dirs matching cfg.WorktreeSuffixPattern,
// drop any that git's worktree list still tracks, then for each
// remaining candidate either remove it (chmod + RemoveAll) or skip it
// with a "looks like a live worktree" notice when the dir still
// carries a `.git` file pointing at the main repo. Returns the
// removed/skipped counts so runCleanup can fold them into the summary.
func sweepOrphanDirs(rc *runCtx, dryRun bool) (removed, skipped int, err error) {
	candidates, err := git.ScanOrphanDirs(rc.repoRoot, rc.cfg.WorktreeSuffixPattern)
	if err != nil {
		return 0, 0, gitErr("scan orphan dirs", err)
	}
	if len(candidates) == 0 {
		return 0, 0, nil
	}

	worktrees, err := rc.git.ListWorktrees(rc.ctx, rc.repoRoot)
	if err != nil {
		return 0, 0, gitErr("list worktrees", err)
	}
	tracked := make(map[string]bool, len(worktrees))
	for _, w := range worktrees {
		tracked[w.Path] = true
	}

	for _, dir := range candidates {
		if tracked[dir] {
			// git still considers it a real worktree — handled by the
			// normal session sweep, not here.
			continue
		}
		display := filepath.Base(dir)
		if looksLikeLiveWorktree(dir) {
			skipped++
			printf("  ⏭ %s: skipped (looks like a live worktree; run `git worktree prune` first)\n", display)
			continue
		}
		if dryRun {
			removed++
			printf("  ▸ %s: would remove (orphan dir)\n", display)
			continue
		}
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			_ = git.ChmodWritableTree(dir)
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return removed, skipped, fmt.Errorf("remove orphan dir %s: %w", dir, rmErr)
		}
		removed++
		printf("  ✓ %s: removed (orphan dir)\n", display)
	}
	return removed, skipped, nil
}

// looksLikeLiveWorktree reports whether dir has a `.git` *file* (not
// directory) whose content begins with `gitdir:` — the on-disk shape of
// a linked worktree's pointer back to the main repo's .git admin tree.
// True means we should refuse to remove it: the safer course is to ask
// the operator to run `git worktree prune` (or `repair`) first so git's
// view of the tree matches the disk.
func looksLikeLiveWorktree(dir string) bool {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir:")
}

// runCleanupTree cascades cleanup through a spawn-tree starting at
// parentLabel. Reaps in dependency order — leaves first, root last —
// so each removal sees a tree with no live children below it (the
// spawn-tree refusal in executeCleanupOne never fires for the cascade
// itself).
//
// Each session in the subtree goes through the normal cleanup gates:
// liveness check, dirty/ahead policy, --force / --purge / --ignore-
// running pass-through. A skip mid-cascade is non-fatal — the rest of
// the subtree proceeds and the skipped session is reported. A real
// git failure aborts.
//
// Mutually exclusive with --orphans (validated at the flag layer).
func runCleanupTree(cmd *cobra.Command, parentLabel string, opts cleanupOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	parsed, err := session.ParseLabel(parentLabel)
	if err != nil {
		return err
	}
	tree := spawntree.NewStore(rc.repoRoot)

	// Reconcile the spawn tree against git BEFORE planning the
	// cascade so the walk doesn't try to reap entries whose worktree
	// and branch are already gone (trial #3c's ghost-children shape).
	// Failure is non-fatal — falling through to the cascade still
	// works, the operator just sees the confusing "no such worktree"
	// error from the underlying git call.
	if pruned, err := tree.SyncWithGit(rc.ctx, rc.git, rc.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: spawn-tree sync: %v\n", err)
	} else if len(pruned) > 0 {
		fmt.Fprintf(os.Stderr, "bosun: pruned %d ghost spawn-tree entr%s before cascade: %s\n",
			len(pruned), pluralEntries(len(pruned)), strings.Join(pruned, ", "))
	}

	// Build the post-order walk: for each subtree, emit descendants
	// before the root. Iterative + visited-set in case the tree on
	// disk has a self-reference cycle (shouldn't happen, but cheap
	// to defend against).
	order, err := postOrderSubtree(tree, parsed)
	if err != nil {
		return internalErr("walk spawn tree", err)
	}
	if len(order) == 0 {
		return userErr("session %s is not in the spawn tree (no record); nothing to cascade", parsed)
	}

	// Derive the live session list once so each iteration uses the
	// same snapshot — saves a `git worktree list` per session.
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	byName := map[string]*session.Session{}
	for i := range sessions {
		byName[sessions[i].Name] = &sessions[i]
	}

	var (
		reaped  []string
		skipped []string
	)
	for _, label := range order {
		s, ok := byName[label]
		if !ok {
			// In the spawn tree but no worktree — already reaped or
			// never fully created. Drop from the tree quietly.
			if !opts.dryRun {
				_ = tree.Remove(label)
			}
			continue
		}
		if opts.dryRun {
			fmt.Fprintf(os.Stdout, "would cleanup %s\n", label)
			reaped = append(reaped, label)
			continue
		}
		action, reason, err := cleanupOne(rc, s, opts)
		if err != nil {
			return err
		}
		switch action {
		case cleanupRemove:
			reaped = append(reaped, label)
		case cleanupSkip:
			skipped = append(skipped, label+" ("+reason+")")
		}
	}

	fmt.Fprintf(os.Stdout, "tree cleanup for %s: %d reaped, %d skipped\n",
		parsed, len(reaped), len(skipped))
	for _, s := range skipped {
		fmt.Fprintf(os.Stdout, "  skipped: %s\n", s)
	}
	return nil
}

// postOrderSubtree returns labels in dependency order — children
// before parent, depth-first. Visited-set prevents infinite loops if
// the on-disk tree somehow contains a cycle.
func postOrderSubtree(tree *spawntree.Store, root string) ([]string, error) {
	visited := map[string]bool{}
	var out []string
	var walk func(label string) error
	walk = func(label string) error {
		if visited[label] {
			return nil
		}
		visited[label] = true
		kids, err := tree.ChildrenOf(label)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if err := walk(k); err != nil {
				return err
			}
		}
		out = append(out, label)
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}
