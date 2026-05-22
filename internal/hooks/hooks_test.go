package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain skips the entire hooks test suite when `sh` isn't on PATH.
// Every test below constructs Hook fixtures with sh-flavoured command
// strings (`printf …`, `exit 7`, `sleep 5`, `touch …`) which the
// hooks package execs via `sh -c <command>`. On Windows runners
// (no `sh` by default) these tests aren't load-bearing — operators
// configure `cmd /c …` or `powershell -c …` hook commands instead,
// and the production exec path itself (internal/hooks/hooks.go) is
// platform-neutral.
//
// Exit 0 (rather than skipping individual tests) keeps the package
// reported as PASS in the windows-latest CI runner. Windows-trial
// finding 2026-05-22 — see docs/windows-trial-2026-05-22.md.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("sh"); err != nil {
		fmt.Fprintln(os.Stderr, "hooks tests skipped: sh not on PATH (Windows runner or trimmed env)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestIsKnownEvent(t *testing.T) {
	for _, e := range KnownEvents {
		if !IsKnownEvent(e) {
			t.Errorf("IsKnownEvent(%q) = false, want true", e)
		}
	}
	if IsKnownEvent("not-a-real-event") {
		t.Errorf("IsKnownEvent(not-a-real-event) = true, want false")
	}
	if IsKnownEvent("") {
		t.Errorf("IsKnownEvent(\"\") = true, want false")
	}
}

func TestRun_NoMatchingEventIsNoOp(t *testing.T) {
	// A pre-init hook should not fire when we dispatch post-init, even if
	// the command would otherwise error.
	hooks := []Hook{{Event: "pre-init", Command: "exit 1"}}
	if err := Run(context.Background(), hooks, "post-init", nil); err != nil {
		t.Fatalf("Run(non-matching event) = %v, want nil", err)
	}
}

func TestRun_EmptyHooksIsNoOp(t *testing.T) {
	if err := Run(context.Background(), nil, "pre-init", nil); err != nil {
		t.Fatalf("Run(nil hooks) = %v, want nil", err)
	}
	if err := Run(context.Background(), []Hook{}, "pre-init", nil); err != nil {
		t.Fatalf("Run([]) = %v, want nil", err)
	}
}

func TestRun_EnvInjection(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")

	hooks := []Hook{{
		Event:   "post-init",
		Command: `printf '%s\n%s' "$BOSUN_REPO_ROOT" "$BOSUN_SESSION_COUNT" > "$OUT_FILE"`,
	}}
	env := map[string]string{
		"BOSUN_REPO_ROOT":     "/tmp/myrepo",
		"BOSUN_SESSION_COUNT": "4",
		"OUT_FILE":            out,
	}
	if err := Run(context.Background(), hooks, "post-init", env); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read %s: %v", out, err)
	}
	if want := "/tmp/myrepo\n4"; string(got) != want {
		t.Fatalf("hook wrote %q, want %q", got, want)
	}
}

func TestRun_OrderPreservedAndOtherEventsSkipped(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "order.txt")
	hooks := []Hook{
		{Event: "post-init", Command: `printf 'a' >> "` + out + `"`},
		{Event: "pre-init", Command: `printf 'X' >> "` + out + `"`}, // skipped
		{Event: "post-init", Command: `printf 'b' >> "` + out + `"`},
		{Event: "post-init", Command: `printf 'c' >> "` + out + `"`},
	}
	if err := Run(context.Background(), hooks, "post-init", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "abc" {
		t.Fatalf("hook order produced %q, want %q", got, "abc")
	}
}

func TestRun_FailClosedReturnsError(t *testing.T) {
	hooks := []Hook{{Event: "pre-init", Command: "exit 7", FailOpen: false}}
	err := Run(context.Background(), hooks, "pre-init", nil)
	if err == nil {
		t.Fatal("Run(fail-closed, exit 7) = nil, want error")
	}
	if !strings.Contains(err.Error(), "pre-init") {
		t.Errorf("error %q should mention the event name", err)
	}
}

