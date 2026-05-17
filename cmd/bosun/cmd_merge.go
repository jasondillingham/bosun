package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/hooks"
	"github.com/jasondillingham/bosun/internal/preflight"
	"github.com/jasondillingham/bosun/internal/proc"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

// procRunning is the live-agent detector used by the merge gate. Indirected
// through a package-level var so unit tests can stub out detection without
// spawning a real `claude` subprocess; scenario tests still exercise the
// real proc.Running path by planting an executable named "claude" in a
// worktree.
var procRunning = proc.Running

func newMergeCmd() *cobra.Command {
	var (
		all           bool
		noSquash      bool
		dryRun        bool
		message       string
		ignoreRunning bool
		undoID        string
		listUndo      bool
		noLoadCheck   bool
	)

	cmd := &cobra.Command{
		Use:   "merge [<session>...]",
		Short: "Squash-merge sessions back to the base branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMerge(cmd, args, mergeOpts{
				all:           all,
				noSquash:      noSquash,
				dryRun:        dryRun,
				message:       message,
				ignoreRunning: ignoreRunning,
				undoID:        undoID,
				listUndo:      listUndo,
				noLoadCheck:   noLoadCheck,
			})
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "attempt every session, not just DONE")
	cmd.Flags().BoolVar(&noSquash, "no-squash", false, "use --no-ff merges instead of --squash")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen, don't act")
	cmd.Flags().StringVarP(&message, "message", "m", "", "override the commit message")
	cmd.Flags().BoolVar(&ignoreRunning, "ignore-running", false, "bypass the live-agent gate (drops untracked + unstaged work)")
	cmd.Flags().StringVar(&undoID, "undo", "", "undo a recent merge by session name or pre-SHA prefix")
	cmd.Flags().BoolVar(&listUndo, "list-undo", false, "list recent merge log entries for inspection")
	cmd.Flags().BoolVar(&noLoadCheck, "no-load-check", false, "skip the pre-flight 1-minute load average check")

	return cmd
}

type mergeOpts struct {
	all           bool
	noSquash      bool
	dryRun        bool
	message       string
	ignoreRunning bool
	undoID        string
	listUndo      bool
	noLoadCheck   bool
}

