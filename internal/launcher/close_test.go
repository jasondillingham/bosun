package launcher

import (
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

// TestCloseTmuxWindow_HappyPath asserts the right tmux argv when
// TMUX is set and kill-window succeeds.
func TestCloseTmuxWindow_HappyPath(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,12345,0")

	var captured []string
	orig := closeRunFn
	closeRunFn = func(cmd *exec.Cmd) error {
		captured = append([]string(nil), cmd.Args...)
		return nil
	}
	t.Cleanup(func() { closeRunFn = orig })

	if err := CloseTmuxWindow("session-1"); err != nil {
		t.Fatalf("CloseTmuxWindow returned %v, want nil", err)
	}

	want := []string{"tmux", "kill-window", "-t", "session-1"}
	if !reflect.DeepEqual(captured, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", captured, want)
	}
}

// TestCloseTmuxWindow_NotInsideTmux confirms the helper no-ops without
// calling tmux when the operator isn't inside a tmux session. The
// launcher wouldn't have created a tmux window either, so there's
// nothing to close.
func TestCloseTmuxWindow_NotInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "")

	called := false
	orig := closeRunFn
	closeRunFn = func(cmd *exec.Cmd) error {
		called = true
		return nil
	}
	t.Cleanup(func() { closeRunFn = orig })

	if err := CloseTmuxWindow("session-1"); err != nil {
		t.Fatalf("CloseTmuxWindow returned %v, want nil", err)
	}
	if called {
		t.Errorf("closeRunFn was invoked when TMUX is unset; helper should no-op")
	}
}

// TestCloseTmuxWindow_TmuxFailureSurfaced asserts the caller gets a
// non-nil error when tmux is reachable AND kill-window fails — e.g.
// the window doesn't exist or tmux itself crashed. The caller's
// log-and-continue policy handles both subcases the same way, so the
// helper doesn't classify.
func TestCloseTmuxWindow_TmuxFailureSurfaced(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,12345,0")

	want := errors.New("can't find window: session-1")
	orig := closeRunFn
	closeRunFn = func(cmd *exec.Cmd) error { return want }
	t.Cleanup(func() { closeRunFn = orig })

	if got := CloseTmuxWindow("session-1"); !errors.Is(got, want) {
		t.Errorf("CloseTmuxWindow returned %v, want %v", got, want)
	}
}

// TestCloseTmuxWindow_EmptyName guards against a programming error
// where the caller passes an empty label. tmux would treat empty `-t`
// as "current window" — closing whatever the operator is looking at —
// which is the worst possible thing to do silently.
func TestCloseTmuxWindow_EmptyName(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,12345,0")

	called := false
	orig := closeRunFn
	closeRunFn = func(cmd *exec.Cmd) error {
		called = true
		return nil
	}
	t.Cleanup(func() { closeRunFn = orig })

	if err := CloseTmuxWindow(""); err == nil {
		t.Errorf("CloseTmuxWindow(\"\") returned nil; want error to prevent killing the operator's current window")
	}
	if called {
		t.Errorf("closeRunFn was invoked with empty name; helper should refuse pre-spawn")
	}
}
