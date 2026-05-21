//go:build !windows

package lockfile

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithLock_RunsFn(t *testing.T) {
	dir := t.TempDir()
	ran := false
	err := WithLock(filepath.Join(dir, "test.lock"), func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run")
	}
}

func TestWithLock_PropagatesFnError(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("boom")
	got := WithLock(filepath.Join(dir, "test.lock"), func() error {
		return want
	})
	if !errors.Is(got, want) {
		t.Fatalf("err=%v, want %v", got, want)
	}
}

func TestWithLockResult_ReturnsPayload(t *testing.T) {
	dir := t.TempDir()
	v, err := WithLockResult(filepath.Join(dir, "test.lock"), func() (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != 42 {
		t.Fatalf("got %d, want 42", v)
	}
}

// TestWithLock_TimeoutSurfacesLockTimeoutError pins the v0.12 M5 fix:
// when the lock is held longer than DefaultTimeout, the second
// caller gets a *LockTimeoutError instead of blocking indefinitely.
// The error also unwraps to ErrLockTimeout so sentinel-style checks
// work.
func TestWithLock_TimeoutSurfacesLockTimeoutError(t *testing.T) {
	prev := DefaultTimeout
	DefaultTimeout = 150 * time.Millisecond
	t.Cleanup(func() { DefaultTimeout = prev })

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "timeout.lock")

	holderInside := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- WithLock(lockPath, func() error {
			close(holderInside)
			<-releaseHolder
			return nil
		})
	}()
	<-holderInside
	defer func() { close(releaseHolder); <-holderDone }()

	start := time.Now()
	err := WithLock(lockPath, func() error {
		t.Fatal("contended fn should not have run")
		return nil
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want errors.Is == ErrLockTimeout", err)
	}
	var timeoutErr *LockTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want *LockTimeoutError via errors.As", err)
	}
	if timeoutErr.Path != lockPath {
		t.Errorf("Path = %q, want %q", timeoutErr.Path, lockPath)
	}
	if timeoutErr.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", timeoutErr.Timeout, DefaultTimeout)
	}
	// The holder writes its PID inside the locked section. Same-process
	// holder means the reported PID equals os.Getpid().
	if timeoutErr.HolderPID == 0 {
		t.Error("HolderPID = 0, want a real PID (writeLockHolder should have stamped the lock file)")
	}
	// Sanity: total elapsed should be within an order of magnitude of
	// the timeout. Wide tolerance to keep this stable under CI load.
	if elapsed < DefaultTimeout {
		t.Errorf("elapsed = %v, want >= %v", elapsed, DefaultTimeout)
	}
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, want < 5s (timeout shouldn't take that long)", elapsed)
	}
}

// TestWithLock_TimeoutErrorMessage pins the operator-facing message
// shape — the error string must mention the lock path, holder PID,
// and a duration. Future drift in the formatter gets caught.
func TestWithLock_TimeoutErrorMessage(t *testing.T) {
	err := &LockTimeoutError{
		Path:      "/repo/.bosun/state/.lock",
		Timeout:   30 * time.Second,
		HolderPID: 12345,
		HeldFor:   42 * time.Second,
	}
	msg := err.Error()
	for _, want := range []string{"/repo/.bosun/state/.lock", "PID 12345", "30s"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n  got: %s", want, msg)
		}
	}
	// No-holder branch should still surface the path + timeout.
	noHolder := &LockTimeoutError{Path: "/x.lock", Timeout: 10 * time.Second}
	if !strings.Contains(noHolder.Error(), "contended") {
		t.Errorf("no-holder branch should say 'contended'; got: %s", noHolder.Error())
	}
}

// TestWithLock_Serializes is the core contract: two concurrent callers
// must not both be inside fn at the same time. Mirrors the test that
// previously lived in mcp_autostart_test.go for the withMcpSpawnLock
// helper this package replaced.
func TestWithLock_Serializes(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	const callers = 8
	var inFlight, maxInFlight int32

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			err := WithLock(lockPath, func() error {
				cur := atomic.AddInt32(&inFlight, 1)
				for {
					prev := atomic.LoadInt32(&maxInFlight)
					if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
						break
					}
				}
				time.Sleep(15 * time.Millisecond)
				atomic.AddInt32(&inFlight, -1)
				return nil
			})
			if err != nil {
				t.Errorf("WithLock: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("max concurrent fn calls = %d, want 1 (lock did not serialize)", got)
	}
}
