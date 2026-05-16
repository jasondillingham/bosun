package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/predict"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubPredictor returns canned slices and records the briefs the tool
// forwarded. Local copy of cmd/bosun's stub so the mcp package has no
// test-only dependency on cmd/bosun (which would import-cycle).
type stubPredictor struct {
	predictions []predict.Prediction
	overlaps    []predict.Overlap
	calls       int
	lastLabels  []string
}

func (s *stubPredictor) Predict(briefs []brief.Brief) ([]predict.Prediction, []predict.Overlap, error) {
	s.calls++
	s.lastLabels = nil
	for _, b := range briefs {
		s.lastLabels = append(s.lastLabels, b.Label)
	}
	return s.predictions, s.overlaps, nil
}

// withStubPredictor swaps the package-level predictor for the duration of
// a test. Restoration on cleanup guards against parallel-test contamination
// (the mcp tests aren't t.Parallel today but the cleanup is cheap and
// future-proofs).
func withStubPredictor(t *testing.T, stub predict.Predictor) {
	t.Helper()
	prev := predictPredictor
	predictPredictor = stub
	t.Cleanup(func() { predictPredictor = prev })
}

const planTwoLanes = `# Plan

## session-1
Refactor auth handlers.

## session-2
Migrate storage layer.
`

func TestPredict_RoundTripReturnsCannedResult(t *testing.T) {
	stub := &stubPredictor{
		predictions: []predict.Prediction{
			{
				Session: "session-1",
				Scope:   "Refactor auth handlers.",
				Paths:   []predict.PredictedPath{{Path: "internal/auth/x.go", Reason: "mentioned"}},
			},
			{
				Session: "session-2",
				Scope:   "Migrate storage layer.",
				Paths:   []predict.PredictedPath{{Path: "internal/storage/y.go"}},
			},
		},
		overlaps: []predict.Overlap{
			{Path: "shared.go", Sessions: []string{"session-1", "session-2"}, Severity: "high", Mitigation: "split shared.go"},
		},
	}
	withStubPredictor(t, stub)

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_predict",
		Arguments: map[string]any{"plan": planTwoLanes},
	})
	if err != nil {
		t.Fatalf("call bosun_predict: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_predict IsError: %+v", result)
	}
	if stub.calls != 1 {
		t.Errorf("stub called %d times, want 1", stub.calls)
	}
	if got := strings.Join(stub.lastLabels, ","); got != "session-1,session-2" {
		t.Errorf("briefs forwarded in wrong order: %q", got)
	}

	var out PredictResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("decode structured content: %v", err)
		}
	}
	if len(out.Predictions) != 2 {
		t.Fatalf("predictions = %d, want 2: %+v", len(out.Predictions), out.Predictions)
	}
	if out.Predictions[0].Session != "session-1" || out.Predictions[1].Session != "session-2" {
		t.Errorf("predictions ordering wrong: %+v", out.Predictions)
	}
	if len(out.Overlaps) != 1 || out.Overlaps[0].Path != "shared.go" {
		t.Errorf("overlaps wrong: %+v", out.Overlaps)
	}
}

func TestPredict_RejectsEmptyPlan(t *testing.T) {
	withStubPredictor(t, &stubPredictor{})

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_predict",
		Arguments: map[string]any{"plan": ""},
	})
	if err == nil && !result.IsError {
		t.Fatalf("empty plan should be rejected, got %+v", result)
	}
}

func TestPredict_RejectsOversizedPlan(t *testing.T) {
	withStubPredictor(t, &stubPredictor{})

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	huge := strings.Repeat("a", maxPredictPlanBytes+1)
	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_predict",
		Arguments: map[string]any{"plan": huge},
	})
	if err == nil && !result.IsError {
		t.Fatalf("oversized plan should be rejected, got %+v", result)
	}
}

func TestPredict_PlanWithNoSessionsFails(t *testing.T) {
	withStubPredictor(t, &stubPredictor{})

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_predict",
		Arguments: map[string]any{"plan": "# heading\nnarrative only\n"},
	})
	if err == nil && !result.IsError {
		t.Fatalf("plan with no session headings should be rejected, got %+v", result)
	}
}

func TestPredict_NilSlicesSerializeAsEmpty(t *testing.T) {
	// Predictor returns nil for both — the JSON wire shape must still be
	// [] so callers can iterate without a nil check.
	withStubPredictor(t, &stubPredictor{})

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_predict",
		Arguments: map[string]any{"plan": planTwoLanes},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected IsError: %+v", result)
	}

	var out PredictResult
	data, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Predictions == nil {
		t.Errorf("Predictions is nil — should be empty slice")
	}
	if out.Overlaps == nil {
		t.Errorf("Overlaps is nil — should be empty slice")
	}
}

func TestPredict_AdvertisesTool(t *testing.T) {
	_, sess, cancel := newPipedSession(t)
	defer cancel()

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var advertised *mcpsdk.Tool
	for _, tl := range tools.Tools {
		if tl.Name == "bosun_predict" {
			advertised = tl
			break
		}
	}
	if advertised == nil {
		t.Fatalf("bosun_predict missing from tool list: %+v", tools.Tools)
	}
	// The description spells out the heuristic-only caveat so a model
	// caller knows not to over-trust the output.
	desc := strings.ToLower(advertised.Description)
	for _, want := range []string{"heuristic", "over-trust"} {
		if !strings.Contains(desc, want) {
			t.Errorf("tool description missing %q caveat:\n%s", want, advertised.Description)
		}
	}
}
