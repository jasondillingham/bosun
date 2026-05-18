// Package git is a thin wrapper around the git CLI. It hides exec details
// and exposes one method per high-level operation bosun needs. A Runner
// interface is provided so callers can inject a fake runner in tests.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultOpTimeout is the per-operation timeout applied by Client when no
// explicit value is configured. Kept here rather than imported from
// internal/config to avoid a package cycle — internal/config wires its own
// constant through Client.SetTimeout at startup.
const DefaultOpTimeout = 30 * time.Second

// DefaultWorktreeAddTimeout is the per-operation cap applied to
// `git worktree add` specifically. Worktree creation under fsync pressure
// (APFS, Spotlight reindex, kernel write-back saturation) is legitimately
// slow — 30s fires spuriously, which the v0.6 trial reproduced. 120s is the
// floor; operators who set a higher `git_op_timeout_seconds` still win,
// because AddWorktree picks max(Timeout, WorktreeAddTimeout).
const DefaultWorktreeAddTimeout = 120 * time.Second

// execPipeDrainTimeout caps how long cmd.Wait blocks reading stdout/stderr
// after the child has been killed by ctx cancellation. Without this, a
// git subprocess that forks helpers (libexec hooks, fsync workers) leaks
// the inherited stdout/stderr fds to those grandchildren; SIGKILL on the
// git leader doesn't close the pipes, and Wait blocks until the
// grandchildren exit. This is the v0.6 trial's 14-minute "hang despite
// 30s timeout" finding: the timeout DID fire, but pipe-drain pinned us.
//
// 1s is a tight bound: the leader has already been killed, anything still
// holding the pipes is an orphaned grandchild whose output we don't need.
// We're not waiting on a long sentence here — we're letting the kernel
// finish flushing whatever was already in the buffer.
const execPipeDrainTimeout = 1 * time.Second

// TimeoutError indicates a git subprocess exceeded its configured timeout.
// Callers can check `errors.As(err, &git.TimeoutError{})` to distinguish
// the silent-hang case (Spotlight reindex, APFS pressure, etc.) from
// real git failures. Hint, when set, carries operator-actionable recovery
// guidance — populated by callers that know what the right next step is
// (e.g. AddWorktree pointing at `bosun init --resume`).
type TimeoutError struct {
	Op      string        // joined git args, e.g. "worktree add /tmp/foo bosun/session-1"
	Timeout time.Duration // the timeout that fired
	Hint    string        // optional operator-actionable suffix
}

func (e *TimeoutError) Error() string {
	base := fmt.Sprintf("git %s: timed out after %s", e.Op, e.Timeout)
	if e.Hint != "" {
		return base + " — " + e.Hint
	}
	return base
}

// Runner is the surface area we depend on from os/exec. It lets tests inject
// a fake runner that returns canned output.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error)
}

// ExecRunner shells out to the real git binary via os/exec.
type ExecRunner struct{}

// Run invokes `git <args>` in dir. If dir is empty, git runs in the current
// working directory.
func (ExecRunner) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// WaitDelay bounds the time Wait spends draining stdout/stderr after
	// the child has been killed by ctx. Without it, fsync-pressured git
	// that forked grandchildren can keep the pipes open long after our
	// timeout fired. See execPipeDrainTimeout for the v0.6 trial root cause.
	cmd.WaitDelay = execPipeDrainTimeout
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Client wraps a Runner with the operations bosun needs.
type Client struct {
	Runner Runner
	// Timeout, if positive, caps every git subprocess invocation. Zero
	// disables the timeout (use the parent ctx as-is). New() initializes
	// this to DefaultOpTimeout.
	Timeout time.Duration
	// WorktreeAddTimeout, if positive, applies specifically to
	// `git worktree add` — which is legitimately slow under fsync pressure
	// and needs a longer cap than regular git ops. The effective timeout
	// for worktree-add is max(Timeout, WorktreeAddTimeout), so an operator
	// who raises `git_op_timeout_seconds` above this floor still wins.
	// New() initializes this to DefaultWorktreeAddTimeout.
	WorktreeAddTimeout time.Duration
}

