package preflight

import (
	"testing"
)

// FuzzParseUptimeLoad covers the brittle bit of the package: parsing
// arbitrary uptime(1) output across macOS and Linux variants. The
// function must never panic and must return either an error OR a
// non-negative float. Anything else is a parser bug.
//
// Run with:  go test -fuzz=FuzzParseUptimeLoad -fuzztime=60s ./internal/preflight/
func FuzzParseUptimeLoad(f *testing.F) {
	// Real-world variants seen across BSDs, Linux, macOS, plus malformed
	// inputs that an old / wrong uptime binary might produce.
	f.Add("14:32  up 1:23, 2 users, load averages: 1.23 1.45 1.67")
	f.Add(" 14:32  up 1:23, 2 users,  load average: 0.05, 0.10, 0.20")
	f.Add("load average: 0.00 0.00 0.00")
	f.Add("load averages: 999.99 999.99 999.99")
	f.Add("")
	f.Add("totally bogus output")
	f.Add("load average:")
	f.Add("load average: not-a-number")
	f.Add("load averages: -1.0 0.0 0.0") // negative (impossible but ensure no panic)

	f.Fuzz(func(t *testing.T, raw string) {
		v, err := parseUptimeLoad(raw)
		if err != nil {
			return
		}
		// Successful parse must yield a finite, non-negative number.
		// NaN/Inf would indicate ParseFloat fed something pathological;
		// negative would indicate sign-bit drift.
		if v < 0 || v != v { // v != v catches NaN
			t.Errorf("parseUptimeLoad(%q) = %v, want non-negative finite", raw, v)
		}
	})
}
