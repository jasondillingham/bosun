package session

import (
	"strings"
	"testing"
)

// FuzzValidateLabel ensures the label charset gate never panics and
// preserves the documented invariant: accepted labels match the
// publicly-stated charset (lowercase letter-prefixed, hyphenated). A
// regression that loosens the regex (e.g. someone "helpfully" allows
// underscores) would surface here.
//
// Run with:  go test -fuzz=FuzzValidateLabel -fuzztime=60s ./internal/session/
func FuzzValidateLabel(f *testing.F) {
	f.Add("session-1")
	f.Add("auth")
	f.Add("auth-flow")
	f.Add("a")
	f.Add("Auth")            // wrong case
	f.Add("1session")        // digit prefix
	f.Add("session-")        // trailing dash
	f.Add("session--double") // double dash
	f.Add("")
	f.Add("..")
	f.Add("path/traversal")
	f.Add("session-\x00")
	f.Add("αβγ")

	// v0.9 added dot as a segment separator for sub-session labels
	// (`session-1.auth`). The charset is now a-z, 0-9, hyphen, dot —
	// with structural constraints (no leading/trailing dot, no `..`,
	// each dot-separated segment also follows the segment rules).
	f.Add("session-1.auth")
	f.Add(".leading-dot")
	f.Add("trailing-dot.")
	f.Add("double..dot")

	f.Fuzz(func(t *testing.T, label string) {
		err := ValidateLabel(label)
		if err != nil {
			return
		}
		// Accepted labels: lowercase ASCII letter, ASCII digit, hyphen,
		// or dot (v0.9+). Anything else slipping past means the regex
		// weakened.
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r == '-':
			case r == '.':
			default:
				t.Errorf("ValidateLabel accepted %q (rune %U at %d)", label, r, i)
			}
		}
		// First rune must be a letter.
		if len(label) > 0 {
			first := rune(label[0])
			if first < 'a' || first > 'z' {
				t.Errorf("ValidateLabel accepted %q but it doesn't start with [a-z]", label)
			}
		}
		// Trailing dash/dot, doubled dash/dot — none allowed.
		if strings.HasSuffix(label, "-") {
			t.Errorf("ValidateLabel accepted trailing-dash label %q", label)
		}
		if strings.HasSuffix(label, ".") {
			t.Errorf("ValidateLabel accepted trailing-dot label %q", label)
		}
		if strings.Contains(label, "--") {
			t.Errorf("ValidateLabel accepted double-dash label %q", label)
		}
		if strings.Contains(label, "..") {
			t.Errorf("ValidateLabel accepted double-dot label %q", label)
		}
		// Each dot-separated segment must independently satisfy the
		// segment rules — no segment may start with a digit/dash.
		for _, seg := range strings.Split(label, ".") {
			if seg == "" {
				t.Errorf("ValidateLabel accepted empty segment in %q", label)
				continue
			}
			first := rune(seg[0])
			if first < 'a' || first > 'z' {
				t.Errorf("ValidateLabel accepted segment %q (in %q) not starting with [a-z]", seg, label)
			}
		}
	})
}

// FuzzParseLabel covers the numeric short-form (`3` → `session-3`) plus
// the named-label pass-through. The function must never panic and must
// return either an error OR a string in the validated charset.
func FuzzParseLabel(f *testing.F) {
	f.Add("session-1")
	f.Add("3")
	f.Add("0")
	f.Add("-1")
	f.Add("999999999999999999999999") // overflow
	f.Add("auth")
	f.Add("")

	f.Fuzz(func(t *testing.T, in string) {
		out, err := ParseLabel(in)
		if err != nil {
			return
		}
		// Output must itself pass ValidateLabel.
		if verr := ValidateLabel(out); verr != nil {
			t.Errorf("ParseLabel(%q) → %q, but that doesn't pass ValidateLabel: %v", in, out, verr)
		}
	})
}
