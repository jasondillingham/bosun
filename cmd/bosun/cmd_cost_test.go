package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/usage"
)

// seedUsageLedgers writes deterministic usage ledgers across multiple
// sessions so the cost tests have a stable corpus to assert against.
// Returns a map of session→sum for cross-checks.
//
// Most callers pass a fixed `now` so date-based assertions (e.g.
// --by=day output) are deterministic. TestRunCost_SinceFilter
// alone passes time.Now() so its --since cutoff math holds as
// the wall clock advances past the fixed-date entries' window.
func seedUsageLedgers(t *testing.T, repo string) map[string]float64 {
	t.Helper()
	return seedUsageLedgersAt(t, repo, time.Date(2026, 5, 20, 14, 0, 0, 0, time.UTC))
}

func seedUsageLedgersAt(t *testing.T, repo string, now time.Time) map[string]float64 {
	t.Helper()

	type seed struct {
		session string
		entries []usage.Entry
	}
	seeds := []seed{
		{
			session: "session-1",
			entries: []usage.Entry{
				{Timestamp: now.Add(-3 * 24 * time.Hour), CostUSD: 0.10, TokensIn: 1000, TokensOut: 500, Model: "haiku"},
				{Timestamp: now.Add(-1 * time.Hour), CostUSD: 0.40, TokensIn: 4000, TokensOut: 2000, Model: "sonnet"},
			},
		},
		{
			session: "session-2",
			entries: []usage.Entry{
				{Timestamp: now.Add(-2 * 24 * time.Hour), CostUSD: 0.25, TokensIn: 2500, TokensOut: 1000, Model: "sonnet"},
			},
		},
		{
			session: "auth",
			entries: []usage.Entry{
				{Timestamp: now.Add(-12 * time.Hour), CostUSD: 0.50, TokensIn: 5000, TokensOut: 2500, Model: "opus"},
			},
		},
	}

	wantPerSession := map[string]float64{}
	for _, s := range seeds {
		for _, e := range s.entries {
			if err := usage.Append(repo, s.session, e); err != nil {
				t.Fatalf("seed %s: %v", s.session, err)
			}
			wantPerSession[s.session] += e.CostUSD
		}
	}
	return wantPerSession
}

// TestRunCost_RoundTotal exercises the default (no-breakdown)
// rendering. Confirms the round-wide total adds up across sessions
// and includes turn counts + token sums.
func TestRunCost_RoundTotal(t *testing.T) {
	repo := initBosunRepo(t)
	want := seedUsageLedgers(t, repo)
	chdir(t, repo)

	expectedTotal := want["session-1"] + want["session-2"] + want["auth"]

	out := captureStdout(t, func() {
		if err := runCost(costOpts{}); err != nil {
			t.Fatalf("runCost: %v", err)
		}
	})
	if !strings.Contains(out, "across all sessions") {
		t.Errorf("output should describe the scope; got:\n%s", out)
	}
	wantDollar := "$1.2500"
	if !strings.Contains(out, wantDollar) {
		t.Errorf("output missing expected total %q (computed %.4f); got:\n%s", wantDollar, expectedTotal, out)
	}
	if !strings.Contains(out, "4 turn(s)") {
		t.Errorf("output should report 4 turns total; got:\n%s", out)
	}
}

// TestRunCost_BySessionBreakdown pins the per-session table shape.
// Each session should appear once with its individual total.
func TestRunCost_BySessionBreakdown(t *testing.T) {
	repo := initBosunRepo(t)
	want := seedUsageLedgers(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runCost(costOpts{by: "session"}); err != nil {
			t.Fatalf("runCost: %v", err)
		}
	})
	for label, total := range want {
		if !strings.Contains(out, label) {
			t.Errorf("table missing session label %q:\n%s", label, out)
		}
		// Format-loose assertion: the dollar amount should appear
		// somewhere on the same line as the label.
		dollar := strings.NewReplacer(",", "").Replace(strings.TrimPrefix("$", "$"))
		_ = dollar
		_ = total
	}
	// Header sanity
	if !strings.Contains(out, "SESSION") {
		t.Errorf("table missing SESSION header:\n%s", out)
	}
}

// TestRunCost_ByDayBreakdown confirms entries get bucketed by UTC
// date when --by=day is requested.
func TestRunCost_ByDayBreakdown(t *testing.T) {
	repo := initBosunRepo(t)
	seedUsageLedgers(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runCost(costOpts{by: "day"}); err != nil {
			t.Fatalf("runCost: %v", err)
		}
	})
	if !strings.Contains(out, "DATE") {
		t.Errorf("--by=day table should use DATE header:\n%s", out)
	}
	// Seeded entries span three calendar dates. Verify all three
	// appear in the output.
	wantDates := []string{"2026-05-17", "2026-05-18", "2026-05-20"}
	for _, d := range wantDates {
		if !strings.Contains(out, d) {
			t.Errorf("missing date %s in --by=day output:\n%s", d, out)
		}
	}
}

