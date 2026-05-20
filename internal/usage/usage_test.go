package usage

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendRead_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	if err := Append(dir, "session-1", Entry{
		Timestamp: now,
		TokensIn:  1234,
		TokensOut: 567,
		CostUSD:   0.0234,
		Model:     "claude-sonnet-4.6",
		TurnLabel: "design",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := Append(dir, "session-1", Entry{
		Timestamp: now.Add(2 * time.Minute),
		TokensIn:  890,
		TokensOut: 123,
		CostUSD:   0.0098,
		Model:     "claude-haiku-4.5",
	}); err != nil {
		t.Fatalf("Append #2: %v", err)
	}

	entries, err := Read(dir, "session-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].TokensIn != 1234 || entries[0].Model != "claude-sonnet-4.6" {
		t.Errorf("first entry mismatch: %+v", entries[0])
	}
	if entries[1].Model != "claude-haiku-4.5" {
		t.Errorf("second entry model: %q", entries[1].Model)
	}
}

func TestReadTotals_SumsAcrossModels(t *testing.T) {
	dir := t.TempDir()
	for i, model := range []string{"opus", "opus", "haiku"} {
		if err := Append(dir, "session-1", Entry{
			Timestamp: time.Date(2026, 5, 20, 12, i, 0, 0, time.UTC),
			TokensIn:  100,
			TokensOut: 50,
			CostUSD:   0.10,
			Model:     model,
		}); err != nil {
			t.Fatal(err)
		}
	}

	tot, err := ReadTotals(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if tot.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", tot.TurnCount)
	}
	if tot.TokensIn != 300 || tot.TokensOut != 150 {
		t.Errorf("token sum: in=%d out=%d, want 300/150", tot.TokensIn, tot.TokensOut)
	}
	if tot.CostUSD < 0.299 || tot.CostUSD > 0.301 {
		t.Errorf("CostUSD = %f, want ~0.30", tot.CostUSD)
	}
	// LastModel is the most-recent entry's model.
	if tot.LastModel != "haiku" {
		t.Errorf("LastModel = %q, want haiku", tot.LastModel)
	}
}

// TestRead_AbsentFileReturnsZero pins the "no usage recorded" path:
// a session that never called bosun_usage shouldn't error or surface
// nil — Read returns no entries, ReadTotals returns the zero value.
func TestRead_AbsentFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	entries, err := Read(dir, "session-1")
	if err != nil {
		t.Fatalf("Read absent: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
	tot, err := ReadTotals(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if tot.TurnCount != 0 || tot.CostUSD != 0 {
		t.Errorf("totals not zero: %+v", tot)
	}
}

// TestRead_MalformedLinesSkipped: a partially-written entry from a
// crashed turn (garbage on its line) shouldn't poison the rest of
// the ledger. Read drops malformed lines silently.
func TestRead_MalformedLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, stateDirRel)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One good line, one half-written corrupt line, another good line.
	body := `{"ts":"2026-05-20T12:00:00Z","tokens_in":10,"tokens_out":5,"cost_usd":0.01,"model":"x"}
{"ts":"2026-05-20T12:01
{"ts":"2026-05-20T12:02:00Z","tokens_in":20,"tokens_out":10,"cost_usd":0.02,"model":"y"}
`
	if err := os.WriteFile(filepath.Join(stateDir, "session-1.usage"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := Read(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 well-formed entries, got %d", len(entries))
	}
	tot, _ := ReadTotals(dir, "session-1")
	if tot.TurnCount != 2 || tot.TokensIn != 30 {
		t.Errorf("totals over malformed file wrong: %+v", tot)
	}
}

// TestAppend_RejectsNegativeValues guards against operator-side
// programming errors that would otherwise pollute the ledger.
func TestAppend_RejectsNegativeValues(t *testing.T) {
	dir := t.TempDir()
	cases := []Entry{
		{TokensIn: -1, TokensOut: 0, CostUSD: 0},
		{TokensIn: 0, TokensOut: -5, CostUSD: 0},
		{TokensIn: 0, TokensOut: 0, CostUSD: -0.01},
	}
	for i, c := range cases {
		if err := Append(dir, "session-1", c); err == nil {
			t.Errorf("case %d: expected error for negative value, got nil (entry=%+v)", i, c)
		}
	}
}

// TestAppend_RejectsEmptySession surfaces the "forgot to thread the
// label" bug before the ledger file ends up at .bosun/state/.usage
// (a hidden, easy-to-miss filename).
func TestAppend_RejectsEmptySession(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "", Entry{TokensIn: 1, CostUSD: 0.01}); err == nil {
		t.Error("expected error for empty session name")
	}
}

// TestAppend_AutoStampsZeroTimestamp: when callers omit Timestamp,
// Append fills it with time.Now(). Lets the bosun_usage MCP tool
// stay simple — agents don't have to send a timestamp.
func TestAppend_AutoStampsZeroTimestamp(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().UTC()
	if err := Append(dir, "session-1", Entry{TokensIn: 1, CostUSD: 0.01, Model: "x"}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()

	entries, _ := Read(dir, "session-1")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	ts := entries[0].Timestamp
	if ts.Before(before) || ts.After(after) {
		t.Errorf("auto-stamped timestamp %v outside [%v, %v]", ts, before, after)
	}
}

// TestAppend_ConcurrentSafety pins the POSIX atomic-append claim:
// many concurrent Append calls of sub-PIPE_BUF lines don't interleave
// and produce N entries when Read'd back. Run with -race to catch
// any in-process write races.
func TestAppend_ConcurrentSafety(t *testing.T) {
	dir := t.TempDir()
	const n = 50

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_ = Append(dir, "session-1", Entry{
				TokensIn:  i,
				TokensOut: i * 2,
				CostUSD:   float64(i) * 0.01,
				Model:     "concurrent",
			})
		}(i)
	}
	wg.Wait()

	entries, err := Read(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Errorf("expected %d entries from concurrent appends, got %d", n, len(entries))
	}
}

// TestClear_RemovesLedger pins the cleanup contract — when a session
// is reaped, its usage ledger goes too so a future label-reuse starts
// fresh.
func TestClear_RemovesLedger(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "session-1", Entry{TokensIn: 1, CostUSD: 0.01}); err != nil {
		t.Fatal(err)
	}
	if err := Clear(dir, "session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(usagePath(dir, "session-1")); !os.IsNotExist(err) {
		t.Errorf("ledger file should be gone after Clear, stat returned: %v", err)
	}
	// Idempotent — second Clear on missing file is fine.
	if err := Clear(dir, "session-1"); err != nil {
		t.Errorf("second Clear on missing file: %v", err)
	}
}

