// Package hooks runs operator-defined shell commands at bosun lifecycle
// moments. v0.1 scaffolds three events (pre-init, post-init, post-done);
// future rounds extend the known set to cover merge and cleanup.
//
// Hooks are intentionally simple: each event matches by exact string, the
// command runs through `sh -c`, and a per-hook FailOpen flag decides whether
// a non-zero exit aborts the bosun command or just emits a warning. The
// caller is responsible for surfacing warnings to the user.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// KnownEvents lists every event name v0.1 recognises. Config validation
// rejects entries outside this set so typos surface at load time rather
// than silently never firing.
var KnownEvents = []string{
	"pre-init",
	"post-init",
	"post-done",
}

// IsKnownEvent reports whether name is in KnownEvents.
func IsKnownEvent(name string) bool {
	for _, e := range KnownEvents {
		if e == name {
			return true
		}
	}
	return false
}

// Hook is a single operator-defined shell command bound to a lifecycle event.
//
// Command runs through `sh -c`, so the operator can use pipes, redirects,
// and `&&` chains without bosun re-parsing the string. FailOpen=true keeps
// the bosun command running on non-zero exit (the default in
// config.example.json); FailOpen=false makes the hook a hard gate.
// TimeoutSeconds=0 means no timeout.
type Hook struct {
	Event          string `json:"event"`
	Command        string `json:"command"`
	FailOpen       bool   `json:"fail_open"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// Run executes every hook in `hooks` whose Event matches `event`, in order,
// injecting env entries on top of the parent process environment. Returns
// the first error from a non-FailOpen hook; FailOpen hooks emit a warning
// to stderr and otherwise behave as if they succeeded.
//
// Hooks that match no event are skipped silently. A nil or empty slice is
// a no-op.
//
// Callers MUST NOT invoke Run while holding any of bosun's cross-process
// flocks (claims, state, mcp.lock). A slow hook command (sleeping or
// awaiting network) would otherwise pin the flock and block every other
// bosun process trying to take the same lock. Today every callsite
// (cmd_init.go pre-init/post-init, cmd_done.go post-done) fires outside
// flock-held regions; preserve that property when adding new events.
func Run(ctx context.Context, hooks []Hook, event string, env map[string]string) error {
	for i, h := range hooks {
		if h.Event != event {
			continue
		}
		if err := runOne(ctx, h, env); err != nil {
			if h.FailOpen {
				fmt.Fprintf(os.Stderr, "bosun: warning: %s hook #%d failed: %v\n", event, i, err)
				continue
			}
			return fmt.Errorf("%s hook: %w", event, err)
		}
	}
	return nil
}

// runOne is the single-hook execution path. Split out so Run reads as
// "select matching, dispatch, decide on failure" without branching on
// timeout/no-timeout inline.
func runOne(ctx context.Context, h Hook, env map[string]string) error {
	if h.Command == "" {
		return errors.New("empty command")
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if h.TimeoutSeconds > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(h.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, "sh", "-c", h.Command)
	cmd.Env = append(os.Environ(), envSlice(env)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %ds", h.TimeoutSeconds)
	}
	return err
}

// envSlice flattens a map into KEY=VALUE entries. Order is not specified;
// downstream `sh -c` reads them as a set, not a sequence.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
