package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Schema-lock test for the event objects emitted on `GET /api/events`
// and persisted to `.bosun/events.log`. Documented in
// `docs/json-schema.md`. If you add, rename, retype, or omit a key,
// this test fails — update the doc and the lock list together.
//
// The Event struct is the single source of truth for both the SSE wire
// format and the JSONL on-disk format; locking the marshaled key set
// covers both.
var eventJSON_LockedKeys = []string{"session", "kind", "message", "at"}

func TestSchema_EventJSON_LockedKeys(t *testing.T) {
	e := Event{
		Session: "session-1",
		Kind:    "progress",
		Message: "halfway",
		At:      time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}

	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string(nil), eventJSON_LockedKeys...)
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Event key set mismatch — update docs/json-schema.md and this lock list when intentional.\n  want: %v\n  got:  %v",
			want, got)
	}

	// Spot-check types: the time format must be RFC3339 — round-tripping
	// through `time.Parse` proves the wire shape without locking the
	// exact string. Consumers can rely on RFC3339 indefinitely.
	atStr, ok := obj["at"].(string)
	if !ok {
		t.Errorf("at: want string, got %T", obj["at"])
	}
	if _, err := time.Parse(time.RFC3339, atStr); err != nil {
		t.Errorf("at value %q is not RFC3339: %v", atStr, err)
	}

	// Locked decision (see F5 in docs/json-schema.md): no version field
	// on event records. A consumer can rely on `kind` being open-ended
	// (info/progress/warn today, possibly more later).
	if _, ok := obj["version"]; ok {
		t.Errorf("Event must NOT emit a 'version' field (see F5 in docs/json-schema.md). Adding one is a deliberate breaking-change moment.")
	}
	if _, ok := obj["v"]; ok {
		t.Errorf("Event must NOT emit a 'v' field (see F5 in docs/json-schema.md)")
	}
}

func TestEvents_PushAndRecent(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := Push(Event{
			Session: "session-1",
			Kind:    "info",
			Message: "msg",
			At:      now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}

	got := Recent(3)
	if len(got) != 3 {
		t.Fatalf("Recent(3) returned %d, want 3", len(got))
	}
	// Chronological — oldest of the three first; most recent last.
	if !got[0].At.Equal(now.Add(2 * time.Second)) {
		t.Errorf("Recent(3)[0] = %v, want %v", got[0].At, now.Add(2*time.Second))
	}
	if !got[2].At.Equal(now.Add(4 * time.Second)) {
		t.Errorf("Recent(3)[2] = %v, want %v", got[2].At, now.Add(4*time.Second))
	}
}

func TestEvents_RingBufferCap(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	for i := 0; i < eventBufCap+50; i++ {
		_ = Push(Event{Session: "s", Kind: "info", Message: "m", At: time.Now()})
	}
	// Asking for more than the cap should clamp to the cap.
	got := Recent(eventBufCap + 100)
	if len(got) != eventBufCap {
		t.Fatalf("Recent(over) = %d, want %d (ring cap)", len(got), eventBufCap)
	}
}

func TestEvents_RecentZeroOrNegative(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)
	_ = Push(Event{Session: "s", Kind: "info", Message: "m", At: time.Now()})

	if got := Recent(0); got != nil {
		t.Errorf("Recent(0) = %v, want nil", got)
	}
	if got := Recent(-5); got != nil {
		t.Errorf("Recent(-5) = %v, want nil", got)
	}
}

