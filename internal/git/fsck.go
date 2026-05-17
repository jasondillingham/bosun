package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// FsckError is returned by FsckWorktree when `git fsck` reports problems
// with the worktree's object store. The Output field carries git's
// diagnostic text so callers (cmd_merge) can surface it directly to the
// operator — fsck failures usually indicate torn writes or filesystem
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

// FsckWorktree runs `git -C dir fsck --no-progress --no-dangling`. Returns
// nil when the worktree's object store is healthy; an *FsckError when fsck
// exits non-zero (i.e. detected an error, as opposed to a dangling-object
// warning).
//
// We pin `--no-progress` so stderr stays empty on a clean repo — otherwise
// "Checking object directories: ..." would always appear and we'd have to
// parse around it. `--no-dangling` suppresses the long list of dangling
// objects that a healthy mid-rebase repo typically has; they're not a
// merge-blocker.
//
// Intended caller: cmd_merge's pre-squash gate. Cheap on small repos
// (~50ms); on a 100k-object repo expect 1-2s.
func FsckWorktree(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "fsck", "--no-progress", "--no-dangling")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// We treat any non-zero exit as a refusal signal. The brief is
	// explicit: no override flag, so we don't try to distinguish hard
	// errors from soft warnings — operator investigates.
	if _, ok := err.(*exec.ExitError); ok {
		return &FsckError{Output: strings.TrimSpace(string(out))}
	}
	// Subprocess didn't run cleanly (couldn't exec git, ctx cancelled,
	// etc.). Propagate as a plain error so callers can distinguish
	// "fsck couldn't run" from "fsck ran and found problems".
	return fmt.Errorf("run git fsck in %s: %w", dir, err)
}
