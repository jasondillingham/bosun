package claudehook

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitRevParseTimeout is the per-invocation cap on the two git
// rev-parse calls the hook makes when resolving cwd → worktree.
// Bounded explicitly so a hung filesystem can't push the hook
// anywhere near Claude Code's 5 s outer timeout.
const gitRevParseTimeout = 2 * time.Second

// gitRevParse runs `git -C dir rev-parse <args...>` and returns the
// trimmed first line of stdout. Errors are wrapped with the
// invocation so the operator can grep for them in `claude` logs.
//
// We don't reuse internal/git's Client here because it pulls a
// timeout config, runner abstraction, and panic-recovery surface
// that bloats the hook binary path. The hook's needs are narrow:
// two short, time-bounded shellouts.
func gitRevParse(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitRevParseTimeout)
	defer cancel()
	all := append([]string{"-C", dir, "rev-parse"}, args...)
	cmd := exec.CommandContext(ctx, "git", all...) //nolint:gosec // G204: bosun's git invocation; argv composed from validated dir + hook-controlled args
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(all, " "), err)
	}
	// rev-parse with single-line outputs writes "<value>\n"; take the
	// first line in case the caller passed flags that produce
	// multi-line output (none of bosun's calls do today, but the
	// safer parse costs nothing).
	s := strings.TrimRight(string(out), "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s, nil
}