// New returns a Client that shells out to the real git binary with the
// default per-operation timeout.
func New() *Client {
	return &Client{
		Runner:             ExecRunner{},
		Timeout:            DefaultOpTimeout,
		WorktreeAddTimeout: DefaultWorktreeAddTimeout,
	}
}

// SetTimeout overrides the per-operation timeout. A zero or negative value
// disables the timeout entirely.
func (c *Client) SetTimeout(d time.Duration) {
	c.Timeout = d
}

// withTimeout returns a child context capped at d, or the parent
// unchanged when d is non-positive. The returned cancel function is
// always safe to call.
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, d)
}

// run is the internal helper that converts (stdout, stderr, err) into a
// uniform error including both stdout and stderr. Both streams matter
// because git writes diagnostic content to whichever stream it pleases —
// `git merge --squash`, for example, prints "CONFLICT (content): Merge
// conflict in ..." to stdout.
//
// run also enforces c.Timeout. When the timeout fires, the returned error
// is a *TimeoutError naming the operation and the configured duration —
// not a bare context.DeadlineExceeded — because this surfaces directly to
// the operator (see `bosun init` silent-hang findings, v0.4).
func (c *Client) run(parentCtx context.Context, dir string, args ...string) (string, error) {
	return c.runWithTimeout(parentCtx, c.Timeout, dir, args...)
}