// TestPerModel_BreaksdownByModel: renderers want to show cost per
// model, not just one opaque total. PerModel + SortedModels together
// give "claude-opus: $1.42 · claude-haiku: $0.03" output.
func TestPerModel_BreaksdownByModel(t *testing.T) {
	entries := []Entry{
		{CostUSD: 1.00, Model: "opus"},
		{CostUSD: 0.50, Model: "haiku"},
		{CostUSD: 0.25, Model: "opus"},
		{CostUSD: 0.10, Model: ""}, // unknown bucket
	}
	m := PerModel(entries)
	if got := m["opus"]; got < 1.249 || got > 1.251 {
		t.Errorf("opus = %f, want 1.25", got)
	}
	if got := m["haiku"]; got < 0.499 || got > 0.501 {
		t.Errorf("haiku = %f, want 0.50", got)
	}
	if got := m["unknown"]; got < 0.099 || got > 0.101 {
		t.Errorf("unknown = %f, want 0.10", got)
	}

	sorted := SortedModels(m)
	// Most expensive first, ties alphabetical.
	want := []string{"opus", "haiku", "unknown"}
	if !sliceEq(sorted, want) {
		t.Errorf("SortedModels = %v, want %v", sorted, want)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRead_HandlesAtypicallyLongLine confirms the bufio.Scanner buffer
// override accommodates very-large entries (e.g. agent wrapper that
// includes a verbose turn_label). 1MB cap is more than reasonable.
func TestRead_HandlesAtypicallyLongLine(t *testing.T) {
	dir := t.TempDir()
	longLabel := strings.Repeat("x", 100*1024) // 100KB label
	if err := Append(dir, "session-1", Entry{
		TokensIn:  10,
		CostUSD:   0.01,
		Model:     "x",
		TurnLabel: longLabel,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if len(entries[0].TurnLabel) != len(longLabel) {
		t.Errorf("long turn_label truncated: got %d chars, want %d", len(entries[0].TurnLabel), len(longLabel))
	}
}
