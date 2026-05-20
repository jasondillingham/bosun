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

// DefaultTimeoutSeconds is applied to any Hook with TimeoutSeconds == 0.
// A zero timeout used to mean "no timeout", which let a runaway operator
// command pin the bosun process indefinitely. v0.6 turns that into a
// 30-second safety net — long enough that legitimate hooks (lint, vet,
// docker pulls) complete comfortably, short enough that an interactive
// `read` or wedged subprocess can't hang init forever.
const DefaultTimeoutSeconds = 30

// ErrTimeout is the sentinel returned (via errors.Is) when a hook is
// killed because its timeout fired. Callers can distinguish a timeout
// from an exit-status failure to render different messaging without
// string-matching on the error text.
var ErrTimeout = errors.New("hook timed out")

// KnownEvents lists every event name v0.1 recognises. Config validation
// rejects entries outside this set so typos surface at load time rather
// than silently never firing.
var KnownEvents = []string{
	"pre-init",
	"post-init",
	"post-done",
	"pre-merge",
	"post-merge",
	"pre-cleanup",
	"post-cleanup",
	// pre-remove was already wired in cmd_remove.go but missing from
	// this list — an operator who registered a hook for it would have
	// failed config validation despite the callsite existing. Added
	// alongside the Phase 5 #64 webhook wiring.
	"pre-remove",
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
//
// TimeoutSeconds == 0 falls back to DefaultTimeoutSeconds (30s). A negative
// value would round trip through JSON unchanged; treat any non-positive
// value as "use the default" rather than as "no timeout" — letting hooks
// run unbounded is exactly the foot-gun this version closes.
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
				_, _ = fmt.Fprintf(os.Stderr, "bosun: warning: hook %s #%d failed: %v\n", event, i, err)
				continue
			}
			return fmt.Errorf("hook %s: %w", event, err)
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
	timeoutSeconds := h.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = DefaultTimeoutSeconds
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", h.Command) //nolint:gosec // G204: user-authored hook command from their own .bosun/config.json
	cmd.Env = append(os.Environ(), envSlice(env)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		// Wrap the sentinel so callers can check errors.Is(err, ErrTimeout)
		// without losing the duration in the user-facing message.
		return fmt.Errorf("timed out after %ds: %w", timeoutSeconds, ErrTimeout)
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