// runWithTimeout is like run but uses an explicit timeout instead of
// c.Timeout. AddWorktree uses it to apply WorktreeAddTimeout — that op is
// the legitimately-slow outlier and shouldn't share a 30s cap with
// instant-feedback ops like `rev-parse`.
func (c *Client) runWithTimeout(parentCtx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := withTimeout(parentCtx, timeout)
	defer cancel()
	out, errOut, err := c.Runner.Run(ctx, dir, args...)
	if err != nil {
		if isOurTimeout(parentCtx, ctx) {
			return out, &TimeoutError{Op: strings.Join(args, " "), Timeout: timeout}
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			combined := strings.TrimSpace(strings.TrimSpace(out) + "\n" + strings.TrimSpace(errOut))
			return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, combined)
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// worktreeAddTimeout returns the timeout to use for `git worktree add`,
// which is max(c.Timeout, c.WorktreeAddTimeout). Either field being
// non-positive means "no cap from this source" — if BOTH are non-positive,
// the operation is unbounded (parent ctx wins). This matches the spec
// commitment: an operator who raises `git_op_timeout_seconds` past the
// 120s floor still gets the longer cap they asked for, while leaving the
// default a sensible floor for the legitimately-slow case.
func (c *Client) worktreeAddTimeout() time.Duration {
	a, b := c.Timeout, c.WorktreeAddTimeout
	if a > b {
		return a
	}
	return b
}

// isOurTimeout reports whether the child ctx deadlined because of *our*
// timeout, as opposed to the parent's. Distinguishing these matters when
// callers wrap the client with their own deadline — we shouldn't claim
// "git op timed out after 30s" when the operator's ctx was the limiter.
func isOurTimeout(parentCtx, childCtx context.Context) bool {
	return childCtx.Err() == context.DeadlineExceeded && parentCtx.Err() != context.DeadlineExceeded
}

// runStreams is like run but returns stdout and stderr separately, without
// folding either into the error string. Use this when a caller (e.g.
// FsckWorktree) needs the raw stderr text — git fsck writes its diagnostic
// findings there, and run's combined-into-error format loses the structure
// the caller wants to preserve.
//
// Timeout handling is identical to run: a child-ctx deadline fired by us
// becomes a *TimeoutError; everything else is returned as the raw exec
// error so callers can errors.As against *exec.ExitError directly.
func (c *Client) runStreams(parentCtx context.Context, dir string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := withTimeout(parentCtx, c.Timeout)
	defer cancel()
	out, errOut, runErr := c.Runner.Run(ctx, dir, args...)
	if runErr != nil && isOurTimeout(parentCtx, ctx) {
		return out, errOut, &TimeoutError{Op: strings.Join(args, " "), Timeout: c.Timeout}
	}
	return out, errOut, runErr
}

// RepoRoot returns the absolute path to the *current* worktree (linked or main).
// Use MainWorktreePath when you need to read bosun's .bosun/ state, which
// always lives in the main worktree regardless of where the command was run.
func (c *Client) RepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := c.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// MainWorktreePath returns the absolute path of the *main* (primary) worktree
// of the repository, regardless of whether dir is inside the main worktree or
// a linked one. This is the worktree whose .bosun/ directory holds the
// canonical claims/state for the repo.
func (c *Client) MainWorktreePath(ctx context.Context, dir string) (string, error) {
	out, err := c.run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(out)
	// commonDir is the absolute path to the shared .git dir, e.g.
	// "/repo/.git" — the main worktree is its parent.
	return filepath.Dir(commonDir), nil
}

// RevParseHEAD returns the current HEAD commit SHA for dir. Equivalent to
// `git rev-parse HEAD`. Empty repos (no commits) surface as an error from
// git, not an empty string.
//
// Routed through c.run so the configured per-op timeout applies. Used by
// cmd_merge's pre/post-merge SHA capture and the merge --undo recovery
// path — anywhere a "what is HEAD pointing at" probe needs to be both
// time-bounded and consistent with the rest of the git surface.
func (c *Client) RevParseHEAD(ctx context.Context, dir string) (string, error) {
	out, err := c.run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// RevParseRef returns the commit SHA the named ref resolves to in dir.
// Equivalent to `git rev-parse <ref>`. Used by init's stale-branch check
// to compare a leftover `bosun/session-N` branch tip against the base
// branch HEAD without depending on the per-call shape of show-ref.
func (c *Client) RevParseRef(ctx context.Context, dir, ref string) (string, error) {
	out, err := c.run(ctx, dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ResetBranchTo force-moves the named branch to point at sha. Equivalent
// to `git branch -f <name> <sha>`. Used by init's stale-branch recovery
// when --force is passed: the branch is recreated at base HEAD so the
// next `git worktree add` checks out a fresh tip rather than the old
// (and possibly stale) commit.
func (c *Client) ResetBranchTo(ctx context.Context, dir, name, sha string) error {
	_, err := c.run(ctx, dir, "branch", "-f", name, sha)
	return err
}

// ResetHard runs `git reset --hard <sha>` in dir. Destructive: discards
// the working tree, index, and HEAD's recorded position in favor of sha.
//
// Used by `bosun merge --undo` to roll the base branch back to its
// pre-merge SHA. On timeout, the *TimeoutError carries a recovery hint
// pointing at `git reflog` — when reset itself can't complete because of
// fsync pressure, the operator's escape hatch is the reflog plus a manual
// retry once load drops.
func (c *Client) ResetHard(ctx context.Context, dir, sha string) error {
	_, err := c.run(ctx, dir, "reset", "--hard", sha)
	var to *TimeoutError
	if errors.As(err, &to) {
		to.Hint = "system likely under fsync pressure; recover the pre-reset SHA via `git reflog` and retry `git reset --hard <sha>` once load drops."
		return to
	}
	return err
}

// CurrentBranch returns the abbreviated HEAD ref name (or empty for detached HEAD).
func (c *Client) CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := c.run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "HEAD" {
		return "", nil // detached
	}
	return name, nil
}

// BranchExists reports whether a local branch with that name exists.
func (c *Client) BranchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, errOut, err := c.Runner.Run(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git show-ref refs/heads/%s: %w: %s", branch, err, strings.TrimSpace(errOut))
}

// CreateBranch creates `name` pointing at `base`.
func (c *Client) CreateBranch(ctx context.Context, dir, name, base string) error {
	_, err := c.run(ctx, dir, "branch", name, base)
	return err
}

// DeleteBranch deletes a local branch. force => -D, otherwise -d (safe).
func (c *Client) DeleteBranch(ctx context.Context, dir, name string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := c.run(ctx, dir, "branch", flag, name)
	return err
}

// AddWorktree creates a worktree at path checking out branch.
//
// Worktree-add uses its own (longer) timeout per WorktreeAddTimeout and
// surfaces a recovery hint on timeout pointing the operator at
// `bosun init --resume` — without that breadcrumb, an operator hitting
// the cap has no signal about how to proceed.
func (c *Client) AddWorktree(ctx context.Context, dir, path, branch string) error {
	_, err := c.runWithTimeout(ctx, c.worktreeAddTimeout(), dir, "worktree", "add", path, branch)
	var to *TimeoutError
	if errors.As(err, &to) {
		to.Hint = "system likely under fsync pressure; check `uptime` and retry, or `bosun init --resume` to continue."
		return to
	}
	return err
}

// RemoveWorktree removes a worktree. force => --force.
//
// Before invoking git, RemoveWorktree walks the worktree and adds the
// user-writable bit to every file and directory it can stat. This is
// necessary because `--isolate-cache` populates `GOMODCACHE` (under
// `<worktree>/.cache/go-mod`) with mode-0444 files inside mode-0555
// directories — `git worktree remove --force` otherwise dies on the first
// `unlink: Permission denied`. On Windows the same chmod sweep clears the
// read-only attribute that read-only modes translate into. The pre-chmod
// is best-effort: per-entry stat/chmod failures are swallowed so a
// concurrently-disappearing file doesn't poison the cleanup.
func (c *Client) RemoveWorktree(ctx context.Context, dir, path string, force bool) error {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		_ = chmodWritableTree(path)
	}
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	_, err := c.run(ctx, dir, args...)
	return err
}

// MoveWorktree renames a linked worktree's directory via
// `git worktree move <old> <new>`. Differs from a raw filesystem rename
// because git keeps admin metadata under `<main>/.git/worktrees/<name>`
// pointing at the worktree's location — `mv` would orphan that metadata
// and every subsequent git op against the worktree would fail.
//
// The new path must not exist; git enforces that itself. Callers that
// want overwrite semantics should detect the collision up front and
// surface a conflict rather than swallowing it.
func (c *Client) MoveWorktree(ctx context.Context, dir, oldPath, newPath string) error {
	_, err := c.run(ctx, dir, "worktree", "move", oldPath, newPath)
	return err
}

// UnlockWorktree releases a `git worktree lock` on path. Used by the
// init --resume path to recover a worktree left locked by a prior killed
// init. Goes through c.run so the configured timeout applies — pre-v0.7+
// the call site lived in cmd_init.go and bypassed timeout enforcement.
func (c *Client) UnlockWorktree(ctx context.Context, dir, path string) error {
	_, err := c.run(ctx, dir, "worktree", "unlock", path)
	return err
}

// chmodWritableTree walks root and ORs the user-write (and, for directories,
// user-execute) bits onto every entry's permission mode. Errors at any
// individual entry are swallowed — the function returns nil unless the walk
// itself can't start. This is a best-effort pre-pass for RemoveWorktree;
// the subsequent git invocation is the authoritative source of failure.
func chmodWritableTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A failure entering one subtree shouldn't abort the rest. Skip
			// just this entry (or subtree if d is nil).
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		mode := info.Mode().Perm()
		want := mode | 0o200 // u+w
		if d.IsDir() {
			want |= 0o100 // u+x so we can descend / unlink inside
		}
		if want != mode {
			_ = os.Chmod(p, want)
		}
		return nil
	})
}

