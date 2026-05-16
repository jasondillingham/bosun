package main

import (
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newMergeCmd() *cobra.Command {
	var (
		all      bool
		noSquash bool
		message  string
	)

	cmd := &cobra.Command{
		Use:   "merge [<session>...]",
		Short: "Squash-merge sessions back to the base branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMerge(cmd, args, mergeOpts{
				all:      all,
				noSquash: noSquash,
				message:  message,
			})
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "attempt every session, not just DONE")
	cmd.Flags().BoolVar(&noSquash, "no-squash", false, "use --no-ff merges instead of --squash")
	cmd.Flags().StringVarP(&message, "message", "m", "", "override the commit message")

	return cmd
}

type mergeOpts struct {
	all      bool
	noSquash bool
	message  string
}

func runMerge(cmd *cobra.Command, args []string, opts mergeOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	// Refuse if HEAD isn't on the base branch.
	currentBranch, err := rc.git.CurrentBranch(rc.ctx, rc.repoRoot)
	if err != nil {
		return gitErr("read current branch", err)
	}
	if currentBranch != rc.cfg.BaseBranch {
		return userErr("merge must run on base branch %q (HEAD is on %q)", rc.cfg.BaseBranch, currentBranch)
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	// Build the candidate list.
	var requested map[int]bool
	if len(args) > 0 {
		requested = map[int]bool{}
		for _, a := range args {
			n, err := session.ParseName(a)
			if err != nil {
				return userErr("%v", err)
			}
			requested[n] = true
		}
	}

	// Dependency-aware ordering: re-parse the archived plan to pick up
	// any `## session-N (depends: session-M)` declarations. Missing or
	// unparseable plan → empty map (no-deps fallback). Within the loop
	// we skip a session whose declared deps aren't merged yet.
	depMap, err := brief.LoadArchivedDeps(rc.repoRoot)
	if err != nil {
		return internalErr("load archived deps", err)
	}
	sessions = topoOrderForMerge(sessions, depMap)
	mergedThisRun := make(map[int]bool, len(sessions))

	type result struct {
		name   string
		status string // "merged", "skipped", "conflict"
		reason string
	}
	var results []result
	conflictHit := false

	for _, s := range sessions {
		if requested != nil && !requested[s.Number] {
			continue
		}
		if !opts.all && requested == nil && s.State != session.StateDone {
			results = append(results, result{name: s.Name, status: mergeStatusSkipped, reason: "not marked DONE (use --all to override)"})
			continue
		}
		// Hold this session if any dependency hasn't merged yet — either in
		// this run or as a previously-merged session whose branch is now
		// patch-equivalent to base. We check per-dep so the reason names the
		// blocker the operator needs to resolve.
		if blocker, ok := blockingDep(s.Number, depMap, sessions, mergedThisRun, rc); ok {
			results = append(results, result{name: s.Name, status: mergeStatusSkipped, reason: fmt.Sprintf("depends on session-%d (not merged yet)", blocker)})
			continue
		}

		status, reason, err := mergeOne(rc, &s, opts)
		if err != nil {
			return err
		}
		results = append(results, result{name: s.Name, status: status, reason: reason})
		if status == mergeStatusConflict {
			conflictHit = true
			break
		}
		if status == mergeStatusMerged {
			mergedThisRun[s.Number] = true
		}
	}

	// Print summary.
	for _, r := range results {
		mark := "✓"
		if r.status == mergeStatusSkipped {
			mark = "⏭"
		} else if r.status == mergeStatusConflict {
			mark = "✗"
		}
		printf("  %s %s: %s — %s\n", mark, r.name, r.status, r.reason)
	}
	if conflictHit {
		println("\nbosun: stopped at first conflict. Resolve, commit, then re-run `bosun merge`.")
	}
	return nil
}

// Status constants returned by mergeOne. Exposed so callers (cmd_merge,
// the TUI control center) can branch on the outcome without duplicating
// string literals.
const (
	mergeStatusMerged   = "merged"
	mergeStatusSkipped  = "skipped"
	mergeStatusConflict = "conflict"
)

// mergeOne performs the merge for a single session and reports the outcome.
// It enforces the per-session safety gates (dirty, ahead, patch-equivalence)
// and runs the squash or no-ff merge. The outer loop is responsible for the
// gates it owns (DONE filtering, dependency holds) before calling.
//
// status is one of mergeStatus{Merged,Skipped,Conflict}. err is non-nil
// only for unexpected git failures; safety-gate skips and merge conflicts
// are reported via status so the TUI can render them without dying.
func mergeOne(rc *runCtx, s *session.Session, opts mergeOpts) (status, reason string, err error) {
	if s.Dirty > 0 {
		return mergeStatusSkipped, fmt.Sprintf("%d uncommitted change(s)", s.Dirty), nil
	}
	if s.Ahead == 0 {
		return mergeStatusSkipped, "no commits ahead", nil
	}

	// If the branch's commits are all patch-id-equivalent to commits already
	// on base (e.g. an operator hand-resolved a prior conflict and committed),
	// don't try to squash again — that would just re-conflict. Treat as
	// merged and clear state/claims so it stops cluttering `bosun status`.
	unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, s.Branch)
	if err != nil {
		return "", "", gitErr("check unmerged patches for "+s.Branch, err)
	}
	if unmerged == 0 {
		_ = rc.state.Clear(s.Name)
		_ = rc.claims.Clear(s.Name)
		return mergeStatusSkipped, "already merged", nil
	}

	commitMsg := opts.message
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("merge: %s", s.Branch)
	}

	if opts.noSquash {
		if err := rc.git.MergeNoFF(rc.ctx, rc.repoRoot, s.Branch, commitMsg); err != nil {
			if isMergeConflict(err) {
				return mergeStatusConflict, "merge conflict — resolve manually then commit", nil
			}
			return "", "", gitErr("merge --no-ff "+s.Branch, err)
		}
	} else {
		if err := rc.git.MergeSquash(rc.ctx, rc.repoRoot, s.Branch); err != nil {
			if isMergeConflict(err) {
				return mergeStatusConflict, "merge conflict — resolve manually then commit", nil
			}
			return "", "", gitErr("merge --squash "+s.Branch, err)
		}
		// `git merge --squash` may leave the index empty when the branch's
		// tree already matches base (e.g. operator hand-resolved earlier).
		// Patch-ids differ so UnmergedPatches doesn't catch this, but the
		// merge staged nothing — treat as already-merged.
		staged, err := rc.git.DirtyCount(rc.ctx, rc.repoRoot)
		if err != nil {
			return "", "", gitErr("check staged after squash", err)
		}
		if staged == 0 {
			_ = rc.state.Clear(s.Name)
			_ = rc.claims.Clear(s.Name)
			return mergeStatusSkipped, "already merged", nil
		}
		if err := rc.git.Commit(rc.ctx, rc.repoRoot, commitMsg); err != nil {
			return "", "", gitErr("commit merged squash", err)
		}
	}

	_ = rc.state.Clear(s.Name)
	_ = rc.claims.Clear(s.Name)
	return mergeStatusMerged, fmt.Sprintf("%d commit(s) squashed", s.Ahead), nil
}

