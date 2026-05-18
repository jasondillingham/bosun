package subtask

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveCount_MissingDirReturnsZero(t *testing.T) {
	tmp := t.TempDir()
	n, err := ActiveCount(tmp, "session-1")
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("ActiveCount = %d, want 0 for missing dir", n)
	}
}

func TestActiveCount_CountsRecordsAndExcludesCancelled(t *testing.T) {
	tmp := t.TempDir()
	if err := CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := CreateForTest(tmp, "session-1", "session-1.sub.2"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := CreateForTest(tmp, "session-1", "session-1.sub.3"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Mark sub.2 cancelled — it should drop out of the active count.
	out, err := Cancel(tmp, "session-1", "session-1.sub.2")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !out.Wrote {
		t.Fatalf("Cancel did not write marker: %+v", out)
	}

	n, err := ActiveCount(tmp, "session-1")
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	if n != 2 {
		t.Fatalf("ActiveCount = %d, want 2 (3 records minus 1 cancelled)", n)
	}
}

func TestActiveCount_ZeroOneAndN(t *testing.T) {
	tmp := t.TempDir()

	if n, _ := ActiveCount(tmp, "session-1"); n != 0 {
		t.Fatalf("empty: ActiveCount = %d, want 0", n)
	}

	if err := CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, _ := ActiveCount(tmp, "session-1"); n != 1 {
		t.Fatalf("one: ActiveCount = %d, want 1", n)
	}

	for i := 2; i <= 5; i++ {
		if err := CreateForTest(tmp, "session-1", "session-1.sub."+itoa(i)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if n, _ := ActiveCount(tmp, "session-1"); n != 5 {
		t.Fatalf("N: ActiveCount = %d, want 5", n)
	}
}

func TestCancel_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	if err := CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := Cancel(tmp, "session-1", "session-1.sub.1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !out.Found || !out.ParentMatches || out.AlreadyCancelled || !out.Wrote {
		t.Fatalf("Cancel happy-path outcome = %+v", out)
	}
	marker := filepath.Join(tmp, ".bosun", "subtasks", "session-1", "session-1.sub.1.cancelled")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
}

func TestCancel_InvalidID(t *testing.T) {
	tmp := t.TempDir()
	out, err := Cancel(tmp, "session-1", "nope")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if out.Found || out.Wrote {
		t.Fatalf("Cancel for missing id should report not found: %+v", out)
	}
}

func TestCancel_ParentMismatch(t *testing.T) {
	tmp := t.TempDir()
	if err := CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := Cancel(tmp, "session-2", "session-1.sub.1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !out.Found {
		t.Fatalf("id should be found under session-1: %+v", out)
	}
	if out.ParentMatches {
		t.Fatalf("Cancel from session-2 should be a parent-mismatch, not a match: %+v", out)
	}
	if out.Wrote {
		t.Fatalf("Cancel should not have written marker on parent-mismatch: %+v", out)
	}
	// And the marker should not have been written.
	marker := filepath.Join(tmp, ".bosun", "subtasks", "session-1", "session-1.sub.1.cancelled")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker present after parent-mismatch refusal: err=%v", err)
	}
}

func TestCancel_AlreadyCancelled(t *testing.T) {
	tmp := t.TempDir()
	if err := CreateForTest(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Cancel(tmp, "session-1", "session-1.sub.1"); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	out, err := Cancel(tmp, "session-1", "session-1.sub.1")
	if err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if !out.Found || !out.ParentMatches {
		t.Fatalf("second Cancel should still find the id under the right parent: %+v", out)
	}
	if !out.AlreadyCancelled {
		t.Fatalf("second Cancel should report already-cancelled: %+v", out)
	}
	if out.Wrote {
		t.Fatalf("second Cancel should not re-write the marker: %+v", out)
	}
}

func TestCountsForSessions(t *testing.T) {
	tmp := t.TempDir()
	_ = CreateForTest(tmp, "session-1", "session-1.sub.1")
	_ = CreateForTest(tmp, "session-1", "session-1.sub.2")
	_ = CreateForTest(tmp, "session-3", "session-3.sub.1")
	counts, err := CountsForSessions(tmp, []string{"session-1", "session-2", "session-3"})
	if err != nil {
		t.Fatalf("CountsForSessions: %v", err)
	}
	if counts["session-1"] != 2 {
		t.Errorf("session-1 = %d, want 2", counts["session-1"])
	}
	if counts["session-2"] != 0 {
		t.Errorf("session-2 = %d, want 0", counts["session-2"])
	}
	if counts["session-3"] != 1 {
		t.Errorf("session-3 = %d, want 1", counts["session-3"])
	}
}

// itoa avoids dragging strconv into a one-call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