func TestRun_FailOpenSwallowsError(t *testing.T) {
	// FailOpen=true: the hook errors but Run returns nil. Subsequent hooks
	// in the same event must still run.
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran.txt")
	hooks := []Hook{
		{Event: "post-done", Command: "exit 1", FailOpen: true},
		{Event: "post-done", Command: `touch "` + marker + `"`},
	}
	if err := Run(context.Background(), hooks, "post-done", nil); err != nil {
		t.Fatalf("Run(fail-open) = %v, want nil", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("second hook did not run after first fail-open failure: %v", err)
	}
}

func TestRun_TimeoutKillsLongHook(t *testing.T) {
	hooks := []Hook{{
		Event:          "pre-init",
		Command:        "sleep 5",
		TimeoutSeconds: 1,
	}}
	start := time.Now()
	err := Run(context.Background(), hooks, "pre-init", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run(timeout) = nil, want error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q should mention timeout", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout took %s; should fire near 1s", elapsed)
	}
}

func TestRun_TimeoutFailOpenContinues(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "after.txt")
	hooks := []Hook{
		{Event: "post-init", Command: "sleep 5", TimeoutSeconds: 1, FailOpen: true},
		{Event: "post-init", Command: `touch "` + marker + `"`},
	}
	if err := Run(context.Background(), hooks, "post-init", nil); err != nil {
		t.Fatalf("Run = %v, want nil (fail-open swallows timeout)", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("hook after fail-open timeout did not run: %v", err)
	}
}

func TestRun_FirstHardFailureStopsLater(t *testing.T) {
	// A fail-closed failure must stop subsequent hooks for the same event.
	dir := t.TempDir()
	marker := filepath.Join(dir, "should-not-exist.txt")
	hooks := []Hook{
		{Event: "pre-init", Command: "exit 2"},
		{Event: "pre-init", Command: `touch "` + marker + `"`},
	}
	if err := Run(context.Background(), hooks, "pre-init", nil); err == nil {
		t.Fatal("expected fail-closed error")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("subsequent hook ran after fail-closed failure (created %s)", marker)
	}
}

func TestRun_EmptyCommandIsError(t *testing.T) {
	// An empty command is almost certainly a config typo. Surface it.
	hooks := []Hook{{Event: "pre-init", Command: ""}}
	if err := Run(context.Background(), hooks, "pre-init", nil); err == nil {
		t.Fatal("Run(empty command) = nil, want error")
	}
}

func TestRun_TimeoutIsErrTimeoutSentinel(t *testing.T) {
	// errors.Is must classify a timeout as ErrTimeout so callers can
	// render a distinct message without parsing the error string.
	hooks := []Hook{{
		Event:          "pre-init",
		Command:        "sleep 5",
		TimeoutSeconds: 1,
	}}
	err := Run(context.Background(), hooks, "pre-init", nil)
	if err == nil {
		t.Fatal("Run(timeout) = nil, want error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected errors.Is(err, ErrTimeout); err = %v", err)
	}
	if !strings.Contains(err.Error(), "timed out after 1s") {
		t.Errorf("error %q should include the timeout duration", err)
	}
}

func TestRun_ZeroTimeoutUsesDefault(t *testing.T) {
	// TimeoutSeconds==0 must not mean "infinite". v0.6 turns zero into the
	// 30-second default. A `sleep 60` hook with TimeoutSeconds==0 should
	// time out (after <=30s) rather than block indefinitely. We assert
	// the upper bound conservatively (~32s) because slow CI can drift a
	// second or two; the point is that the hook DOES return, not that
	// it returns in exactly 30s.
	if testing.Short() {
		t.Skip("skipping 30s default-timeout test in -short mode")
	}
	hooks := []Hook{{
		Event:   "pre-init",
		Command: "sleep 60",
		// TimeoutSeconds left at zero on purpose.
	}}
	start := time.Now()
	err := Run(context.Background(), hooks, "pre-init", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run(default-timeout) = nil, want error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected errors.Is(err, ErrTimeout); err = %v", err)
	}
	if !strings.Contains(err.Error(), "30s") {
		t.Errorf("error %q should mention 30s default", err)
	}
	if elapsed > 32*time.Second {
		t.Errorf("default timeout took %s; expected ~30s", elapsed)
	}
}