func runMerge(cmd *cobra.Command, args []string, opts mergeOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	// --list-undo and --undo run before the on-base-branch check is
	// strictly necessary (list is read-only; undo enforces base-branch
	// itself). Keep them in this file so cmd_merge owns the full undo
	// surface.
	if opts.listUndo {
		return runMergeListUndo(rc)
	}
	if opts.undoID != "" {
		return runMergeUndo(rc, opts.undoID)
	}

	// Refuse if HEAD isn't on the base branch.
	currentBranch, err := rc.git.CurrentBranch(rc.ctx, rc.repoRoot)
	if err != nil {
		return gitErr("read current branch", err)
	}
	if currentBranch != rc.cfg.BaseBranch {
		return userErr("merge must run on base branch %q (HEAD is on %q)", rc.cfg.BaseBranch, currentBranch)
	}

	// Pre-flight: 1-min load average advisory. Merge's pre-merge fsck is
	// exactly the operation that hung for 11 minutes in v0.6.1 under
	// fsync pressure — same shape as init, same warning surface, same
	// --no-load-check escape hatch.
	if !opts.noLoadCheck {
		preflight.CheckLoad(os.Stdout, "merge", preflight.DefaultLoadWarnThreshold, preflight.DefaultLoadAveragePauseDuration)
	}

	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	// Build the candidate list.
	var requested map[string]bool
	if len(args) > 0 {
		requested = map[string]bool{}
		for _, a := range args {
			label, err := session.ParseLabel(a)
			if err != nil {
				return userErr("%v", err)
			}
			requested[label] = true
		}
	}

	// Dependency-aware ordering: re-parse the archived plan to pick up
	// any `## <label> (depends: <other-label>)` declarations. Missing or
	// unparseable plan → empty map (no-deps fallback). Within the loop
	// we skip a session whose declared deps aren't merged yet.
	depMap, err := brief.LoadArchivedDeps(rc.repoRoot)
	if err != nil {
		return internalErr("load archived deps", err)
	}
	if cycle := brief.FindDependencyCycle(depMap); cycle != nil {
		return userErr("dependency cycle detected: %s — edit the plan, run `bosun init`, then retry", strings.Join(cycle, " → "))
	}
	sessions = topoOrderForMerge(sessions, depMap)
	mergedThisRun := make(map[string]bool, len(sessions))

	type result struct {
		name   string
		status string // "merged", "skipped", "conflict"
		reason string
	}
	var results []result
	conflictHit := false

	for _, s := range sessions {
		if requested != nil && !requested[s.Label] {
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
		if blocker, ok := blockingDep(s.Label, depMap, sessions, mergedThisRun, rc); ok {
			results = append(results, result{name: s.Name, status: mergeStatusSkipped, reason: fmt.Sprintf("depends on %s (not merged yet)", blocker)})
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
		if status == mergeStatusMerged || status == mergeStatusWouldMerge {
			// Track would-merge too so a dependent session in the same dry-run
			// sees its blocker as resolved and reports its own plan accurately.
			mergedThisRun[s.Label] = true
		}
	}

	// Print summary.
	for _, r := range results {
		if r.status == mergeStatusWouldMerge {
			printf("  ▸ %s: would merge (%s)\n", r.name, r.reason)
			continue
		}
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
	mergeStatusMerged     = "merged"
	mergeStatusSkipped    = "skipped"
	mergeStatusConflict   = "conflict"
	mergeStatusWouldMerge = "would-merge"
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

	// Agent-liveness gate: refuse to squash a worktree that has both a
	// live `claude` process AND uncommitted/untracked changes. Without
	// this, `bosun merge --all` can race an in-flight edit and squash
	// only the committed half, dropping unstaged work. --ignore-running
	// bypasses for the case where the operator knows the dirty state is
	// disposable. Runs before fsck and the patch-id checks so the most
	// common in-flight scenario gets the most specific error.
	if !opts.ignoreRunning {
		if reason, refuse, gerr := agentLivenessGate(rc, s); gerr != nil {
			return "", "", gerr
		} else if refuse {
			if opts.dryRun {
				return mergeStatusSkipped, reason, nil
			}
			return "", "", userErr("%s", reason)
		}
	}

	// Pre-merge fsck: refuse on object-store corruption. fsck failure
	// means torn writes or filesystem damage; the operator must
	// investigate, not retry. No bypass flag, per the v0.6 brief.
	//
	// Runs before UnmergedPatches/TreeEqualsBase below — both of those
	// call `git cherry` / `git diff` which would themselves fail on
	// corrupt objects, but with a cryptic git-plumbing error rather
	// than the clear "fsck" diagnostic the operator needs.
	if err := rc.git.FsckWorktree(rc.ctx, s.Path); err != nil {
		return "", "", gitErr("pre-merge fsck "+s.Name, err)
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
		clearSessionMetadata(rc, s.Name)
		return mergeStatusSkipped, "already merged", nil
	}

	// Tree-equivalence: when the operator hand-resolved a prior squash
	// conflict, the patch ids on branch differ from base's squashed
	// commit, so UnmergedPatches reports unmerged > 0. But the branch's
	// tree may now equal base's tree (operator merged "theirs"), in which
	// case re-running the squash would just re-conflict against
	// already-applied content. Catch that here.
	treeEqual, err := rc.git.TreeEqualsBase(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, s.Branch)
	if err != nil {
		return "", "", gitErr("check tree-equivalence for "+s.Branch, err)
	}
	if treeEqual {
		clearSessionMetadata(rc, s.Name)
		return mergeStatusSkipped, "already merged (tree-equivalent to base)", nil
	}

	// Dry-run: all safety/dep gates passed, so a real run would actually
	// merge. Report what would happen without touching the working tree or
	// state. Conflict prediction is out of scope — this only reports the
	// plan, not the merge outcome.
	//
	// pre-merge intentionally does NOT fire on dry-run: dry-run is meant
	// to be side-effect-free, and operator hooks (Slack pings, backups,
	// preflight gates) shouldn't observe a planning run.
	if opts.dryRun {
		return mergeStatusWouldMerge, fmt.Sprintf("%d commit(s)", s.Ahead), nil
	}

	// pre-merge: fail-closed hooks block the squash for this session.
	// Surface as Skipped (not Conflict) — the merge never started, the
	// reason names the hook so the operator knows where to look.
	preEnv := mergeHookEnv(s, rc.cfg.BaseBranch)
	if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "pre-merge", preEnv); err != nil {
		return mergeStatusSkipped, fmt.Sprintf("pre-merge hook refused: %v", err), nil
	}

	// Capture main's HEAD before the merge so we can record an undo
	// anchor in .bosun/merges.log once the squash commits. Recording
	// happens after the post-merge state lands; we resolve it now so a
	// later rev-parse race doesn't confuse the recorded pre-SHA with
	// the post-SHA.
	preSHA, err := rc.git.RevParseHEAD(rc.ctx, rc.repoRoot)
	if err != nil {
		return "", "", gitErr("read pre-merge HEAD", err)
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
			clearSessionMetadata(rc, s.Name)
			return mergeStatusSkipped, "already merged", nil
		}
		if err := rc.git.Commit(rc.ctx, rc.repoRoot, commitMsg); err != nil {
			return "", "", gitErr("commit merged squash", err)
		}
	}

	// post-merge fires after the squash commit lands. Failures are
	// non-fatal: a notification or backup hook misfiring shouldn't unwind
	// a clean merge. Routed through rc.git.RevParseHEAD so the configured
	// per-op timeout applies — pre-v0.6.2 this shelled out directly and
	// could hang indefinitely under fsync pressure.
	postSHA, _ := rc.git.RevParseHEAD(rc.ctx, rc.repoRoot)
	postEnv := mergeHookEnv(s, rc.cfg.BaseBranch)
	if postSHA != "" {
		postEnv["BOSUN_MERGE_COMMIT"] = postSHA
	}

	// Record the merge in .bosun/merges.log so `bosun merge --undo` has
	// a pre/post anchor. We only record once both SHAs are known and
	// pre != post — defensive against the (unexpected) ff-no-op case.
	// Logging failure is non-fatal: a write error here shouldn't unwind
	// a clean merge.
	if preSHA != "" && postSHA != "" && preSHA != postSHA {
		if err := appendMergeLog(rc.repoRoot, mergeLogEntry{
			TS:        time.Now().UTC().Format(time.RFC3339),
			Session:   s.Name,
			Pre:       preSHA,
			Post:      postSHA,
			SquashMsg: truncateForMergeLog(commitMsg),
		}); err != nil {
			printf("bosun: warning: record merge log: %v\n", err)
		}
	}

	if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "post-merge", postEnv); err != nil {
		printf("bosun: warning: post-merge hook: %v\n", err)
	}

	clearSessionMetadata(rc, s.Name)
	return mergeStatusMerged, fmt.Sprintf("%d commit(s) squashed", s.Ahead), nil
}

