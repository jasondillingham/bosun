package state

import (
	"runtime"
	"sync"
	"testing"

	"github.com/jasondillingham/bosun/internal/session"
)

// TestMarkConcurrent_AlwaysOneMarker exercises the interleaving of
// MarkDone and MarkStuck on the same session. Without a lock,
// write(done)→write(stuck)→remove(stuck)→remove(done) leaves NEITHER
// marker on disk, silently demoting the session back to WORKING. With
// the mutex, every observation must report DONE or STUCK.
func TestMarkConcurrent_AlwaysOneMarker(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := s.MarkDone("session-1", "shipped"); err != nil {
				t.Errorf("MarkDone: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := s.MarkStuck("session-1", "blocked"); err != nil {
				t.Errorf("MarkStuck: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// After all writes settle, the final state must be DONE or STUCK,
	// never WORKING — both writers always leave one marker present.
	st, _, err := s.Read(dir, "session-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st == session.StateWorking {
		t.Fatalf("final state = WORKING, want DONE or STUCK (race wiped both markers)")
	}
}

// TestMarkConcurrent_CrossProcessFlock simulates two bosun processes
// (CLI vs. MCP daemon) racing on the same state directory by giving each
// goroutine its OWN Store instance. Each Store has a fresh sync.Mutex,
// so this exercise relies entirely on the cross-process flock in
// withStateLock to serialize MarkDone vs. MarkStuck — the in-process
// mutex provides no coverage across distinct Store instances.
//
// Without the flock, the marker-wipe interleaving
//
//	A.write(.done) → B.write(.stuck) → A.remove(.stuck) → B.remove(.done)
//
// leaves NEITHER marker on disk and the session reads as WORKING.
// With the flock, every observation after the goroutines finish must
// be DONE or STUCK.
func TestMarkConcurrent_CrossProcessFlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lockfile.WithLock is a no-op on Windows builds")
	}
	dir := t.TempDir()
	storeA := NewStore(dir)
	storeB := NewStore(dir)

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := storeA.MarkDone("session-1", "shipped"); err != nil {
				t.Errorf("storeA.MarkDone: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := storeB.MarkStuck("session-1", "blocked"); err != nil {
				t.Errorf("storeB.MarkStuck: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Read via a third Store so the assertion isn't accidentally coupled
	// to whichever Store happened to hold mu last.
	reader := NewStore(dir)
	st, _, err := reader.Read(dir, "session-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st == session.StateWorking {
		t.Fatalf("final state = WORKING across distinct Store instances; cross-process flock did not serialize")
	}
}

// TestClearConcurrent_CrossProcessFlock pairs Clear against MarkDone to
// catch the second flavor of the race: Clear racing MarkDone could remove
// the marker MarkDone just wrote, leaving the session as WORKING when the
// MarkDone caller believed it had succeeded.
//
// The test runs concurrent MarkDone (storeA, fixed iter count) and Clear
// (storeB, looping until storeA finishes) for the contention window, then
// issues one synchronous MarkDone *after* the Clear loop has stopped. With
// the flock, that final MarkDone is the strictly last mutator and Read
// must see DONE. Without the flock, the assertion catches a torn ordering
// only when the race happens to align — but the more frequent failure
// mode (Clear wiping mid-iteration writes) is also covered by the
// MarkDone goroutine reporting t.Errorf on a write error, and by the
// final state being DONE rather than WORKING.
func TestClearConcurrent_CrossProcessFlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lockfile.WithLock is a no-op on Windows builds")
	}
	dir := t.TempDir()
	storeA := NewStore(dir)
	storeB := NewStore(dir)

	const iters = 200
	stopClear := make(chan struct{})
	clearDone := make(chan struct{})
	go func() {
		defer close(clearDone)
		for {
			select {
			case <-stopClear:
				return
			default:
				if err := storeB.Clear("session-1"); err != nil {
					t.Errorf("Clear: %v", err)
					return
				}
			}
		}
	}()
	for i := 0; i < iters; i++ {
		if err := storeA.MarkDone("session-1", ""); err != nil {
			t.Errorf("MarkDone: %v", err)
			break
		}
	}
	close(stopClear)
	<-clearDone

	if err := storeA.MarkDone("session-1", "final"); err != nil {
		t.Fatalf("final MarkDone: %v", err)
	}
	st, _, err := storeB.Read(dir, "session-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st != session.StateDone {
		t.Fatalf("final state = %s, want DONE (Clear wiped a marker that MarkDone just wrote)", st)
	}
}
