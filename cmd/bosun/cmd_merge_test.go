package main

import (
	"strings"
	"testing"
)

// findMergeEntry returns the newest match; ties broken in append order
// from newest to oldest. Cover the three lookup forms: session name,
// SHA prefix on pre, SHA prefix on post.
func TestFindMergeEntry_NewestSessionMatchWins(t *testing.T) {
	entries := []mergeLogEntry{
		{Session: "session-1", Pre: "aaaa1111", Post: "bbbb2222"},
		{Session: "session-2", Pre: "cccc3333", Post: "dddd4444"},
		{Session: "session-1", Pre: "eeee5555", Post: "ffff6666"}, // newest session-1
	}
	got, ok := findMergeEntry(entries, "session-1")
	if !ok {
		t.Fatal("expected match")
	}
	if got.Pre != "eeee5555" {
		t.Errorf("got pre=%q, want newest session-1 entry (eeee5555)", got.Pre)
	}
}

func TestFindMergeEntry_ByPreSHAPrefix(t *testing.T) {
	entries := []mergeLogEntry{
		{Session: "a", Pre: "abcdef0123456789", Post: "post11111111"},
		{Session: "b", Pre: "fedcba9876543210", Post: "post22222222"},
	}
	got, ok := findMergeEntry(entries, "abcdef")
	if !ok {
		t.Fatal("expected match")
	}
	if got.Session != "a" {
		t.Errorf("got session=%q, want a", got.Session)
	}
}

func TestFindMergeEntry_ByPostSHAPrefix(t *testing.T) {
	entries := []mergeLogEntry{
		{Session: "a", Pre: "abc11111", Post: "abc22222"},
		{Session: "b", Pre: "def33333", Post: "def44444"},
	}
	got, ok := findMergeEntry(entries, "def4")
	if !ok {
		t.Fatal("expected match")
	}
	if got.Session != "b" {
		t.Errorf("got session=%q, want b", got.Session)
	}
}

func TestFindMergeEntry_ShortPrefixRejected(t *testing.T) {
	// SHA prefix lookups require ≥4 chars; otherwise spurious matches
	// would be too easy (any 2-char hex string matches a third of all
	// SHAs). 3-char queries fall through to "not found" unless they
	// happen to be a session name.
	entries := []mergeLogEntry{
		{Session: "x", Pre: "abcd1111", Post: "ef001111"},
	}
	if _, ok := findMergeEntry(entries, "abc"); ok {
		t.Fatal("expected no match for 3-char prefix")
	}
}

func TestFindMergeEntry_Missing(t *testing.T) {
	entries := []mergeLogEntry{
		{Session: "a", Pre: "abc11111", Post: "abc22222"},
	}
	if _, ok := findMergeEntry(entries, "session-nope"); ok {
		t.Fatal("expected no match")
	}
	if _, ok := findMergeEntry(nil, "anything"); ok {
		t.Fatal("expected no match on empty log")
	}
}

func TestTruncateForMergeLog(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"merge: bosun/session-1", "merge: bosun/session-1"},
		{"first line\nsecond line", "first line"},
		{strings.Repeat("a", 100), strings.Repeat("a", 77) + "..."},
		{"", ""},
	}
	for _, tc := range cases {
		got := truncateForMergeLog(tc.in)
		if got != tc.want {
			t.Errorf("truncateForMergeLog(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAppendAndReadMergeLog(t *testing.T) {
	repo := t.TempDir()
	entries := []mergeLogEntry{
		{TS: "2026-05-16T10:00:00Z", Session: "session-1", Pre: "aaaa", Post: "bbbb", SquashMsg: "first"},
		{TS: "2026-05-16T11:00:00Z", Session: "session-2", Pre: "cccc", Post: "dddd", SquashMsg: "second"},
	}
	for _, e := range entries {
		if err := appendMergeLog(repo, e); err != nil {
			t.Fatalf("appendMergeLog: %v", err)
		}
	}
	got, err := readMergeLog(repo)
	if err != nil {
		t.Fatalf("readMergeLog: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("readMergeLog got %d entries, want 2", len(got))
	}
	for i := range entries {
		if got[i] != entries[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], entries[i])
		}
	}
}

func TestReadMergeLog_MissingFileIsNotAnError(t *testing.T) {
	// Fresh repos won't have .bosun/merges.log yet — readers must
	// treat that as "no history", not a failure.
	entries, err := readMergeLog(t.TempDir())
	if err != nil {
		t.Fatalf("readMergeLog on missing file: %v", err)
	}
	if entries != nil {
		t.Errorf("readMergeLog on missing file: got %v, want nil", entries)
	}
}

// TestConflictErr_MapsToExitConflict pins the Bughunt-1 F032 fix: a
// merge that halts on conflict returns conflictErr, which exitCodeFor
// translates to exit code 4 (exitConflict). Pre-fix the merge returned
// nil and CI scripts kept marching with a wedged working tree.
func TestConflictErr_MapsToExitConflict(t *testing.T) {
	err := conflictErr("merge halted at conflict")
	if err == nil {
		t.Fatal("conflictErr returned nil")
	}
	if code := exitCodeFor(err); code != exitConflict {
		t.Errorf("exitCodeFor(conflictErr) = %d, want %d (exitConflict)", code, exitConflict)
	}
	// The error message must surface "bosun: " prefix so any operator
	// who DOES see it (e.g. SilenceErrors=false at the command level)
	// recognizes the source.
	if msg := err.Error(); !strings.HasPrefix(msg, "bosun:") {
		t.Errorf("conflictErr.Error() = %q, want bosun: prefix", msg)
	}
}

// TestExitCodeFor_KindCoverage pins the full kind→exit-code map so the
// next person adding an errKind notices they need to add an exit code
// (and a test row).
func TestExitCodeFor_KindCoverage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitOK},
		{"user", userErr("bad"), exitUserErr},
		{"git", gitErr("op", nil), exitGitErr},
		{"internal", internalErr("op", nil), exitInternal},
		{"conflict", conflictErr("merge halted"), exitConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFor(tc.err); got != tc.want {
				t.Errorf("exitCodeFor(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}