// isMergeConflict heuristically detects whether an err message indicates a merge conflict.
// git returns non-zero exit + a message containing "CONFLICT" or "Automatic merge failed".
func isMergeConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") || strings.Contains(msg, "automatic merge failed")
}

// topoOrderForMerge returns sessions reordered so that any session listed
// as a dependency of another comes first. Sessions outside the dep graph
// keep their original (numeric) position. Cycles fall back to the input
// order — bosun merge will still skip dependent sessions until their
// blockers clear, so a cycle just means none of the cyclic group can
// progress, which the operator will see in the output.
func topoOrderForMerge(sessions []session.Session, depMap map[int][]int) []session.Session {
	if len(depMap) == 0 {
		return sessions
	}
	indexOf := make(map[int]int, len(sessions))
	for i, s := range sessions {
		indexOf[s.Number] = i
	}
	out := make([]session.Session, 0, len(sessions))
	visited := make(map[int]bool, len(sessions))
	visiting := make(map[int]bool, len(sessions))

	var visit func(n int)
	visit = func(n int) {
		if visited[n] || visiting[n] {
			return
		}
		visiting[n] = true
		for _, dep := range depMap[n] {
			if _, ok := indexOf[dep]; ok {
				visit(dep)
			}
		}
		visiting[n] = false
		if i, ok := indexOf[n]; ok {
			out = append(out, sessions[i])
			visited[n] = true
		}
	}
	for _, s := range sessions {
		visit(s.Number)
	}
	return out
}

// blockingDep returns the first unmerged dependency of session n, if any.
// "Merged" means: merged earlier in this run, OR the session no longer
// exists in the candidate list (already cleaned up), OR its branch is
// patch-equivalent to base (zero unmerged patches via git cherry).
func blockingDep(n int, depMap map[int][]int, sessions []session.Session, mergedThisRun map[int]bool, rc *runCtx) (int, bool) {
	deps, ok := depMap[n]
	if !ok || len(deps) == 0 {
		return 0, false
	}
	byNumber := make(map[int]*session.Session, len(sessions))
	for i := range sessions {
		byNumber[sessions[i].Number] = &sessions[i]
	}
	for _, d := range deps {
		if mergedThisRun[d] {
			continue
		}
		dep, present := byNumber[d]
		if !present {
			// Dep session no longer exists — assume operator cleaned it up
			// after a successful merge.
			continue
		}
		// Patch-equivalent to base counts as merged even if `ahead` > 0.
		unmerged, err := rc.git.UnmergedPatches(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, dep.Branch)
		if err == nil && unmerged == 0 {
			continue
		}
		return d, true
	}
	return 0, false
}