// Worktree represents one entry from `git worktree list --porcelain`.
type Worktree struct {
	Path     string // absolute path
	HEAD     string // commit SHA at HEAD, or empty if bare
	Branch   string // full ref like "refs/heads/bosun/session-1", or empty if detached
	Bare     bool
	Locked   bool
	Prunable bool // git reports the gitdir points at a missing/invalid worktree
}

// ListWorktrees returns every worktree known to git for this repo.
// It parses the porcelain format documented in git-worktree(1).
func (c *Client) ListWorktrees(ctx context.Context, dir string) ([]Worktree, error) {
	out, err := c.run(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(out), nil
}

func parseWorktreeList(s string) []Worktree {
	var result []Worktree
	var cur *Worktree
	flush := func() {
		if cur != nil && cur.Path != "" {
			result = append(result, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &Worktree{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			// Skip stray lines outside a record.
		case strings.HasPrefix(line, "HEAD "):
			cur.HEAD = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(line, "branch ")
		case line == "bare":
			cur.Bare = true
		case line == "detached":
			cur.Branch = ""
		case strings.HasPrefix(line, "locked"):
			cur.Locked = true
		case strings.HasPrefix(line, "prunable"):
			cur.Prunable = true
		}
	}
	flush()
	return result
}

// RevListCount returns the number of commits in `base..HEAD` for the worktree at dir.
func (c *Client) RevListCount(ctx context.Context, dir, base string) (int, error) {
	out, err := c.run(ctx, dir, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse rev-list count: %w", err)
	}
	return n, nil
}

// UnmergedPatches counts commits on `branch` whose patch-id is not already
// present on `base`. It parses `git cherry <base> <branch>` output: each line
// is prefixed with `+` (unmerged) or `-` (patch-equivalent commit on base).
// A squash-merged session typically reports `-` for its commit, so this is
// the right signal for "is this branch's content already on main?" — whereas
// RevListCount would still report 1 ahead.
func (c *Client) UnmergedPatches(ctx context.Context, dir, base, branch string) (int, error) {
	out, err := c.run(ctx, dir, "cherry", base, branch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+ ") || line == "+" {
			n++
		}
	}
	return n, nil
}

// TreeEqualsBase reports whether the tip trees of base and branch are
// identical (i.e. `git diff base..branch` is empty). True means merging
// branch into base would be a no-op. This catches an "already merged"
// state that patch-id comparison misses: after the operator hand-resolves
// a prior squash conflict and commits, the branch's content lives on base
// but the patch-ids no longer match.
func (c *Client) TreeEqualsBase(ctx context.Context, dir, base, branch string) (bool, error) {
	_, errOut, err := c.Runner.Run(ctx, dir, "diff", "--quiet", base+".."+branch, "--")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git diff --quiet %s..%s: %w: %s", base, branch, err, strings.TrimSpace(errOut))
}

// PruneWorktrees runs `git worktree prune` to drop admin metadata for
// worktrees whose directories have disappeared on disk. Safe to call
// even when there's nothing to prune.
func (c *Client) PruneWorktrees(ctx context.Context, dir string) error {
	_, err := c.run(ctx, dir, "worktree", "prune")
	return err
}

// ScanOrphanDirs returns absolute paths of sibling directories of repoRoot
// whose name matches the worktree suffix pattern. These are candidates for
// orphan-dir cleanup — directories left behind on disk after the matching
// worktree's git admin metadata was already pruned (the v0.3 corruption
// shape that `git worktree list` can't surface).
//
// suffixPattern is the bosun config pattern (e.g. "-bosun-{N}"). The
// substitution point `{N}` is treated as a `*` glob, so any non-empty
// session label matches. The repo's own directory is never returned.
//
// The caller is responsible for further filtering: excluding entries that
// `git worktree list` currently tracks, and refusing to remove directories
// that still carry a `.git` file pointing back at the main repo.
func ScanOrphanDirs(repoRoot string, suffixPattern string) ([]string, error) {
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)

	// The suffix is concatenated onto the repo base name. Translating
	// `{N}` → `*` produces a glob pattern usable with filepath.Match.
	glob := base + strings.ReplaceAll(suffixPattern, "{N}", "*")

	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, fmt.Errorf("read parent dir %s: %w", parent, err)
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Belt-and-suspenders: never return the repo's own directory.
		if name == base {
			continue
		}
		matched, err := filepath.Match(glob, name)
		if err != nil {
			return nil, fmt.Errorf("match glob %q: %w", glob, err)
		}
		if !matched {
			continue
		}
		out = append(out, filepath.Join(parent, name))
	}
	return out, nil
}

