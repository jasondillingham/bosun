// Package preflight collects cheap, fast checks bosun runs before
// long-blocking git operations (init, merge). Each check should print
// an advisory rather than fail closed — operators can override with
// --no-load-check or similar flags when they know better than the
// heuristic.
package preflight

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Default knobs for the 1-minute load average advisory. var-scoped (not
// const) so tests can shorten the pause without sleeping for real seconds
// and an operator-overridable threshold could be wired in later without
// reshaping callers.
var (
	DefaultLoadWarnThreshold       = 5.0
	DefaultLoadAveragePauseDuration = 2 * time.Second
)

// LoadAverageOverride is the indirection point tests use to inject a
// synthetic 1-minute load average. When nil, LoadAverage reads the
// host's real load. Tests that want to exercise the warning path
// (without setting BOSUN_TEST_LOAD_AVERAGE) swap this in and restore
// in t.Cleanup.
var LoadAverageOverride func() (float64, error)

// LoadAverage returns the system's 1-minute load average. Honors
// LoadAverageOverride (test seam) and BOSUN_TEST_LOAD_AVERAGE (subprocess
// seam used by scenario tests) before falling back to the platform
// implementation.
func LoadAverage() (float64, error) {
	if LoadAverageOverride != nil {
		return LoadAverageOverride()
	}
	if v := os.Getenv("BOSUN_TEST_LOAD_AVERAGE"); v != "" {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("BOSUN_TEST_LOAD_AVERAGE=%q: %w", v, err)
		}
		return f, nil
	}
	switch runtime.GOOS {
	case "linux":
		return readLoadFromProcLoadavg()
	case "darwin":
		return readLoadFromUptime()
	default:
		// Windows / other: no reliable cross-vendor 1-min load average.
		return 0, nil
	}
}

// CheckLoad prints a warning and pauses if the 1-minute load average
// exceeds threshold. Returns true if the warning fired. A failure to
// read the load is downgraded to a stderr warning — the check is
// advisory, never a hard gate. The op label appears in the warning
// (init / merge / …) so operators can correlate the pause with the
// command that triggered it.
func CheckLoad(out io.Writer, op string, threshold float64, pause time.Duration) bool {
	load, err := LoadAverage()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: load-average check failed: %v\n", err)
		return false
	}
	if load <= threshold {
		return false
	}
	fmt.Fprintf(out, "system load is %.2f; %s may be slow (--no-load-check to skip)\n", load, op)
	if pause > 0 {
		time.Sleep(pause)
	}
	return true
}

func readLoadFromProcLoadavg() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected /proc/loadavg format: %q", data)
	}
	return strconv.ParseFloat(fields[0], 64)
}

func readLoadFromUptime() (float64, error) {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		return 0, err
	}
	return parseUptimeLoad(string(out))
}

// parseUptimeLoad pulls the 1-minute load average out of `uptime` output.
// Example input: "14:32  up 1:23, 2 users, load averages: 1.23 1.45 1.67".
// macOS uses "load averages:" (plural) and Linux uses "load average:";
// match either by anchoring on "load average".
func parseUptimeLoad(text string) (float64, error) {
	idx := strings.Index(text, "load average")
	if idx < 0 {
		return 0, fmt.Errorf("unexpected uptime output: %q", text)
	}
	colon := strings.Index(text[idx:], ":")
	if colon < 0 {
		return 0, fmt.Errorf("unexpected uptime output: %q", text)
	}
	rest := text[idx+colon+1:]
	// Linux uptime separates with commas; macOS with spaces. Normalize.
	rest = strings.ReplaceAll(rest, ",", " ")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected uptime output: %q", text)
	}
	return strconv.ParseFloat(fields[0], 64)
}
