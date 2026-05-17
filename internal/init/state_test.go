package initstate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoad_MissingReturnsNotExist(t *testing.T) {
	// A repo that's never run `bosun init` has no state file; callers branch
	// on errors.Is(err, fs.ErrNotExist) to take the "fresh init" path.
	dir := t.TempDir()
	got, err := Load(dir)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load(missing) err = %v, want fs.ErrNotExist", err)
	}
	if got != nil {
		t.Fatalf("Load(missing) = %+v, want nil", got)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New([]string{"session-1", "session-2", "session-3"}, "plan.md")
	s.CurrentSession = "session-2"
	s.CurrentStep = StepGitWorktreeAdd
	s.CompletedSessions = []string{"session-1"}

	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != stateVersion {
		t.Errorf("Version = %q, want %q", got.Version, stateVersion)
	}
	if got.PlanPath != "plan.md" {
		t.Errorf("PlanPath = %q, want plan.md", got.PlanPath)
	}
	if got.TotalSessions != 3 {
		t.Errorf("TotalSessions = %d, want 3", got.TotalSessions)
	}
	if got.CurrentSession != "session-2" {
		t.Errorf("CurrentSession = %q, want session-2", got.CurrentSession)
	}
	if got.CurrentStep != StepGitWorktreeAdd {
		t.Errorf("CurrentStep = %q, want %s", got.CurrentStep, StepGitWorktreeAdd)
	}
	if len(got.CompletedSessions) != 1 || got.CompletedSessions[0] != "session-1" {
		t.Errorf("CompletedSessions = %v, want [session-1]", got.CompletedSessions)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	if Exists(dir) {
		t.Fatal("Exists(empty) = true, want false")
	}
	if err := New([]string{"session-1"}, "").Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !Exists(dir) {
		t.Fatal("Exists(after-save) = false, want true")
	}
}

func TestMarkComplete_PersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s := New([]string{"session-1", "session-2"}, "")
	if err := s.SetCurrent(dir, "session-1", StepBranchCreate); err != nil {
		t.Fatalf("SetCurrent: %v", err)
	}
	if err := s.MarkComplete(dir, "session-1"); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.IsCompleted("session-1") {
		t.Errorf("session-1 should be completed after reload: %+v", got)
	}
	if got.CurrentSession != "" {
		t.Errorf("CurrentSession should clear when marked complete; got %q", got.CurrentSession)
	}
	if got.CurrentStep != "" {
		t.Errorf("CurrentStep should clear when marked complete; got %q", got.CurrentStep)
	}
}

func TestMarkComplete_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s := New([]string{"session-1", "session-2"}, "")
	if err := s.MarkComplete(dir, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkComplete(dir, "session-1"); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.CompletedSessions) != 1 {
		t.Errorf("MarkComplete duplicates appended; got %v", got.CompletedSessions)
	}
}

func TestClear_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	s := New([]string{"session-1"}, "")
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	if !Exists(dir) {
		t.Fatal("state file should exist before Clear")
	}
	if err := s.Clear(dir); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if Exists(dir) {
		t.Fatal("state file should be gone after Clear")
	}
	// Clear of an already-missing file is a no-op, not an error.
	if err := s.Clear(dir); err != nil {
		t.Errorf("second Clear should be a no-op; got %v", err)
	}
	if err := ClearFile(dir); err != nil {
		t.Errorf("ClearFile on missing should be no-op; got %v", err)
	}
}

func TestSave_AtomicWriteLeavesNoTmp(t *testing.T) {
	// The temp file written by Save should be renamed into place, not
	// linger on disk. A leaked .tmp would confuse operator inspection
	// and risk being mistaken for the real state file.
	dir := t.TempDir()
	s := New([]string{"session-1", "session-2"}, "plan.md")
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, dirRelative))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestSave_ProducesValidJSON(t *testing.T) {
	dir := t.TempDir()
	s := New([]string{"session-1", "session-2"}, "plan.md")
	s.CompletedSessions = []string{"session-1"}
	s.CurrentSession = "session-2"
	s.CurrentStep = StepGitWorktreeAdd
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("on-disk state is not valid JSON: %v\n%s", err, data)
	}
	for _, key := range []string{"version", "started_at", "total_sessions", "current_session", "current_step", "completed_sessions"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("on-disk state missing %q field: %s", key, data)
		}
	}
}

func TestSessionLabels_StoresExplicitLabels(t *testing.T) {
	// Named-session init must preserve the operator's label list so
	// `bosun init --resume` with no args can still re-derive auth/http/storage
	// rather than fabricating session-1..session-N.
	s := New([]string{"auth", "http", "storage"}, "plan.md")
	got := s.SessionLabels()
	want := []string{"auth", "http", "storage"}
	if len(got) != len(want) {
		t.Fatalf("SessionLabels len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SessionLabels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSessionLabels_NumberedFallback_ForOlderStateFiles(t *testing.T) {
	// State files written before the Labels field landed only carry
	// TotalSessions. To keep older state files resumable, SessionLabels
	// falls back to the numbered scheme (session-1..session-N) — the only
	// label form bosun creates without an explicit list.
	s := &InitState{TotalSessions: 3}
	got := s.SessionLabels()
	want := []string{"session-1", "session-2", "session-3"}
	if len(got) != len(want) {
		t.Fatalf("SessionLabels len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SessionLabels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSessionLabels_PersistsAcrossReload(t *testing.T) {
	// After Save/Load, the labels list must survive intact — that's what
	// `bosun init --resume` with no args reads to derive the label set.
	dir := t.TempDir()
	s := New([]string{"auth", "http"}, "")
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	labels := got.SessionLabels()
	if len(labels) != 2 || labels[0] != "auth" || labels[1] != "http" {
		t.Errorf("labels after reload = %v, want [auth http]", labels)
	}
}

// TestConcurrentWritesDoNotTear is the analogue of internal/state's
// TestMarkConcurrent_CrossProcessFlock: two distinct *InitState instances
// (modelling two processes) hammering Save on the same file must produce
// a final on-disk state that parses cleanly. Without the flock, Save's
// write-then-rename can leak partial files; with it, every reader sees
// a complete JSON document. Each goroutine uses its own InitState pointer
// so the in-process sync.Mutex provides no coverage; the flock is the
// only thing serialising them.
func TestConcurrentWritesDoNotTear(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lockfile.WithLock is a no-op on Windows builds")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, dirRelative), 0o755); err != nil {
		t.Fatal(err)
	}

	const iters = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s := New([]string{"session-1", "session-2", "session-3"}, "plan.md")
		for i := 0; i < iters; i++ {
			s.CurrentSession = "session-A"
			if err := s.Save(dir); err != nil {
				t.Errorf("writer A: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		s := New([]string{"session-1", "session-2", "session-3"}, "plan.md")
		for i := 0; i < iters; i++ {
			s.CurrentSession = "session-B"
			if err := s.Save(dir); err != nil {
				t.Errorf("writer B: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Final read must parse successfully — torn writes would surface as a
	// JSON unmarshal error.
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after concurrent writes: %v", err)
	}
	if got.CurrentSession != "session-A" && got.CurrentSession != "session-B" {
		t.Errorf("CurrentSession = %q, want one of session-A/session-B (writes torn?)", got.CurrentSession)
	}
}