// clearSessionMetadata wipes the .bosun/{state,claims}/<name>.* files
// after a session has either merged or been determined already-merged.
// Failures are non-fatal (the load-bearing git side already succeeded)
// but they get logged so a permission-denied or filesystem-gone error
// doesn't silently leave stale entries that confuse `bosun status`
// forever after.
func clearSessionMetadata(rc *runCtx, name string) {
	if err := rc.state.Clear(name); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: clear state for %s: %v\n", name, err)
	}
	if err := rc.claims.Clear(name); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: clear claims for %s: %v\n", name, err)
	}
}

// mergeHookEnv returns the env injected into pre-merge and post-merge
// hooks for a given session. Kept in one place so the two call-sites
// stay in sync; post-merge layers BOSUN_MERGE_COMMIT on top.
func mergeHookEnv(s *session.Session, baseBranch string) map[string]string {
	return map[string]string{
		"BOSUN_SESSION":       s.Name,
		"BOSUN_TARGET_BRANCH": baseBranch,
		"BOSUN_BRANCH":        s.Branch,
		"BOSUN_AHEAD":         strconv.Itoa(s.Ahead),
	}
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
// keep their original (numeric/named) position. Callers must run
// brief.FindDependencyCycle first — this function assumes an acyclic
// dep graph and silently falls back to input order if one exists.
func topoOrderForMerge(sessions []session.Session, depMap map[string][]string) []session.Session {
	if len(depMap) == 0 {
		return sessions
	}
	indexOf := make(map[string]int, len(sessions))
	for i, s := range sessions {
		indexOf[s.Label] = i
	}
	out := make([]session.Session, 0, len(sessions))
	visited := make(map[string]bool, len(sessions))
	visiting := make(map[string]bool, len(sessions))

	var visit func(label string)
	visit = func(label string) {
		if visited[label] || visiting[label] {
			return
		}
		visiting[label] = true
		for _, dep := range depMap[label] {
			if _, ok := indexOf[dep]; ok {
				visit(dep)
			}
		}
		visiting[label] = false
		if i, ok := indexOf[label]; ok {
			out = append(out, sessions[i])
			visited[label] = true
		}
	}
	for _, s := range sessions {
		visit(s.Label)
	}
	return out
}

// blockingDep returns the first unmerged dependency of the given session
// label, if any. "Merged" means: merged earlier in this run, OR the session
// no longer exists in the candidate list (already cleaned up), OR its
// branch is patch-equivalent to base (zero unmerged patches via git cherry).
func blockingDep(label string, depMap map[string][]string, sessions []session.Session, mergedThisRun map[string]bool, rc *runCtx) (string, bool) {
	deps, ok := depMap[label]
	if !ok || len(deps) == 0 {
		return "", false
	}
	byLabel := make(map[string]*session.Session, len(sessions))
	for i := range sessions {
		byLabel[sessions[i].Label] = &sessions[i]
	}
	for _, d := range deps {
		if mergedThisRun[d] {
			continue
		}
		dep, present := byLabel[d]
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
		// Tree-equivalent to base also counts as merged — covers the
		// hand-resolved-squash case where patch ids no longer match.
		treeEqual, terr := rc.git.TreeEqualsBase(rc.ctx, rc.repoRoot, rc.cfg.BaseBranch, dep.Branch)
		if terr == nil && treeEqual {
			continue
		}
		return d, true
	}
	return "", false
}

// agentLivenessGate evaluates the merge-time refusal condition: a live
// agent process rooted in the worktree AND any uncommitted dirty files
// (modified or untracked). Returns (reason, true, nil) when the merge
// should be refused; (_, false, nil) when it's safe to proceed; a non-nil
// error only on unexpected git failures during the status check.
//
// The gate exists to keep `bosun merge --all` from quietly squashing the
// committed half of an in-flight edit and dropping the unstaged half.
// Operators who actually intend that behavior pass `--ignore-running`.
func agentLivenessGate(rc *runCtx, s *session.Session) (string, bool, error) {
	pid, running, _ := procRunning(s.Path)
	if !running {
		return "", false, nil
	}
	// git status --porcelain reports both modified-but-staged/unstaged
	// (` M`, `M `, `A `) and untracked (`??`) entries. Either is enough
	// to refuse — dropping an untracked file is just as bad as dropping
	// an unstaged edit.
	statusLines, err := rc.git.Status(rc.ctx, s.Path)
	if err != nil {
		return "", false, gitErr("status "+s.Branch, err)
	}
	if len(statusLines) == 0 {
		return "", false, nil
	}
	reason := fmt.Sprintf(
		"%s has a live agent (pid %d) and uncommitted changes — refusing merge\n"+
			"       to avoid dropping in-flight work. Recovery:\n"+
			"       - finish the agent's edit and `bosun done` from inside the session, OR\n"+
			"       - bosun merge %s --ignore-running (drops untracked + unstaged work)",
		s.Name, pid, s.Name)
	return reason, true, nil
}

// mergeLogEntry is one row of .bosun/merges.log. Stored as JSON Lines so
// the file is human-greppable and append-only — each successful merge
// adds one line, and `bosun merge --undo` reads them newest-first.
type mergeLogEntry struct {
	TS        string `json:"ts"`
	Session   string `json:"session"`
	Pre       string `json:"pre"`
	Post      string `json:"post"`
	SquashMsg string `json:"squash_msg"`
}

// truncateForMergeLog caps the squash_msg field at one line, ~80 chars,
// so a long --message doesn't blow up the log's per-line readability.
func truncateForMergeLog(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 80
	if len(s) > max {
		s = s[:max-3] + "..."
	}
	return s
}

// mergeLogPath returns the absolute path to .bosun/merges.log under
// repoRoot. The directory itself is created on demand by appendMergeLog;
// readers must tolerate a missing file (fresh repo, never merged).
func mergeLogPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".bosun", "merges.log")
}

