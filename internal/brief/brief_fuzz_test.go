package brief

import (
	"testing"
)

// FuzzParseContent runs the brief parser against arbitrary input to surface
// crashes, infinite regex loops, or panics from malformed markdown. The
// parser must always return a slice (possibly empty) without panicking,
// regardless of what's fed in.
//
// Run with:  go test -fuzz=FuzzParseContent -fuzztime=60s ./internal/brief/
func FuzzParseContent(f *testing.F) {
	// Seeds picked to exercise the heading regex and the dependency clause:
	// a degenerate empty input, valid one-session, valid with dependency,
	// the unicode-in-headings case (currently rejected by the [a-z]
	// charset but the parser shouldn't crash on it).
	f.Add("")
	f.Add("## session-1\n\nbody\n")
	f.Add("## session-1\n## session-2 (depends: session-1)\n")
	f.Add("## αβγ\n\nbody\n")
	f.Add("## session-1 (depends: session-1)\n") // self-dep
	f.Add("## session-1\n## session-1\n")        // duplicate
	f.Add("##\n##\n")                            // empty headings
	// CRLF and mixed line endings.
	f.Add("## session-1\r\nbody\r\n")

	f.Fuzz(func(t *testing.T, s string) {
		// parseContent should never panic; ValidateBriefs may reject but
		// also must not panic. Both are exercised here because plan-level
		// errors (duplicate headings, self-dep) live in ValidateBriefs.
		briefs := parseContent(s)
		_ = ValidateBriefs(briefs)

		// Invariant: every returned brief has a non-empty Label.
		for i, b := range briefs {
			if b.Label == "" {
				t.Errorf("brief[%d] has empty Label; input=%q", i, s)
			}
		}
	})
}
