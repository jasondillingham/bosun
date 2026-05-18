package predict

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/brief"
)

func TestPredict_OwnedAvoidLists(t *testing.T) {
	briefs := []brief.Brief{{
		Label: "session-2",
		Body: strings.Join([]string{
			"**Predictive conflict — heuristic engine.**",
			"",
			"Build a new package `internal/predict/` that takes briefs.",
			"",
			"Files (own):",
			"- `internal/predict/types.go`",
			"- `internal/predict/predict.go`",
			"- `internal/predict/predict_test.go`",
			"",
			"Files (avoid):",
			"- `cmd/bosun/`",
			"- `internal/mcp/`",
		}, "\n"),
	}}

	preds, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(overlaps) != 0 {
		t.Errorf("single brief should produce no overlaps, got %d: %+v", len(overlaps), overlaps)
	}
	if len(preds) != 1 {
		t.Fatalf("expected 1 prediction, got %d", len(preds))
	}
	got := preds[0]
	if got.Session != "session-2" {
		t.Errorf("Session = %q, want %q", got.Session, "session-2")
	}
	if len(got.Predicted) != len(got.Source) {
		t.Fatalf("Predicted and Source must be 1:1: %d vs %d",
			len(got.Predicted), len(got.Source))
	}
	for _, want := range []string{
		"internal/predict/types.go",
		"internal/predict/predict.go",
		"internal/predict/predict_test.go",
	} {
		if !containsPath(got.Predicted, want) {
			t.Errorf("missing predicted path %q in %v", want, got.Predicted)
		}
	}
	for _, avoided := range []string{"cmd/bosun/", "internal/mcp/"} {
		if containsPath(got.Predicted, avoided) {
			t.Errorf("avoid-list path %q must NOT appear in Predicted: %v", avoided, got.Predicted)
		}
		if !containsPath(got.Avoid, avoided) {
			t.Errorf("expected %q in Avoid, got %v", avoided, got.Avoid)
		}
	}
	idxOwned := indexOf(got.Predicted, "internal/predict/types.go")
	if idxOwned < 0 || got.Source[idxOwned] != "owned list" {
		t.Errorf("expected `owned list` source for types.go, got %q", got.Source[idxOwned])
	}
}

// TestPredict_AvoidListExcludedFromOverlaps is the regression test for
// the v0.6-round1 false-positive blowup: when two briefs both put a
// shared lane like `internal/auth/` on their "Files (avoid)" list, the
// predictor must not flag that as an overlap — neither session is
// going to touch it.
func TestPredict_AvoidListExcludedFromOverlaps(t *testing.T) {
	briefs := []brief.Brief{
		{
			Label: "session-1",
			Body: strings.Join([]string{
				"Files (own):",
				"- `internal/foo/foo.go`",
				"",
				"Files (avoid):",
				"- `internal/shared/`",
				"- `cmd/bosun/cmd_merge.go`",
			}, "\n"),
		},
		{
			Label: "session-2",
			Body: strings.Join([]string{
				"Files (own):",
				"- `internal/bar/bar.go`",
				"",
				"Files (avoid):",
				"- `internal/shared/`",
				"- `cmd/bosun/cmd_merge.go`",
			}, "\n"),
		},
	}
	preds, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(overlaps) != 0 {
		t.Errorf("two sessions sharing only an avoid list must not overlap, got %d: %+v", len(overlaps), overlaps)
	}
	for _, p := range preds {
		for _, avoided := range []string{"internal/shared/", "cmd/bosun/cmd_merge.go"} {
			if containsPath(p.Predicted, avoided) {
				t.Errorf("%s: avoid-list path %q leaked into Predicted: %v", p.Session, avoided, p.Predicted)
			}
		}
	}
}

