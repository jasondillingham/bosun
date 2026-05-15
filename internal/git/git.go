// Package git is a thin wrapper around the git CLI. It hides exec details
// and exposes one method per high-level operation bosun needs. A Runner
// interface is provided so callers can inject a fake runner in tests.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

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
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Client wraps a Runner with the operations bosun needs.
type Client struct {
	Runner Runner
}

// New returns a Client that shells out to the real git binary.
func New() *Client {
	return &Client{Runner: ExecRunner{}}
}

// run is the internal helper that converts (stdout, stderr, err) into a
// uniform error including both stdout and stderr. Both streams matter
// because git writes diagnostic content to whichever stream it pleases —
// `git merge --squash`, for example, prints "CONFLICT (content): Merge
// conflict in ..." to stdout.
func (c *Client) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, errOut, err := c.Runner.Run(ctx, dir, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			combined := strings.TrimSpace(strings.TrimSpace(out) + "\n" + strings.TrimSpace(errOut))
			return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, combined)
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
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
func (c *Client) AddWorktree(ctx context.Context, dir, path, branch string) error {
	_, err := c.run(ctx, dir, "worktree", "add", path, branch)
	return err
}

// RemoveWorktree removes a worktree. force => --force.
func (c *Client) RemoveWorktree(ctx context.Context, dir, path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	_, err := c.run(ctx, dir, args...)
	return err
}

// Worktree represents one entry from `git worktree list --porcelain`.
type Worktree struct {
	Path   string // absolute path
	HEAD   string // commit SHA at HEAD, or empty if bare
	Branch string // full ref like "refs/heads/bosun/session-1", or empty if detached
	Bare   bool
	Locked bool
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