func TestEvents_PersistAppendsJSONL(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	SetEventsLog(logPath)

	if err := Push(Event{Session: "session-1", Kind: "info", Message: "hello", At: time.Now().UTC()}); err != nil {
		t.Fatalf("push 1: %v", err)
	}
	if err := Push(Event{Session: "session-2", Kind: "progress", Message: "halfway", At: time.Now().UTC()}); err != nil {
		t.Fatalf("push 2: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], `"session":"session-1"`) || !strings.Contains(lines[0], `"message":"hello"`) {
		t.Errorf("line 1 missing fields: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"kind":"progress"`) {
		t.Errorf("line 2 missing kind: %s", lines[1])
	}
}

func TestEvents_TailReadsLastN(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	SetEventsLog(logPath)

	base := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if err := Push(Event{
			Session: "session-1",
			Kind:    "info",
			Message: "m",
			At:      base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}

	got, err := TailEvents(logPath, 3)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("TailEvents(3) = %d, want 3", len(got))
	}
	if !got[0].At.Equal(base.Add(7 * time.Second)) {
		t.Errorf("first tail event = %v, want %v", got[0].At, base.Add(7*time.Second))
	}
	if !got[2].At.Equal(base.Add(9 * time.Second)) {
		t.Errorf("last tail event = %v, want %v", got[2].At, base.Add(9*time.Second))
	}
}

func TestEvents_TailHandlesLargeFile(t *testing.T) {
	// Force the backwards-scan loop to iterate by pushing more bytes than
	// one chunk holds. Each line is ~120 bytes; 200 of them ≈ 24 KB > 4 KB
	// chunk size, so the scan reads multiple chunks before it has 5 records.
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	SetEventsLog(logPath)

	for i := 0; i < 200; i++ {
		if err := Push(Event{
			Session: "session-" + string(rune('a'+(i%26))),
			Kind:    "info",
			Message: strings.Repeat("x", 64),
			At:      time.Now().UTC(),
		}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}

	got, err := TailEvents(logPath, 5)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("TailEvents(5) over large file = %d, want 5", len(got))
	}
}

func TestEvents_TailMissingFile(t *testing.T) {
	got, err := TailEvents(filepath.Join(t.TempDir(), "no-such.log"), 5)
	if err != nil {
		t.Fatalf("tail missing: unexpected error %v", err)
	}
	if got != nil {
		t.Errorf("tail missing = %v, want nil", got)
	}
}

// TestEvents_RotatesAtMaxBytes pins the security-audit H2 fix:
// events.log must roll over to events.log.1 (and so on) when it
// would exceed eventsLogMaxBytes. Without rotation, an agent
// calling bosun_announce on a loop could fill the disk over a
// long-running session. We shrink the cap via direct call into
// rotateEventsIfNeeded so the test doesn't have to write 10MB.
func TestEvents_RotatesAtMaxBytes(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")

	// Seed the file with a fake current size by writing 10MB+ of bytes
	// directly. Rotation triggers at eventsLogMaxBytes (10 MiB).
	bigContent := make([]byte, eventsLogMaxBytes+1)
	for i := range bigContent {
		bigContent[i] = 'x'
	}
	if err := os.WriteFile(logPath, bigContent, 0o600); err != nil {
		t.Fatalf("seed oversize log: %v", err)
	}

	SetEventsLog(logPath)
	if err := Push(Event{Session: "s", Kind: "info", Message: "post-rotate"}); err != nil {
		t.Fatalf("push after oversize: %v", err)
	}

	// After the rotation, events.log.1 should hold the original
	// oversize content and the active events.log should be just the
	// one new entry.
	rotated, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("expected events.log.1 after rotation: %v", err)
	}
	if rotated.Size() != int64(eventsLogMaxBytes+1) {
		t.Errorf("events.log.1 size = %d, want %d (the pre-rotation bytes)",
			rotated.Size(), eventsLogMaxBytes+1)
	}
	active, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read active log: %v", err)
	}
	if !strings.Contains(string(active), "post-rotate") {
		t.Errorf("active events.log missing post-rotate entry: %s", string(active))
	}
	// Active file should only have one line (just the new entry) —
	// rotation moved everything else away.
	lines := strings.Count(strings.TrimRight(string(active), "\n"), "\n") + 1
	if lines != 1 {
		t.Errorf("active log has %d lines, want 1 after rotation: %s",
			lines, string(active))
	}
}

// TestEvents_RotationKeepsAtMostNCopies confirms the
// eventsLogMaxFiles cap — older rotated copies past .5 get
// dropped, so an infinite stream of rotations doesn't itself grow
// disk usage unbounded.
func TestEvents_RotationKeepsAtMostNCopies(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	SetEventsLog(logPath)

	// Pre-seed all eventsLogMaxFiles rotated slots so the next
	// rotation has to drop the oldest.
	for i := 1; i <= eventsLogMaxFiles; i++ {
		body := []byte(fmt.Sprintf("rotated-slot-%d\n", i))
		if err := os.WriteFile(fmt.Sprintf("%s.%d", logPath, i), body, 0o600); err != nil {
			t.Fatalf("seed slot %d: %v", i, err)
		}
	}
	// Plus the active log at oversize so a rotation actually fires.
	if err := os.WriteFile(logPath, make([]byte, eventsLogMaxBytes+1), 0o600); err != nil {
		t.Fatalf("seed oversize log: %v", err)
	}

	if err := Push(Event{Session: "s", Kind: "info", Message: "trigger-rotate"}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Slot N+1 must not exist (drop-the-oldest enforced).
	beyond := fmt.Sprintf("%s.%d", logPath, eventsLogMaxFiles+1)
	if _, err := os.Stat(beyond); err == nil {
		t.Errorf("%s exists; rotation kept too many copies", beyond)
	}
	// Slot N must hold what slot N-1 used to hold (the shift went
	// through). The shifted slot 4 now lives at slot 5.
	tailBody, err := os.ReadFile(fmt.Sprintf("%s.%d", logPath, eventsLogMaxFiles))
	if err != nil {
		t.Fatalf("read final slot: %v", err)
	}
	wantTailMarker := fmt.Sprintf("rotated-slot-%d", eventsLogMaxFiles-1)
	if !strings.Contains(string(tailBody), wantTailMarker) {
		t.Errorf("slot %d = %q, want it to contain %q (shift didn't happen)",
			eventsLogMaxFiles, string(tailBody), wantTailMarker)
	}
}

func TestEvents_TailSkipsCorruptLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	// Hand-write a log with two valid records sandwiching one corrupt line.
	contents := `{"session":"s","kind":"info","message":"first","at":"2026-05-15T12:00:00Z"}
not json at all
{"session":"s","kind":"info","message":"third","at":"2026-05-15T12:00:02Z"}
`
	if err := os.WriteFile(logPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	got, err := TailEvents(logPath, 10)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 valid events (corrupt skipped), got %d: %+v", len(got), got)
	}
	if got[0].Message != "first" || got[1].Message != "third" {
		t.Errorf("unexpected events: %+v", got)
	}
}
