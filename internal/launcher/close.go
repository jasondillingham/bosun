package launcher

import (
	"fmt"
	"os"
	"os/exec"
)

// CloseTmuxWindow runs `tmux kill-window -t <name>` to close the window
// bosun opened for a session via launchTmux. Best-effort: returns nil
// when TMUX is unset (no tmux session to close into), and surfaces
// the tmux exit error otherwise.
//
// The caller is responsible for log-and-continue. A non-existent
// window (already closed, opened by a different launcher, never
// opened) and "tmux choked" produce the same error at this layer, but
// both outcomes are benign for cleanup's purposes: the worktree is
// going to be reaped regardless.
//
// TMUX being set is a stronger signal than `exec.LookPath("tmux")`:
// it's set iff tmux is currently running in the operator's session.
// If the binary disappeared while TMUX is still set, cmd.Run() will
// fail cleanly with a "no such file" error — also fine for our
// log-and-continue contract.
//
// Window name matches launchTmux's `-n` argument (Options.SessionName,
// e.g. "session-1"). Validated session labels can't collide with
// operator-typed window names — that's why this lookup is unambiguous
// when it finds a match.
func CloseTmuxWindow(name string) error {
	if name == "" {
		return fmt.Errorf("empty window name")
	}
	if os.Getenv("TMUX") == "" {
		return nil
	}
	return closeRunFn(exec.Command("tmux", "kill-window", "-t", name)) //nolint:gosec // G204: name is a validated bosun session label
}

// closeRunFn is the test seam mirroring execRunFn used by the launch
// paths. Tests override it to assert on argv and simulate
// success / window-not-found / hard-error responses without invoking
// the real tmux binary.
var closeRunFn = func(cmd *exec.Cmd) error { return cmd.Run() }