// ChmodWritableTree is the exported wrapper around the internal chmod
// pre-pass used by RemoveWorktree. Callers that need to nuke an orphan
// worktree directory (where git no longer has admin metadata, so
// `git worktree remove` can't help) use this to clear the read-only
// bits that --isolate-cache leaves on GOMODCACHE entries before
// os.RemoveAll. The walk is best-effort — per-entry failures are
// swallowed.
func ChmodWritableTree(root string) error {
	return chmodWritableTree(root)
}

// PorcelainStatusLine is one parsed line of `git status --porcelain`.
type PorcelainStatusLine struct {
	XY   string // first two chars, e.g. " M", "??", "A "
	Path string
}

// Status returns parsed lines from `git status --porcelain` in dir.
// Renamed entries (R) collapse to the new path.
func (c *Client) Status(ctx context.Context, dir string) ([]PorcelainStatusLine, error) {
	out, err := c.run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parsePorcelainStatus(out), nil
}

func parsePorcelainStatus(s string) []PorcelainStatusLine {
	var result []PorcelainStatusLine
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 3 {
			continue
		}
		xy := line[:2]
		rest := line[3:]
		// Handle rename "R  old -> new" — keep the new path.
		if strings.Contains(xy, "R") {
			if idx := strings.Index(rest, " -> "); idx >= 0 {
				rest = rest[idx+4:]
			}
		}
		result = append(result, PorcelainStatusLine{XY: xy, Path: rest})
	}
	return result
}

