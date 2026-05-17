package predict

import (
	"testing"
)

// FuzzExtractPaths exercises the predictor's brief-body parser against
// arbitrary input. The regex engine is the highest-risk surface here:
// catastrophic backtracking on pathological inputs would manifest as a
// fuzz timeout, and a heading variant that confuses extractListSection
// could surface as a returned slice with bogus paths.
//
// The function must not panic, must always return three slices (possibly
// nil), and must keep the avoid set disjoint from the predicted set.
//
// Run with:  go test -fuzz=FuzzExtractPaths -fuzztime=60s ./internal/predict/
func FuzzExtractPaths(f *testing.F) {
	// Seeds covering the parsing surfaces: owned list, code fence, inline
	// mention, avoid list, and the cross-interactions.
	f.Add("")
	f.Add("Files (own):\n- cmd/foo.go\n")
	f.Add("Files (avoid):\n- cmd/foo.go\n")
	f.Add("Files (own):\n- a.go\n\nFiles (avoid):\n- a.go\n") // overlap
	f.Add("```\ninternal/bar.go\n```\n")
	f.Add("plain prose mentioning `cmd/foo.go`")
	f.Add("Files (own):\n- ../etc/passwd\n") // path traversal seed
	f.Add("Files (own):\n- " + "long" + ".go\n")

	f.Fuzz(func(t *testing.T, body string) {
		predicted, sources, avoid := extractPaths(body)
		if len(predicted) != len(sources) {
			t.Errorf("predicted/sources length mismatch: %d vs %d", len(predicted), len(sources))
		}
		// Avoid set must never overlap predicted set — that's the v0.7
		// invariant we just landed. A regression here is exactly the
		// kind of subtle drift fuzzing should catch.
		avoidSet := map[string]struct{}{}
		for _, a := range avoid {
			avoidSet[a] = struct{}{}
		}
		for _, p := range predicted {
			if _, dup := avoidSet[p]; dup {
				t.Errorf("predicted path %q also in avoid set; body=%q", p, body)
			}
		}
	})
}
