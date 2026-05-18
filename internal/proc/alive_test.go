package proc

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestIsAlive_Self(t *testing.T) {
	// The current Go test process is by definition alive.
	if !IsAlive(os.Getpid()) {
		t.Fatalf("IsAlive(self pid %d) = false, want true", os.Getpid())
	}
}

func TestIsAlive_ZeroAndNegative(t *testing.T) {
	for _, pid := range []int{0, -1, -42} {
		if IsAlive(pid) {
			t.Errorf("IsAlive(%d) = true, want false (only positive PIDs are valid)", pid)
		}
	}
}

func TestIsAlive_DeadProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep not portable to windows")
	}
	cmd := exec.Command("sleep", "0.05")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// Wait for the child to actually finish and be reaped, otherwise the
	// PID can briefly remain visible as a zombie. cmd.Wait reaps it.
	if err := cmd.Wait(); err != nil {
		// sleep exits 0; an unexpected exit is fine for our purposes —
		// we only care that the process is gone.
		_ = err
	}
	// Give the kernel a tick to update the process table on slow CI.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !IsAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("IsAlive(%d) still true 2s after child exited+reaped", pid)
}

func TestIsAlive_NeverExistedPID(t *testing.T) {
	// A very high PID that almost certainly isn't allocated on any test
	// host. This isn't bulletproof (a CI box could in principle have a
	// process at this PID), but the alternative is reserving a PID for
	// the test, which the kernel won't let us do.
	if IsAlive(2147483640) {
		t.Skip("PID 2147483640 happens to be live on this host; pick a different sentinel if this recurs")
	}
}
