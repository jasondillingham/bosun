package state

import (
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