// TestPredict_AvoidListSuppressesInlineMention covers a subtler case:
// even if a path appears in prose elsewhere in the brief, listing it
// under "Files (avoid)" should keep it out of the predicted set —
// otherwise the inline-mention scan would re-introduce the false
// positive the explicit list was meant to suppress.
func TestPredict_AvoidListSuppressesInlineMention(t *testing.T) {
	briefs := []brief.Brief{{
		Label: "session-1",
		Body: strings.Join([]string{
			"Edit internal/foo/foo.go and leave cmd/bosun/cmd_merge.go alone.",
			"",
			"Files (own):",
			"- `internal/foo/foo.go`",
			"",
			"Files (avoid):",
			"- `cmd/bosun/cmd_merge.go`",
		}, "\n"),
	}}
	preds, _, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if containsPath(preds[0].Predicted, "cmd/bosun/cmd_merge.go") {
		t.Errorf("avoid-listed path picked up by inline mention: %v", preds[0].Predicted)
	}
	if !containsPath(preds[0].Avoid, "cmd/bosun/cmd_merge.go") {
		t.Errorf("expected cmd/bosun/cmd_merge.go in Avoid, got %v", preds[0].Avoid)
	}
}

func TestPredict_CodeBlockFiles(t *testing.T) {
	body := "" +
		"Shared types in this file:\n\n" +
		"```go\n" +
		"// internal/predict/types.go\n" +
		"package predict\n" +
		"```\n"
	briefs := []brief.Brief{{Label: "session-x", Body: body}}
	preds, _, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if !containsPath(preds[0].Predicted, "internal/predict/types.go") {
		t.Errorf("expected types.go from fenced block, got %v", preds[0].Predicted)
	}
	idx := indexOf(preds[0].Predicted, "internal/predict/types.go")
	if preds[0].Source[idx] != "code block" {
		t.Errorf("source = %q, want code block", preds[0].Source[idx])
	}
}

func TestPredict_TestCoLocation(t *testing.T) {
	briefs := []brief.Brief{{
		Label: "session-1",
		Body: "Files (own):\n" +
			"- `internal/auth/handler.go`\n",
	}}
	preds, _, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if !containsPath(preds[0].Predicted, "internal/auth/handler.go") {
		t.Errorf("missing owned file")
	}
	if !containsPath(preds[0].Predicted, "internal/auth/handler_test.go") {
		t.Errorf("expected handler_test.go via test co-location, got %v", preds[0].Predicted)
	}
	idx := indexOf(preds[0].Predicted, "internal/auth/handler_test.go")
	if !strings.HasPrefix(preds[0].Source[idx], "test co-location") {
		t.Errorf("source = %q, want test co-location", preds[0].Source[idx])
	}
}

func TestPredict_DirectoryPrefix(t *testing.T) {
	briefs := []brief.Brief{{
		Label: "auth",
		Body:  "All work happens under `internal/auth/`. Touch nothing else.",
	}}
	preds, _, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if !containsPath(preds[0].Predicted, "internal/auth/") {
		t.Errorf("expected dir prefix `internal/auth/`, got %v", preds[0].Predicted)
	}
}

func TestPredict_VaguebriefProducesFew(t *testing.T) {
	briefs := []brief.Brief{{
		Label: "session-3",
		Body:  "Refactor the auth module. Make it cleaner and add tests.",
	}}
	preds, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(preds[0].Predicted) != 0 {
		t.Errorf("vague brief should produce 0 predictions, got %v", preds[0].Predicted)
	}
	if len(overlaps) != 0 {
		t.Errorf("vague brief should produce no overlaps")
	}
}

func TestPredict_SortedBySession(t *testing.T) {
	briefs := []brief.Brief{
		{Label: "zeta", Body: "Files (own):\n- `internal/zeta/x.go`\n"},
		{Label: "alpha", Body: "Files (own):\n- `internal/alpha/x.go`\n"},
		{Label: "mid", Body: "Files (own):\n- `internal/mid/x.go`\n"},
	}
	preds, _, _ := New().Predict(briefs)
	got := []string{preds[0].Session, preds[1].Session, preds[2].Session}
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("session order = %v, want %v", got, want)
	}
}

func TestPredict_FiltersImportPaths(t *testing.T) {
	briefs := []brief.Brief{{
		Label: "session-x",
		// Backticks make the operator's intent explicit: `path/filepath`
		// is a deliberate reference, `internal/foo/bar.go` is the file
		// to touch. The extension gate still filters the import path.
		Body: "Use `path/filepath`, not string concat. Touch `internal/foo/bar.go`.",
	}}
	preds, _, _ := New().Predict(briefs)
	if containsPath(preds[0].Predicted, "path/filepath") {
		t.Errorf("import path `path/filepath` should not be predicted (no extension), got %v", preds[0].Predicted)
	}
	if !containsPath(preds[0].Predicted, "internal/foo/bar.go") {
		t.Errorf("expected concrete file `internal/foo/bar.go`, got %v", preds[0].Predicted)
	}
}

