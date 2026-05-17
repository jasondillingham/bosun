package phantom

import (
	"strings"
	"testing"
)

// FuzzIsLikelyPhantom asserts the matching surface stays anchored on the
// documented shape: a literal space + digit-run (or paren-wrapped digits)
// before the extension. Operator-named files that don't contain that
// shape must never match. A regression that broadens the pattern (e.g.
// accidentally matches every file with a digit anywhere) would surface
// as a fuzz failure naming the unexpectedly-matched input.
//
// Run with:  go test -fuzz=FuzzIsLikelyPhantom -fuzztime=60s ./internal/phantom/
func FuzzIsLikelyPhantom(f *testing.F) {
	f.Add("session-1.json", "json")
	f.Add("session-1 2.json", "json")
	f.Add("a (1).done", "done")
	f.Add("normal_file.go", "go")
	f.Add(".lock", "json")
	f.Add("", "json")
	f.Add("name with spaces.json", "json")

	f.Fuzz(func(t *testing.T, name, ext string) {
		matched := IsLikelyPhantom(name, ext)
		if !matched {
			return
		}
		// A match means the name must contain either ` <digits>.` or
		// ` (<digits>).` immediately followed by an allowed extension.
		// Both phantomSpotlightPattern and phantomICloudPattern anchor
		// on a space; a file without a space in it should never match.
		if !strings.Contains(name, " ") {
			t.Errorf("IsLikelyPhantom(%q, %q) matched but name contains no space", name, ext)
		}
		// The matched extension (last dot-separated piece) must equal
		// the requested ext when ext is non-empty.
		if ext != "" {
			dot := strings.LastIndex(name, ".")
			if dot < 0 {
				t.Errorf("IsLikelyPhantom(%q, %q) matched but name has no extension", name, ext)
				return
			}
			if name[dot+1:] != ext {
				t.Errorf("IsLikelyPhantom(%q, %q) matched but extension is %q", name, ext, name[dot+1:])
			}
		}
	})
}
