package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/predict"
)

// stubPredictor returns canned predictions and overlaps regardless of
// input, plus records the briefs the runner handed it. Used everywhere
// the test needs deterministic output instead of session-2's heuristic.
type stubPredictor struct {
	predictions []predict.Prediction
	overlaps    []predict.Overlap

	calls       int
	lastBriefs  []brief.Brief
	lastLabels  []string
}

func (s *stubPredictor) Predict(briefs []brief.Brief) ([]predict.Prediction, []predict.Overlap, error) {
	s.calls++
	s.lastBriefs = briefs
	s.lastLabels = nil
	for _, b := range briefs {
		s.lastLabels = append(s.lastLabels, b.Label)
	}
	return s.predictions, s.overlaps, nil
}

// writePlan writes content into a tempdir/plan.md and returns the path.
func writePlan(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

// twoLanePlan is the minimal plan used across the runPredict tests.
const twoLanePlan = `# Plan

## session-1
Refactor auth handlers under internal/auth.

## session-2
Migrate storage layer.
`

func TestRunPredict_NoOverlaps_ExitsZeroAndPrintsReport(t *testing.T) {
	planPath := writePlan(t, twoLanePlan)

	stub := &stubPredictor{
		predictions: []predict.Prediction{
			{
				Session: "session-1",
				Scope:   "Refactor auth handlers under internal/auth.",
				Paths: []predict.PredictedPath{
					{Path: "internal/auth/handlers.go", Reason: "mentioned in brief"},
				},
			},
			{
				Session: "session-2",
				Scope:   "Migrate storage layer.",
				Paths: []predict.PredictedPath{
					{Path: "internal/storage/", Reason: "package-pattern"},
				},
			},
		},
	}

	var out bytes.Buffer
	err := runPredict(&out, planPath, false, predictDeps{predictor: stub})
	if err != nil {
		t.Fatalf("runPredict: %v", err)
	}

	if stub.calls != 1 {
		t.Errorf("predictor called %d times, want 1", stub.calls)
	}
	if got := strings.Join(stub.lastLabels, ","); got != "session-1,session-2" {
		t.Errorf("briefs forwarded in wrong order: %q", got)
	}

	got := out.String()
	for _, want := range []string{
		"session-1", "session-2",
		"internal/auth/handlers.go", "mentioned in brief",
		"internal/storage/", "package-pattern",
		"Overlaps: none predicted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\nfull report:\n%s", want, got)
		}
	}
}

func TestRunPredict_OverlapExitsNonZero_AndReportNamesLanes(t *testing.T) {
	planPath := writePlan(t, twoLanePlan)

	stub := &stubPredictor{
		predictions: []predict.Prediction{
			{Session: "session-1", Paths: []predict.PredictedPath{{Path: "internal/auth/x.go"}}},
			{Session: "session-2", Paths: []predict.PredictedPath{{Path: "internal/auth/x.go"}}},
		},
		overlaps: []predict.Overlap{
			{
				Path:       "internal/auth/x.go",
				Sessions:   []string{"session-1", "session-2"},
				Severity:   "high",
				Mitigation: "narrow session-2 to avoid the internal/auth glob",
			},
		},
	}

	var out bytes.Buffer
	err := runPredict(&out, planPath, false, predictDeps{predictor: stub})
	if err == nil {
		t.Fatal("expected error when overlaps are predicted")
	}
	// The error must map to exitUserErr (kindUser) — that's the contract
	// per the brief: exit 1 when any overlaps exist regardless of severity.
	if code := exitCodeFor(err); code != exitUserErr {
		t.Errorf("exit code = %d, want %d (user error)", code, exitUserErr)
	}

	got := out.String()
	for _, want := range []string{
		"Overlaps: 1",
		"[high]",
		"internal/auth/x.go",
		"session-1, session-2",
		"narrow session-2 to avoid the internal/auth glob",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\nfull report:\n%s", want, got)
		}
	}
}

func TestRunPredict_JSONFlag_EmitsStructuredReport(t *testing.T) {
	planPath := writePlan(t, twoLanePlan)

	stub := &stubPredictor{
		predictions: []predict.Prediction{
			{
				Session: "session-1",
				Scope:   "refactor",
				Paths:   []predict.PredictedPath{{Path: "a.go", Reason: "mentioned"}},
			},
		},
		overlaps: []predict.Overlap{
			{Path: "a.go", Sessions: []string{"session-1", "session-2"}, Severity: "low"},
		},
	}

	var out bytes.Buffer
	err := runPredict(&out, planPath, true, predictDeps{predictor: stub})
	if err == nil {
		t.Fatal("expected non-zero exit when overlaps reported, got nil")
	}

	var report predictReport
	if jsonErr := json.Unmarshal(out.Bytes(), &report); jsonErr != nil {
		t.Fatalf("output is not JSON: %v\n%s", jsonErr, out.String())
	}
	if len(report.Predictions) != 1 || report.Predictions[0].Session != "session-1" {
		t.Errorf("predictions wrong: %+v", report.Predictions)
	}
	if len(report.Overlaps) != 1 || report.Overlaps[0].Severity != "low" {
		t.Errorf("overlaps wrong: %+v", report.Overlaps)
	}
}

func TestRunPredict_JSON_EmptySlicesAreNotNull(t *testing.T) {
	planPath := writePlan(t, twoLanePlan)

	// Predictor returns nil for both slices.
	stub := &stubPredictor{}

	var out bytes.Buffer
	if err := runPredict(&out, planPath, true, predictDeps{predictor: stub}); err != nil {
		t.Fatalf("runPredict: %v", err)
	}

	// The JSON wire shape must read [] not null so consumers can iterate
	// without a nil check. Spot-check the literal text.
	got := out.String()
	if strings.Contains(got, "null") {
		t.Errorf("JSON contains null where [] expected:\n%s", got)
	}
	if !strings.Contains(got, "\"predictions\": []") {
		t.Errorf("predictions should serialise as []:\n%s", got)
	}
	if !strings.Contains(got, "\"overlaps\": []") {
		t.Errorf("overlaps should serialise as []:\n%s", got)
	}
}

func TestRunPredict_EmptyPlan_FailsCleanly(t *testing.T) {
	// A plan with no `## session-N` headings must be flagged as user
	// error, not an internal/heuristic-time crash.
	planPath := writePlan(t, "# Plan with no sessions\n\nJust narrative text.\n")

	var out bytes.Buffer
	err := runPredict(&out, planPath, false, predictDeps{predictor: &stubPredictor{}})
	if err == nil {
		t.Fatal("expected error for plan with no session headings")
	}
	if code := exitCodeFor(err); code != exitUserErr {
		t.Errorf("exit code = %d, want %d", code, exitUserErr)
	}
}

func TestRunPredict_MissingFile_FailsCleanly(t *testing.T) {
	var out bytes.Buffer
	err := runPredict(&out, "/nonexistent/does-not-exist.md", false,
		predictDeps{predictor: &stubPredictor{}})
	if err == nil {
		t.Fatal("expected error for missing plan file")
	}
	if code := exitCodeFor(err); code != exitUserErr {
		t.Errorf("exit code = %d, want %d", code, exitUserErr)
	}
}

func TestRunPredict_NilPredictor_IsInternalError(t *testing.T) {
	planPath := writePlan(t, twoLanePlan)
	var out bytes.Buffer
	err := runPredict(&out, planPath, false, predictDeps{predictor: nil})
	if err == nil {
		t.Fatal("expected error for nil predictor")
	}
	if code := exitCodeFor(err); code != exitInternal {
		t.Errorf("exit code = %d, want %d (internal)", code, exitInternal)
	}
}