// DirtyCount returns the number of tracked-file changes in dir, excluding untracked (??) entries.
func (c *Client) DirtyCount(ctx context.Context, dir string) (int, error) {
	lines, err := c.Status(ctx, dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range lines {
		if l.XY != "??" {
			n++
		}
	}
	return n, nil
}

// LogEntry is one row from `git log --format=...`.
type LogEntry struct {
	ShortSHA   string
	Unix       int64
	Relative   string
	Subject    string
}

// LastCommit returns the most recent commit on HEAD in dir. Returns (nil, nil) if there are no commits ahead.
// callers pass `base` so we can distinguish "no commits ahead" from real errors.
func (c *Client) LastCommit(ctx context.Context, dir, base string) (*LogEntry, error) {
	// First check if there are any commits ahead of base. This avoids
	// returning the base-branch HEAD as the "last commit".
	ahead, err := c.RevListCount(ctx, dir, base)
	if err != nil {
		return nil, err
	}
	if ahead == 0 {
		return nil, nil
	}
	out, err := c.run(ctx, dir, "log", "-1", "--format=%h|%ct|%ar|%s")
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return nil, nil
	}
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected log format: %q", line)
	}
	unix, _ := strconv.ParseInt(parts[1], 10, 64)
	return &LogEntry{
		ShortSHA: parts[0],
		Unix:     unix,
		Relative: parts[2],
		Subject:  parts[3],
	}, nil
}

// LogN returns the last n commits in dir as raw `git log -n` output (for `bosun show`).
func (c *Client) LogN(ctx context.Context, dir string, n int) (string, error) {
	out, err := c.run(ctx, dir, "log", fmt.Sprintf("-%d", n), "--oneline", "--decorate")
	if err != nil {
		return "", err
	}
	return out, nil
}

// MergeSquash performs `git merge --squash <branch>` in dir without committing.
// Caller is responsible for the subsequent `git commit`.
func (c *Client) MergeSquash(ctx context.Context, dir, branch string) error {
	_, err := c.run(ctx, dir, "merge", "--squash", branch)
	return err
}

// MergeNoFF performs `git merge --no-ff <branch>` in dir, creating a merge commit.
// Caller provides the commit message via Commit afterwards if --no-commit was used.
func (c *Client) MergeNoFF(ctx context.Context, dir, branch, message string) error {
	args := []string{"merge", "--no-ff", branch}
	if message != "" {
		args = append(args, "-m", message)
	}
	_, err := c.run(ctx, dir, args...)
	return err
}

// Commit creates a commit in dir with the given message. The index is assumed
// to already contain the staged changes (e.g. from `merge --squash`).
func (c *Client) Commit(ctx context.Context, dir, message string) error {
	_, err := c.run(ctx, dir, "commit", "-m", message)
	return err
}

// MergeAbort aborts an in-progress merge in dir. Safe to call even if no merge is active (returns an error).
func (c *Client) MergeAbort(ctx context.Context, dir string) error {
	_, err := c.run(ctx, dir, "merge", "--abort")
	return err
}

// AppendWorktreeExclude appends a line to the worktree's .git/info/exclude
// file. That file is local to the worktree (or its sharing group) and is
// never committed — perfect for marking files like BOSUN_BRIEF.md that
// bosun writes into the worktree but doesn't want the session to commit.
func (c *Client) AppendWorktreeExclude(ctx context.Context, dir, line string) error {
	out, err := c.run(ctx, dir, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	excludePath := strings.TrimSpace(out)
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(dir, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("mkdir exclude: %w", err)
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open exclude: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("write exclude: %w", err)
	}
	return nil
}
