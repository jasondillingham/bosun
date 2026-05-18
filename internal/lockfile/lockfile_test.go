//go:build !windows

package lockfile

import (
	"errors"
	"path/filepath"
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
