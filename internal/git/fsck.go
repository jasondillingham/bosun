package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// FsckError is returned by (*Client).FsckWorktree when `git fsck` reports
// problems with the worktree's object store. The Output field carries
// git's diagnostic text so callers (cmd_merge) can surface it directly to
// the operator — fsck failures usually indicate torn writes or filesystem
// corruption, not anything the operator can override.
type FsckError struct {
	Output string
}

func (e *FsckError) Error() string {
	if e.Output == "" {
		return "git fsck reported errors"
	}
	return "git fsck reported errors:\n" + e.Output
}

// FsckWorktree runs `git fsck --no-progress --no-dangling` in dir. Returns
// nil when the worktree's object store is healthy; an *FsckError when fsck
// exits non-zero (i.e. detected an error, as opposed to a dangling-object
// warning); a *TimeoutError if the configured per-op timeout fires; or a
// plain wrapped error if the subprocess couldn't run at all.
//
// We pin `--no-progress` so stderr stays empty on a clean repo — otherwise
// "Checking object directories: ..." would always appear and we'd have to
// parse around it. `--no-dangling` suppresses the long list of dangling
// objects that a healthy mid-rebase repo typically has; they're not a
// merge-blocker.
//
// Migrated from a free function in v0.6.2 so the Client's configured
// per-op timeout applies. Pre-migration, fsck called exec.CommandContext
// directly with the caller's ctx and would hang indefinitely on a worktree
// under fsync pressure regardless of operator-configured caps.
//
// Intended caller: cmd_merge's pre-squash gate. Cheap on small repos
// (~50ms); on a 100k-object repo expect 1-2s.
func (c *Client) FsckWorktree(ctx context.Context, dir string) error {
	// Use runStreams (not run) because fsck writes its diagnostic findings
	// to stderr; the FsckError.Output contract embeds that text so the
	// operator sees what was wrong, not just "fsck reported errors".
	stdout, stderr, err := c.runStreams(ctx, dir, "fsck", "--no-progress", "--no-dangling")
	if err == nil {
		return nil
	}
	// Our timeout fired — annotate with a recovery hint specific to the
	// fsync-pressure failure mode. Different from worktree-add's hint
	// (which points at `bosun init --resume`); for fsck the operator
	// either waits and retries, or drops the session entirely.
	var to *TimeoutError
	if errors.As(err, &to) {
		to.Hint = "system likely under fsync pressure; check `uptime` and retry, or `bosun cleanup` to drop this session and start fresh."
		return to
	}
	// Non-zero exit means fsck ran and found problems. Combine both
	// streams into FsckError.Output — historically this used
	// cmd.CombinedOutput(); we replicate that here so callers can keep
	// surfacing the diagnostic text verbatim.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		combined := strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
		return &FsckError{Output: combined}
	}
	// Subprocess didn't run cleanly (couldn't exec git, ctx cancelled by
	// parent, etc.). Propagate as a plain error so callers can distinguish
	// "fsck couldn't run" from "fsck ran and found problems".
	return fmt.Errorf("run git fsck in %s: %w", dir, err)
}
