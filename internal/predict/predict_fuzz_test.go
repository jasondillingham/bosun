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
// The function must not panic, must always return four slices (possibly
// nil), and must keep the predicted / avoid / warned buckets pairwise
// disjoint.
//
// Run with:  go test -fuzz=FuzzExtractPaths -fuzztime=60s ./internal/predict/
func FuzzExtractPaths(f *testing.F) {
	// Seeds covering the parsing surfaces: owned list, code fence,
	// backtick span, prose mention, avoid list, and cross-interactions.
	f.Add("")
	f.Add("Files (own):\n- cmd/foo.go\n")
	f.Add("Files (avoid):\n- cmd/foo.go\n")
	f.Add("Files (own):\n- a.go\n\nFiles (avoid):\n- a.go\n") // overlap
	f.Add("```\ninternal/bar.go\n```\n")
	f.Add("plain prose mentioning `cmd/foo.go`")
	f.Add("Do NOT modify internal/config/foo.go in this lane.") // prose-only
	f.Add("Constraint: leave `internal/config/foo.go` alone.")  // backticked
	f.Add("`unterminated backtick run /foo.go")                 // dangling tick
	f.Add("Files (own):\n- ../etc/passwd\n")                    // traversal
	f.Add("Files (own):\n- " + "long" + ".go\n")

	f.Fuzz(func(t *testing.T, body string) {
		predicted, sources, avoid, warned := extractPaths(body)
		if len(predicted) != len(sources) {
			t.Errorf("predicted/sources length mismatch: %d vs %d", len(predicted), len(sources))
		}
		// The three buckets are pairwise disjoint by contract: a path
		// is either claimed (predicted), forbidden (avoid), or merely
		// mentioned (warned) — never two of those at once. A regression
		// here is exactly the kind of subtle drift fuzzing should catch.
		predictedSet := map[string]struct{}{}
		for _, p := range predicted {
			predictedSet[p] = struct{}{}
		}
		avoidSet := map[string]struct{}{}
		for _, a := range avoid {
			avoidSet[a] = struct{}{}
		}
		for _, p := range predicted {
			if _, dup := avoidSet[p]; dup {
				t.Errorf("predicted path %q also in avoid set; body=%q", p, body)
			}
		}
		for _, w := range warned {
			if _, dup := predictedSet[w]; dup {
				t.Errorf("warned path %q also predicted; body=%q", w, body)
			}
			if _, dup := avoidSet[w]; dup {
				t.Errorf("warned path %q also in avoid set; body=%q", w, body)
			}
		}
	})
}
