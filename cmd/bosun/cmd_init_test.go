package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUptimeLoad_macOS(t *testing.T) {
	// macOS `uptime` uses "load averages:" with space-separated values.
	got, err := parseUptimeLoad("14:32  up 1:23, 2 users, load averages: 1.23 1.45 1.67\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1.23 {
		t.Errorf("want 1.23, got %v", got)
	}
}

func TestParseUptimeLoad_Linux(t *testing.T) {
	// Linux `uptime` uses "load average:" (singular) with comma separators.
	got, err := parseUptimeLoad(" 14:32:01 up 12 days,  3:45,  1 user,  load average: 7.42, 5.10, 3.88\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7.42 {
		t.Errorf("want 7.42, got %v", got)
	}
}

func TestParseUptimeLoad_Malformed(t *testing.T) {
	if _, err := parseUptimeLoad("nothing useful here"); err == nil {
		t.Fatal("expected error for malformed uptime output")
	}
}

func TestGetLoadAverage_RespectsEnvOverride(t *testing.T) {
	// BOSUN_TEST_LOAD_AVERAGE is the seam scenario tests use to inject a
	// synthetic load from outside the process. Cover it directly so the
	// contract doesn't silently rot.
	t.Setenv("BOSUN_TEST_LOAD_AVERAGE", "12.5")
	got, err := readSystemLoadAverage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 12.5 {
		t.Errorf("want 12.5, got %v", got)
	}
}

func TestGetLoadAverage_StubbableViaVar(t *testing.T) {
	// getLoadAverage is a package-level var precisely so unit tests can
	// inject a value without depending on the host's real load.
	orig := getLoadAverage
	t.Cleanup(func() { getLoadAverage = orig })
	getLoadAverage = func() (float64, error) { return 9.9, nil }
	got, err := getLoadAverage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 9.9 {
		t.Errorf("want 9.9, got %v", got)
	}
}

func TestFindPhantomBranchRefs_DetectsFinderDuplicates(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".git", "refs", "heads", "bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Real ref + two Finder-style duplicates + an unrelated file that
	// happens to have a space (no trailing digit — should be ignored).
	mustWrite(t, filepath.Join(dir, "session-1"), "abc\n")
	mustWrite(t, filepath.Join(dir, "session-1 2"), "abc\n")
	mustWrite(t, filepath.Join(dir, "session-1 3"), "abc\n")
	mustWrite(t, filepath.Join(dir, "feature thing"), "abc\n")

	phantoms, err := findPhantomBranchRefs(repo, "bosun")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(phantoms) != 2 {
		t.Fatalf("want 2 phantoms, got %d: %v", len(phantoms), phantoms)
	}
}

func TestFindPhantomBranchRefs_MissingDirNotAnError(t *testing.T) {
	repo := t.TempDir()
	phantoms, err := findPhantomBranchRefs(repo, "bosun")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(phantoms) != 0 {
		t.Errorf("want 0 phantoms, got %d", len(phantoms))
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSameLabels(t *testing.T) {
	// sameLabels gates the --resume "args contradict state" warning. The
	// fix downgraded the contradiction from a fatal error to a warning, so
	// the comparator's correctness is what controls whether the operator
	// sees the warning at all.
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"identical numbered", []string{"session-1", "session-2"}, []string{"session-1", "session-2"}, true},
		{"identical named", []string{"auth", "http"}, []string{"auth", "http"}, true},
		{"differ in count", []string{"session-1", "session-2", "session-3"}, []string{"session-1", "session-2"}, false},
		{"differ in order", []string{"auth", "http"}, []string{"http", "auth"}, false},
		{"differ in content", []string{"auth"}, []string{"storage"}, false},
		{"empty vs non-empty", nil, []string{"session-1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameLabels(tc.a, tc.b); got != tc.want {
				t.Errorf("sameLabels(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