// appendMergeLog appends one entry as a single JSON line. .bosun/ is
// created if missing (matches the contract of state/claims storage in
// the same directory).
func appendMergeLog(repoRoot string, entry mergeLogEntry) error {
	bosunDir := filepath.Join(repoRoot, ".bosun")
	if err := os.MkdirAll(bosunDir, 0o755); err != nil {
		return fmt.Errorf("mkdir .bosun: %w", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal merge log entry: %w", err)
	}
	f, err := os.OpenFile(mergeLogPath(repoRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open merges.log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write merges.log: %w", err)
	}
	return nil
}

// readMergeLog returns all entries in append order (oldest first). A
// missing file yields (nil, nil) — that's the fresh-repo case, not an
// error. Malformed lines are skipped silently so a hand-edit doesn't
// brick `bosun merge --list-undo`.
func readMergeLog(repoRoot string) ([]mergeLogEntry, error) {
	data, err := os.ReadFile(mergeLogPath(repoRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []mergeLogEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e mergeLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// findMergeEntry locates the newest entry matching mergeID. Match order:
// exact session name first, then SHA prefix on pre OR post (≥4 chars,
// short-SHA territory). Returns the matching entry by value.
func findMergeEntry(entries []mergeLogEntry, mergeID string) (mergeLogEntry, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Session == mergeID {
			return e, true
		}
	}
	if len(mergeID) >= 4 {
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if strings.HasPrefix(e.Pre, mergeID) || strings.HasPrefix(e.Post, mergeID) {
				return e, true
			}
		}
	}
	return mergeLogEntry{}, false
}

// runMergeUndo implements `bosun merge --undo <merge-id>`. It refuses
// unless HEAD is on the base branch (we're about to reset --hard, which
// only makes sense on the branch the merge landed on) and unless main's
// current HEAD still equals the recorded post-SHA — if anything has
// landed since, the operator needs `git reflog`, not us.
func runMergeUndo(rc *runCtx, mergeID string) error {
	currentBranch, err := rc.git.CurrentBranch(rc.ctx, rc.repoRoot)
	if err != nil {
		return gitErr("read current branch", err)
	}
	if currentBranch != rc.cfg.BaseBranch {
		return userErr("merge --undo must run on base branch %q (HEAD is on %q)", rc.cfg.BaseBranch, currentBranch)
	}

	entries, err := readMergeLog(rc.repoRoot)
	if err != nil {
		return internalErr("read merge log", err)
	}
	if len(entries) == 0 {
		return userErr("no merge history recorded (.bosun/merges.log empty or missing)")
	}

	entry, ok := findMergeEntry(entries, mergeID)
	if !ok {
		return userErr("no merge log entry matching %q (run `bosun merge --list-undo` to see recent merges)", mergeID)
	}

	curHead, err := rc.git.RevParseHEAD(rc.ctx, rc.repoRoot)
	if err != nil {
		return gitErr("read HEAD", err)
	}
	if curHead != entry.Post {
		return userErr("main has moved past %s's merge (HEAD=%s, recorded post=%s); recover via `git reflog`",
			entry.Session, shortSHA(curHead), shortSHA(entry.Post))
	}

	if err := rc.git.ResetHard(rc.ctx, rc.repoRoot, entry.Pre); err != nil {
		return gitErr("reset --hard "+shortSHA(entry.Pre), err)
	}
	printf("bosun: undid merge of %s (was at %s, now at %s)\n", entry.Session, shortSHA(entry.Post), shortSHA(entry.Pre))
	return nil
}

// runMergeListUndo prints the merge log newest-first for operator
// inspection. Missing log file → "(no merge history)" rather than an
// error; nothing to undo isn't a failure mode.
func runMergeListUndo(rc *runCtx) error {
	entries, err := readMergeLog(rc.repoRoot)
	if err != nil {
		return internalErr("read merge log", err)
	}
	if len(entries) == 0 {
		println("(no merge history)")
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		printf("  %s  %-14s  %s..%s  %s\n", e.TS, e.Session, shortSHA(e.Pre), shortSHA(e.Post), e.SquashMsg)
	}
	return nil
}

// shortSHA returns the leading 8 characters of a SHA, or the SHA itself
// if it's already shorter. Used only for human-readable diagnostics —
// never compared against git's own short-SHA resolution.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