// TestPredict_ContextSensitive is the table-driven coverage for the
// claim-vs-prose split (closes #17). Code fences and backtick spans are
// claims; unquoted prose is informational only and lands in Warned.
func TestPredict_ContextSensitive(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantClaim   []string // must appear in Predicted
		wantNoClaim []string // must NOT appear in Predicted
		wantWarned  []string // must appear in Warned
	}{
		{
			name: "fenced code block: claim",
			body: "```go\n" +
				"// internal/predict/types.go\n" +
				"package predict\n" +
				"```\n",
			wantClaim: []string{"internal/predict/types.go"},
		},
		{
			name:      "single-backtick span: claim",
			body:      "Edit `internal/foo/bar.go` to fix the bug.",
			wantClaim: []string{"internal/foo/bar.go"},
		},
		{
			name:        "plain prose: not a claim, lands in warned",
			body:        "Touch internal/foo/bar.go in your work.",
			wantNoClaim: []string{"internal/foo/bar.go"},
			wantWarned:  []string{"internal/foo/bar.go"},
		},
		{
			name: "backticked constraint clause: still a claim",
			// Operator's intent: they chose to wrap the path. Even though
			// the surrounding clause is "Do NOT modify", the backticks
			// signal a deliberate, callable-out reference.
			body:      "**Constraints:** Do NOT modify `internal/config/foo.go`.",
			wantClaim: []string{"internal/config/foo.go"},
		},
		{
			name: "mixed: fence + backtick + prose in one brief",
			body: "```\n" +
				"internal/a.go\n" +
				"```\n" +
				"See also `internal/b.go`.\n" +
				"Avoid touching internal/c.go.\n",
			wantClaim:   []string{"internal/a.go", "internal/b.go"},
			wantNoClaim: []string{"internal/c.go"},
			wantWarned:  []string{"internal/c.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preds, _, err := New().Predict([]brief.Brief{{Label: "s", Body: tt.body}})
			if err != nil {
				t.Fatalf("Predict: %v", err)
			}
			got := preds[0]
			for _, want := range tt.wantClaim {
				if !containsPath(got.Predicted, want) {
					t.Errorf("expected claim %q in Predicted=%v", want, got.Predicted)
				}
			}
			for _, no := range tt.wantNoClaim {
				if containsPath(got.Predicted, no) {
					t.Errorf("path %q must NOT be a claim, but Predicted=%v", no, got.Predicted)
				}
			}
			for _, want := range tt.wantWarned {
				if !containsPath(got.Warned, want) {
					t.Errorf("expected %q in Warned=%v", want, got.Warned)
				}
			}
		})
	}
}

// TestPredict_ArchitectMCPRegression reproduces the false-positive
// blowup the architect-mcp dogfood hit before this lane landed. Two
// briefs share a long, prose-only constraint paragraph naming a dozen
// paths each session must avoid. Under the old prose-as-claim parser
// every constraint path counted as a predicted touch in both briefs,
// producing well over a dozen high-severity overlaps on a plan whose
// real overlap surface was zero. The new context-sensitive parser
// drops prose mentions from the overlap calc, so this case must stay
// at ≤3 overlaps (and ideally 0).
func TestPredict_ArchitectMCPRegression(t *testing.T) {
	constraintProse := strings.Join([]string{
		"Constraints: do NOT modify internal/config/loader.go,",
		"internal/config/schema.go, internal/mcp/server.go,",
		"internal/mcp/handlers.go, internal/git/clone.go,",
		"internal/git/worktree.go, internal/status/render.go,",
		"internal/status/table.go, cmd/bosun/cmd_init.go,",
		"cmd/bosun/cmd_done.go, cmd/bosun/cmd_status.go, or",
		"internal/brief/parse.go. The lane should also avoid",
		"touching internal/session/manager.go.",
	}, "\n")
	briefs := []brief.Brief{
		{
			Label: "session-1",
			Body: "**Lane: refactor predict.**\n\n" +
				"Files (own):\n- `internal/predict/predict.go`\n\n" +
				constraintProse,
		},
		{
			Label: "session-2",
			Body: "**Lane: refactor predict tests.**\n\n" +
				"Files (own):\n- `internal/predict/predict_test.go`\n\n" +
				constraintProse,
		},
	}
	_, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(overlaps) > 3 {
		t.Errorf("architect-mcp regression: got %d overlaps, want ≤3: %+v",
			len(overlaps), overlaps)
	}
}

