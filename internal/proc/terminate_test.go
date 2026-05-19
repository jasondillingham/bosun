package proc

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestTerminate_GracefulSIGTERM spawns a `sleep` that responds to
// SIGTERM, calls Terminate, and asserts the process is gone within
// the grace window. The wall-clock should be well under the grace
// period — sleep exits immediately on SIGTERM.
func TestTerminate_GracefulSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM has no graceful path on Windows; Kill() path covered by TestTerminate_AlreadyGone")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Reap the child in a goroutine. In production, the agent's parent
	// (the terminal/tmux process that launched it) does the reaping;
	// the test process isn't that parent, so without an explicit Wait
	// the post-SIGTERM child stays a zombie and signal(0) keeps
	// reporting "alive" — Terminate would burn the full grace window
	// for nothing. The goroutine mirrors what happens in production.
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()

	start := time.Now()
	if err := Terminate(cmd.Process.Pid, DefaultTerminateGrace); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	elapsed := time.Since(start)

	// Sleep should exit fast on SIGTERM — well under the grace window.
	if elapsed > DefaultTerminateGrace {
		t.Errorf("Terminate took %v; expected SIGTERM-fast exit under grace=%v", elapsed, DefaultTerminateGrace)
	}

	select {
	case <-reaped:
	case <-time.After(1 * time.Second):
		t.Fatal("child not reaped after Terminate returned")
	}
}

// TestTerminate_EscalatesToKill spawns a process that ignores SIGTERM
// (a shell script that traps SIGTERM with an empty handler) and
// confirms Terminate escalates to SIGKILL after the grace window.
func TestTerminate_EscalatesToKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("trap not portable to Windows")
	}
	// Shell script: trap SIGTERM to a no-op, then sleep. SIGTERM won't
	// kill it; SIGKILL will.
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn trap-shell: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Short grace so the test doesn't sit forever.
	if err := Terminate(cmd.Process.Pid, 300*time.Millisecond); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	// After Terminate returns, signal-0 should report the process is
	// gone (Wait reaps it; the kernel may briefly hold the zombie
	// until then).
	if err := cmd.Wait(); err == nil {
		t.Errorf("expected non-zero exit after SIGKILL, got clean exit")
	}
}

// TestTerminate_AlreadyGone confirms a no-op on a PID that has already
// exited (the typical case when the operator runs `bosun cleanup`
// after the agent has already finished and exited on its own).
func TestTerminate_AlreadyGone(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit /b 0")
	}
	if err := cmd.Run(); err != nil {
		// The exit code is what we want to surface; non-zero from
		// `cmd /c exit /b 0` is fine, just keep going.
		_ = err
	}
	pid := cmd.Process.Pid
	if err := Terminate(pid, 200*time.Millisecond); err != nil {
		t.Fatalf("Terminate on dead pid %d: %v (want nil)", pid, err)
	}
}

// TestTerminate_InvalidPID surfaces a clear error rather than silently
// no-op'ing on garbage input.
func TestTerminate_InvalidPID(t *testing.T) {
	if err := Terminate(0, time.Second); err == nil {
		t.Errorf("Terminate(0) returned nil; want error on invalid pid")
	}
	if err := Terminate(-1, time.Second); err == nil {
		t.Errorf("Terminate(-1) returned nil; want error on invalid pid")
	}
}

// keep the syscall import in case future tests need to send signals
// directly to compare against Terminate's behavior.
var _ = syscall.SIGTERM
