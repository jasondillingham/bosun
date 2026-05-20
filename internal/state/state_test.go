package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/session"
)

func TestMarkDone_ReplacesStuck(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.MarkStuck("session-1", "blocked on review"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone("session-1", "shipped"); err != nil {
		t.Fatal(err)
	}
	st, body, err := s.Read(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if st != session.StateDone {
		t.Fatalf("state = %s, want DONE", st)
	}
	if !strings.Contains(body, "shipped") {
		t.Fatalf("body missing message: %q", body)
	}
}

func TestMarkStuck_ReplacesDone(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_ = s.MarkDone("session-1", "")
	if err := s.MarkStuck("session-1", "merge conflict"); err != nil {
		t.Fatal(err)
	}
	st, body, err := s.Read(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if st != session.StateStuck {
		t.Fatalf("state = %s, want STUCK", st)
	}
	if !strings.Contains(body, "merge conflict") {
		t.Fatalf("body missing message: %q", body)
	}
}

func TestRead_Missing(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	st, body, err := s.Read(dir, "session-99")
	if err != nil {
		t.Fatal(err)
	}
	if st != session.StateWorking {
		t.Fatalf("state = %s, want WORKING", st)
	}
	if body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
}

// TestLoadAll_FiltersFinderDuplicates pins the phantom-file filter from
// roadmap ask #4: macOS Spotlight / iCloud occasionally clone marker
// files inside .bosun/state/ (`session-1 2.done`, `session-1 (1).json`,
// …). LoadAll must strip those so `bosun list` / `bosun status` don't
// surface ghost sessions.
func TestLoadAll_FiltersFinderDuplicates(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".bosun", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"session-1.done",
		"session-1 2.done",   // Spotlight duplicate
		"session-1.json",     // hypothetical .json sibling
		"session-1 2.json",   // Spotlight duplicate of json
		"session-1 (1).done", // iCloud duplicate
	} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	s := NewStore(dir)
	got, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 1 || got[0] != "session-1" {
		t.Fatalf("LoadAll = %v, want [session-1]", got)
	}
}

// TestLoadAll_MissingDir returns an empty result without erroring so
// callers iterating markers on a fresh repo don't have to special-case
// "no .bosun/state/ yet".
func TestLoadAll_MissingDir(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	got, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll on missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LoadAll = %v, want empty", got)
	}
}

// TestEnsureSpotlightMarker_CreatesAndIsIdempotent pins the marker drop
// from roadmap ask #4 — the macOS-documented filename Spotlight honors
// to stop indexing the .bosun/ directory tree. Second call is a no-op.
func TestEnsureSpotlightMarker_CreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureSpotlightMarker(dir); err != nil {
		t.Fatalf("EnsureSpotlightMarker: %v", err)
	}
	marker := filepath.Join(dir, ".bosun", ".metadata_never_index")
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("marker size = %d, want 0", info.Size())
	}
	// Second call: still nil, file unchanged.
	if err := EnsureSpotlightMarker(dir); err != nil {
		t.Fatalf("second EnsureSpotlightMarker: %v", err)
	}
}

// TestAttachedPID_WriteReadClear pins the basic round-trip on the
// attached-pid registration. Write → Attached reports the pid; Clear
// → Attached reports ok=false; Clear on a missing file is a no-op.
func TestAttachedPID_WriteReadClear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if pid, ok, err := s.Attached(dir, "session-1"); err != nil || ok || pid != 0 {
		t.Fatalf("Attached(empty) = (%d, %v, %v), want (0, false, nil)", pid, ok, err)
	}
	if err := s.WriteAttachedPID("session-1", 12345); err != nil {
		t.Fatal(err)
	}
	pid, ok, err := s.Attached(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || pid != 12345 {
		t.Fatalf("Attached after write = (%d, %v), want (12345, true)", pid, ok)
	}
	if err := s.ClearAttachedPID("session-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Attached(dir, "session-1"); ok {
		t.Fatal("Attached after clear should report ok=false")
	}
	// Second clear is a no-op.
	if err := s.ClearAttachedPID("session-1"); err != nil {
		t.Fatalf("second ClearAttachedPID (missing) = %v, want nil", err)
	}
}