func TestPredict_OverlapHighConcrete(t *testing.T) {
	briefs := []brief.Brief{
		{Label: "session-1", Body: "Files (own):\n- `cmd/bosun/cmd_merge.go`\n"},
		{Label: "session-2", Body: "Files (own):\n- `cmd/bosun/cmd_merge.go`\n"},
	}
	_, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(overlaps) == 0 {
		t.Fatalf("expected at least one overlap")
	}
	got := overlaps[0]
	if got.Severity != "high" {
		t.Errorf("severity = %q, want high", got.Severity)
	}
	if got.Path != "cmd/bosun/cmd_merge.go" {
		t.Errorf("path = %q, want cmd/bosun/cmd_merge.go", got.Path)
	}
	if !reflect.DeepEqual(got.Sessions, []string{"session-1", "session-2"}) {
		t.Errorf("sessions = %v", got.Sessions)
	}
}

func TestPredict_OverlapMediumDirGlob(t *testing.T) {
	briefs := []brief.Brief{
		{Label: "session-1", Body: "Files (own):\n- `internal/auth/`\n"},
		{Label: "session-2", Body: "Files (own):\n- `internal/auth/handler.go`\n"},
	}
	_, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(overlaps) == 0 {
		t.Fatalf("expected at least one overlap")
	}
	found := false
	for _, ov := range overlaps {
		if ov.Severity == "medium" && ov.Path == "internal/auth/" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected medium overlap on internal/auth/, got %+v", overlaps)
	}
}

func TestPredict_OverlapMediumPattern(t *testing.T) {
	briefs := []brief.Brief{
		{Label: "session-1", Body: "Files (own):\n- `cmd/bosun/cmd_*.go`\n"},
		{Label: "session-2", Body: "Files (own):\n- `cmd/bosun/cmd_merge.go`\n"},
	}
	_, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	gotMed := false
	for _, ov := range overlaps {
		if ov.Severity == "medium" {
			gotMed = true
		}
	}
	if !gotMed {
		t.Errorf("expected medium overlap from glob+concrete, got %+v", overlaps)
	}
}

func TestPredict_OverlapLowSiblingFiles(t *testing.T) {
	briefs := []brief.Brief{
		{Label: "session-1", Body: "Files (own):\n- `internal/auth/login.go`\n"},
		{Label: "session-2", Body: "Files (own):\n- `internal/auth/logout.go`\n"},
	}
	_, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	gotLow := false
	for _, ov := range overlaps {
		if ov.Severity == "low" && ov.Path == "internal/auth/" {
			gotLow = true
		}
	}
	if !gotLow {
		t.Errorf("expected low overlap on shared dir, got %+v", overlaps)
	}
}

func TestPredict_NoOverlapDifferentDirs(t *testing.T) {
	briefs := []brief.Brief{
		{Label: "session-1", Body: "Files (own):\n- `internal/auth/login.go`\n"},
		{Label: "session-2", Body: "Files (own):\n- `internal/billing/charge.go`\n"},
	}
	_, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(overlaps) != 0 {
		t.Errorf("expected no overlaps for disjoint dirs, got %+v", overlaps)
	}
}

func TestPredict_OverlapSortedSeverityFirst(t *testing.T) {
	briefs := []brief.Brief{
		{Label: "a", Body: "Files (own):\n- `pkg/x.go`\n- `pkg/y/z.go`\n- `pkg/y/`\n"},
		{Label: "b", Body: "Files (own):\n- `pkg/x.go`\n- `pkg/y/z.go`\n- `pkg/q.go`\n"},
	}
	_, overlaps, err := New().Predict(briefs)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(overlaps) < 2 {
		t.Fatalf("expected multiple overlaps, got %d: %+v", len(overlaps), overlaps)
	}
	for i := 1; i < len(overlaps); i++ {
		if sevRank(overlaps[i-1].Severity) > sevRank(overlaps[i].Severity) {
			t.Errorf("overlaps not sorted by severity: %+v", overlaps)
		}
	}
}