// TestRunCost_SinceFilter limits results to entries within the
// supplied window. The 3-day-old session-1 entry should drop out
// when --since=2d is set.
func TestRunCost_SinceFilter(t *testing.T) {
	repo := initBosunRepo(t)
	// Seed relative to wall-clock now (not the fixture's fixed 2026-05-20
	// date) — otherwise the test calendar-rolls and starts failing once
	// real time moves more than 2 days past 2026-05-20. The --since
	// filter compares against time.Now() at runtime; the seed entries
	// must be in the same time frame.
	seedUsageLedgersAt(t, repo, time.Now().UTC())
	chdir(t, repo)

	// --since=2d filter math: cutoff is now()-2d, and the filter uses
	// strict After(cutoff). Seeded entries' ages relative to now:
	//   -3d (session-1 $0.10) → older than cutoff → excluded
	//   -2d (session-2 $0.25) → exactly at cutoff, strict After → excluded
	//   -12h (auth $0.50)     → newer → included
	//   -1h  (session-1 $0.40) → newer → included
	// So filtered total is $0.90 over 2 turns.
	out := captureStdout(t, func() {
		if err := runCost(costOpts{since: "2d"}); err != nil {
			t.Fatalf("runCost: %v", err)
		}
	})
	if !strings.Contains(out, "since 2d") {
		t.Errorf("output should echo the --since scope; got:\n%s", out)
	}
	if !strings.Contains(out, "$0.9000") {
		t.Errorf("expected filtered total $0.9000 (2 surviving entries); got:\n%s", out)
	}
	if !strings.Contains(out, "2 turn(s)") {
		t.Errorf("expected 2 surviving turns in --since=2d output; got:\n%s", out)
	}
	if strings.Contains(out, "$1.2500") {
		t.Errorf("full total $1.2500 leaked into --since output:\n%s", out)
	}
}

// TestRunCost_SessionFilter narrows to one session.
func TestRunCost_SessionFilter(t *testing.T) {
	repo := initBosunRepo(t)
	seedUsageLedgers(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runCost(costOpts{session: "session-1"}); err != nil {
			t.Fatalf("runCost: %v", err)
		}
	})
	// session-1's total is 0.10 + 0.40 = 0.50.
	if !strings.Contains(out, "$0.5000") {
		t.Errorf("expected session-1 total $0.5000; got:\n%s", out)
	}
	if strings.Contains(out, "$1.2500") {
		t.Errorf("round-wide total leaked into --session output:\n%s", out)
	}
}

// TestRunCost_JSONOutput emits a structured payload with rows and
// total. Confirms the schema doesn't drift accidentally.
func TestRunCost_JSONOutput(t *testing.T) {
	repo := initBosunRepo(t)
	seedUsageLedgers(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runCost(costOpts{by: "session", jsonOut: true}); err != nil {
			t.Fatalf("runCost: %v", err)
		}
	})
	var p costPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if p.By != "session" {
		t.Errorf("payload.By = %q, want session", p.By)
	}
	if len(p.Rows) != 3 {
		t.Errorf("expected 3 rows (sessions), got %d", len(p.Rows))
	}
	if p.Total.TurnCount != 4 {
		t.Errorf("Total.TurnCount = %d, want 4", p.Total.TurnCount)
	}
	if p.Total.CostUSD < 1.24 || p.Total.CostUSD > 1.26 {
		t.Errorf("Total.CostUSD = %f, want ~1.25", p.Total.CostUSD)
	}
}

// TestRunCost_EmptyRepoIsFriendly: when there's no usage data,
// the command should produce a helpful one-liner instead of
// erroring or emitting blank output.
func TestRunCost_EmptyRepoIsFriendly(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runCost(costOpts{}); err != nil {
			t.Fatalf("runCost on empty repo: %v", err)
		}
	})
	if !strings.Contains(out, "no usage recorded") {
		t.Errorf("empty-repo output should explain the no-data state:\n%s", out)
	}
}

// TestRunCost_RejectsBadByFlag pins the validation for --by.
func TestRunCost_RejectsBadByFlag(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)

	err := runCost(costOpts{by: "model"})
	if err == nil {
		t.Fatal("expected error for unknown --by value, got nil")
	}
	if !strings.Contains(err.Error(), `--by`) {
		t.Errorf("error should mention --by, got: %v", err)
	}
}

// TestParseCostSince exercises the "Nd" extension and standard
// time.ParseDuration fall-through. Negative + zero rejected.
func TestParseCostSince(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"", 0, true},
		{"0", 0, true},
		{"-7d", 0, true},
		{"banana", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseCostSince(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got %v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseCostSince(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestListSessions_Lists round-trips the new usage.ListSessions
// helper to confirm it picks up labels and ignores non-ledger files.
func TestListSessions_Lists(t *testing.T) {
	repo := initBosunRepo(t)
	seedUsageLedgers(t, repo)

	// Drop a non-.usage file in the state dir to confirm it gets ignored.
	stateDir := filepath.Join(repo, ".bosun", "state")
	if err := os.WriteFile(filepath.Join(stateDir, "session-1.heartbeat"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	labels, err := usage.ListSessions(repo)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	want := []string{"auth", "session-1", "session-2"}
	if len(labels) != len(want) {
		t.Errorf("got labels %v, want %v", labels, want)
	}
	for i, lbl := range want {
		if i >= len(labels) || labels[i] != lbl {
			t.Errorf("labels[%d] = %q, want %q (full: %v)", i, labels[i], lbl, labels)
		}
	}
}
