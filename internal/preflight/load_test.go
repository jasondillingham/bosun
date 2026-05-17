package preflight

import (
	"bytes"
	"strings"
	"testing"
	"time"
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

func TestLoadAverage_RespectsEnvOverride(t *testing.T) {
	// BOSUN_TEST_LOAD_AVERAGE is the seam scenario tests use to inject a
	// synthetic load from outside the process. Cover it directly so the
	// contract doesn't silently rot.
	t.Setenv("BOSUN_TEST_LOAD_AVERAGE", "12.5")
	got, err := LoadAverage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 12.5 {
		t.Errorf("want 12.5, got %v", got)
	}
}

func TestLoadAverage_StubbableViaOverride(t *testing.T) {
	// LoadAverageOverride is the in-process indirection unit tests use to
	// inject a value without depending on the host's real load.
	t.Cleanup(func() { LoadAverageOverride = nil })
	LoadAverageOverride = func() (float64, error) { return 9.9, nil }
	got, err := LoadAverage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 9.9 {
		t.Errorf("want 9.9, got %v", got)
	}
}

func TestCheckLoad_BelowThresholdSilent(t *testing.T) {
	t.Cleanup(func() { LoadAverageOverride = nil })
	LoadAverageOverride = func() (float64, error) { return 1.0, nil }
	var buf bytes.Buffer
	if fired := CheckLoad(&buf, "init", 5.0, 0); fired {
		t.Errorf("CheckLoad should not have fired at load=1.0, threshold=5.0")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output below threshold, got: %q", buf.String())
	}
}

func TestCheckLoad_AboveThresholdWarns(t *testing.T) {
	t.Cleanup(func() { LoadAverageOverride = nil })
	LoadAverageOverride = func() (float64, error) { return 8.0, nil }
	var buf bytes.Buffer
	start := time.Now()
	fired := CheckLoad(&buf, "merge", 5.0, 0)
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("CheckLoad slept despite pause=0 (elapsed %s)", elapsed)
	}
	if !fired {
		t.Errorf("CheckLoad should have fired at load=8.0, threshold=5.0")
	}
	out := buf.String()
	if !strings.Contains(out, "system load is 8.00") {
		t.Errorf("expected load value in output, got: %q", out)
	}
	if !strings.Contains(out, "merge may be slow") {
		t.Errorf("expected op label %q in output, got: %q", "merge", out)
	}
	if !strings.Contains(out, "--no-load-check") {
		t.Errorf("expected --no-load-check hint, got: %q", out)
	}
}

func TestCheckLoad_HonorsPause(t *testing.T) {
	// Smoke-test the pause path: with a non-zero pause we should sleep at
	// least the configured duration when the warning fires.
	t.Cleanup(func() { LoadAverageOverride = nil })
	LoadAverageOverride = func() (float64, error) { return 9.0, nil }
	var buf bytes.Buffer
	pause := 50 * time.Millisecond
	start := time.Now()
	CheckLoad(&buf, "init", 5.0, pause)
	if elapsed := time.Since(start); elapsed < pause {
		t.Errorf("CheckLoad did not pause: elapsed %s < expected %s", elapsed, pause)
	}
}