// TestAttachedPID_MalformedBodyTreatedAsAbsent: a hand-edited or
// truncated file shouldn't poison the liveness gate. The reader
// returns ok=false so callers fall back to the proc-scan path rather
// than surfacing a parse error to the operator.
func TestAttachedPID_MalformedBodyTreatedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".bosun", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "  \n", "not-a-pid", "-1\n", "0\n"} {
		if err := os.WriteFile(filepath.Join(stateDir, "session-1.attached-pid"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s := NewStore(dir)
		pid, ok, err := s.Attached(dir, "session-1")
		if err != nil {
			t.Errorf("Attached(body=%q) err = %v, want nil", body, err)
		}
		if ok || pid != 0 {
			t.Errorf("Attached(body=%q) = (%d, %v), want (0, false)", body, pid, ok)
		}
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_ = s.MarkDone("session-1", "")
	if err := s.Clear("session-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear("session-1"); err != nil {
		t.Fatalf("second Clear (missing) = %v, want nil", err)
	}
	st, _, _ := s.Read(dir, "session-1")
	if st != session.StateWorking {
		t.Fatalf("state after clear = %s, want WORKING", st)
	}
}

// TestAgentCommand_WriteReadClear walks the persistence lifecycle for
// the per-session agent command (Phase 1 of agent-command-design.md):
// missing → write → read → Clear-includes-it. Mirrors
// TestAttachedPID_WriteReadClear's pattern so a future reviewer can
// see the surface is symmetric.
func TestAgentCommand_WriteReadClear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Missing file: ok=false, no error.
	if cmd, ok, err := s.ReadAgentCommand(dir, "session-1"); err != nil || ok || cmd != "" {
		t.Fatalf("ReadAgentCommand(missing) = (%q, %v, %v), want (\"\", false, nil)", cmd, ok, err)
	}

	if err := s.WriteAgentCommand("session-1", "./scripts/ollama-claude.sh"); err != nil {
		t.Fatal(err)
	}
	cmd, ok, err := s.ReadAgentCommand(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cmd != "./scripts/ollama-claude.sh" {
		t.Fatalf("ReadAgentCommand after write = (%q, %v), want (%q, true)", cmd, ok, "./scripts/ollama-claude.sh")
	}

	// Clear must drop the agent-command marker alongside done/stuck.
	// Without this, a reaped session's stale command would resurface
	// the next time a label slot got reused.
	if err := s.Clear("session-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ReadAgentCommand(dir, "session-1"); ok {
		t.Errorf("ReadAgentCommand after Clear should report ok=false")
	}
}

// TestAgentCommand_EmptyRejected guards against persisting an empty
// override — that's a programming error, not "fall back to default."
// Callers wanting the fallback should call Clear instead.
func TestAgentCommand_EmptyRejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.WriteAgentCommand("session-1", ""); err == nil {
		t.Errorf("WriteAgentCommand(\"\") returned nil; want error")
	}
}

// TestDockerHost_WriteReadClear walks the persistence lifecycle for
// the per-session Docker host (Phase 3 lane 4 of remote-docker-plan.md):
// missing → write → read → Clear-includes-it. Mirrors the agent-command
// pattern so a future reviewer can see the surface is symmetric.
func TestDockerHost_WriteReadClear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Missing file: ok=false, no error.
	if host, ok, err := s.ReadDockerHost(dir, "session-1"); err != nil || ok || host != "" {
		t.Fatalf("ReadDockerHost(missing) = (%q, %v, %v), want (\"\", false, nil)", host, ok, err)
	}

	if err := s.WriteDockerHost("session-1", "ssh://thor"); err != nil {
		t.Fatal(err)
	}
	host, ok, err := s.ReadDockerHost(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || host != "ssh://thor" {
		t.Fatalf("ReadDockerHost after write = (%q, %v), want (%q, true)", host, ok, "ssh://thor")
	}

	// Clear must drop the docker-host marker alongside done/stuck/
	// agent-command. Without this, a reaped session's stale host
	// would resurface the next time a label slot got reused — and
	// `bosun cleanup` would try to talk to the wrong daemon.
	if err := s.Clear("session-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ReadDockerHost(dir, "session-1"); ok {
		t.Errorf("ReadDockerHost after Clear should report ok=false")
	}
}

// TestDockerHost_EmptyRejected guards against persisting an empty host
// — that's a programming error, not "fall back to local docker."
// Callers wanting the no-override semantics should not call
// WriteDockerHost at all.
func TestDockerHost_EmptyRejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.WriteDockerHost("session-1", ""); err == nil {
		t.Errorf("WriteDockerHost(\"\") returned nil; want error")
	}
}

// TestDockerHost_MalformedBodyTreatedAsAbsent mirrors the
// agent-command pattern: a hand-edited or truncated file shouldn't
// strand the operator with a poison value. Multi-line bodies in
// particular get treated as absent so a future format change
// (multi-line endpoints) doesn't get silently parsed as a single line.
func TestDockerHost_MalformedBodyTreatedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".bosun", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "  \n", "ssh://thor\nssh://other\n"} {
		if err := os.WriteFile(filepath.Join(stateDir, "session-1.docker-host"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s := NewStore(dir)
		host, ok, err := s.ReadDockerHost(dir, "session-1")
		if err != nil {
			t.Errorf("ReadDockerHost(body=%q) err = %v, want nil", body, err)
		}
		if ok || host != "" {
			t.Errorf("ReadDockerHost(body=%q) = (%q, %v), want (\"\", false)", body, host, ok)
		}
	}
}

// TestAgentCommand_MalformedBodyTreatedAsAbsent mirrors the
// attached-pid pattern: a hand-edited or truncated file shouldn't
// strand the operator with a poison value. Multi-line bodies in
// particular get treated as absent so a future format change
// (multi-line config) doesn't get silently parsed as a single line.
func TestAgentCommand_MalformedBodyTreatedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".bosun", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "  \n", "line one\nline two\n"} {
		if err := os.WriteFile(filepath.Join(stateDir, "session-1.agent-command"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s := NewStore(dir)
		cmd, ok, err := s.ReadAgentCommand(dir, "session-1")
		if err != nil {
			t.Errorf("ReadAgentCommand(body=%q) err = %v, want nil", body, err)
		}
		if ok || cmd != "" {
			t.Errorf("ReadAgentCommand(body=%q) = (%q, %v), want (\"\", false)", body, cmd, ok)
		}
	}
}