func TestPredict_TableDriven(t *testing.T) {
	tests := []struct {
		name           string
		briefs         []brief.Brief
		wantPredicted  map[string][]string // session → must-contain paths
		wantSeverities []string            // severities expected, in order
	}{
		{
			name: "v0.4-plan-style: three lanes, no overlap",
			briefs: []brief.Brief{
				{Label: "session-1", Body: "Files (own):\n- `cmd/bosun/cmd_config.go`\n"},
				{Label: "session-2", Body: "Files (own):\n- `internal/predict/types.go`\n- `internal/predict/predict.go`\n"},
				{Label: "session-3", Body: "Files (own):\n- `internal/mcp/server.go`\n"},
			},
			wantPredicted: map[string][]string{
				"session-1": {"cmd/bosun/cmd_config.go"},
				"session-2": {"internal/predict/types.go", "internal/predict/predict.go"},
				"session-3": {"internal/mcp/server.go"},
			},
			wantSeverities: nil,
		},
		{
			name: "two lanes collide on shared file (also collide on co-located test)",
			briefs: []brief.Brief{
				{Label: "a", Body: "Files (own):\n- `cmd/bosun/cmd_done.go`\n"},
				{Label: "b", Body: "Files (own):\n- `cmd/bosun/cmd_done.go`\n"},
			},
			wantSeverities: []string{"high", "high"},
		},
		{
			name: "vague briefs produce no work",
			briefs: []brief.Brief{
				{Label: "session-1", Body: "Refactor auth."},
				{Label: "session-2", Body: "Clean up logging."},
			},
			wantPredicted: map[string][]string{
				"session-1": {},
				"session-2": {},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preds, overlaps, err := New().Predict(tt.briefs)
			if err != nil {
				t.Fatalf("Predict: %v", err)
			}
			for session, expected := range tt.wantPredicted {
				p := findPrediction(preds, session)
				if p == nil {
					t.Errorf("missing prediction for %s", session)
					continue
				}
				if len(expected) == 0 && len(p.Predicted) != 0 {
					t.Errorf("%s: expected empty predictions, got %v", session, p.Predicted)
				}
				for _, want := range expected {
					if !containsPath(p.Predicted, want) {
						t.Errorf("%s: missing %q in %v", session, want, p.Predicted)
					}
				}
			}
			if tt.wantSeverities != nil {
				if len(overlaps) != len(tt.wantSeverities) {
					t.Fatalf("overlap count = %d, want %d: %+v",
						len(overlaps), len(tt.wantSeverities), overlaps)
				}
				for i, sev := range tt.wantSeverities {
					if overlaps[i].Severity != sev {
						t.Errorf("overlap[%d].Severity = %q, want %q",
							i, overlaps[i].Severity, sev)
					}
				}
			}
		})
	}
}

func TestPredict_DedupePreservesFirstSource(t *testing.T) {
	briefs := []brief.Brief{{
		Label: "session-1",
		Body: "Files (own):\n- `internal/foo.go`\n\n" +
			"See also `internal/foo.go` for context.",
	}}
	preds, _, _ := New().Predict(briefs)
	idx := indexOf(preds[0].Predicted, "internal/foo.go")
	if idx < 0 {
		t.Fatalf("missing internal/foo.go")
	}
	if preds[0].Source[idx] != "owned list" {
		t.Errorf("source = %q, want owned list (first occurrence wins)", preds[0].Source[idx])
	}
}

func TestPredict_LabelFallback(t *testing.T) {
	briefs := []brief.Brief{
		{Session: 1, Body: "Files (own):\n- `a.go`\n"},
		{Session: 0, Body: "Files (own):\n- `b.go`\n"},
	}
	preds, _, _ := New().Predict(briefs)
	labels := []string{preds[0].Session, preds[1].Session}
	if !reflect.DeepEqual(labels, []string{"session-1", "session-?"}) {
		t.Errorf("labels = %v, want [session-1 session-?]", labels)
	}
}

// helpers

func containsPath(haystack []string, needle string) bool {
	for _, p := range haystack {
		if p == needle {
			return true
		}
	}
	return false
}

func indexOf(haystack []string, needle string) int {
	for i, p := range haystack {
		if p == needle {
			return i
		}
	}
	return -1
}

func findPrediction(preds []Prediction, session string) *Prediction {
	for i := range preds {
		if preds[i].Session == session {
			return &preds[i]
		}
	}
	return nil
}
