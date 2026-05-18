package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/predict"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name: "bosun_predict",
			Description: "Heuristically predict per-session paths and overlaps for a plan markdown body. " +
				"Input is the plan content as a string (not a path); the tool parses the same `## session-N` " +
				"headings `bosun init --brief` reads. Returns {predictions: [{session, scope, paths}], " +
				"overlaps: [{path, sessions, severity, mitigation}]}. " +
				"The predictor is heuristic-only — filename mentions, code-fence excerpts, and common " +
				"package patterns. It catches obvious collisions; do not over-trust the output. " +
				"An empty overlaps slice means \"no obvious collision predicted,\" not \"safe to merge.\"",
		}, s.toolPredict)
	})
}

// PredictArgs is the input schema for bosun_predict. Plan is the raw
// markdown body — pass the file contents, not a path. Capping to a
// generous size keeps an over-eager agent from shoving a multi-MB string
// into the buffer; real plans top out around 10-20 KB.
type PredictArgs struct {
	Plan string `json:"plan" jsonschema:"plan markdown body — the same shape bosun init --brief reads"`
}

// PredictResult is the structured output for bosun_predict.
type PredictResult struct {
	Predictions []predict.Prediction `json:"predictions"`
	Overlaps    []predict.Overlap    `json:"overlaps"`
}

// maxPredictPlanBytes is the inbound size cap. A typical plan is a few KB;
// 256 KB leaves comfortable headroom for verbose plans without inviting
// abuse. Above the cap we reject rather than truncate — silent truncation
// would parse a partial plan and mislead the caller.
const maxPredictPlanBytes = 256 * 1024

// predictPredictor is the package-level injection seam tests use to swap
// in a stub Predictor without modifying Server's struct shape (which is
// session-2-adjacent territory). Tests save the current value, replace
// it, and restore in cleanup.
var predictPredictor predict.Predictor = predict.New()

// toolPredict implements bosun_predict. Writes the plan body to a temp
// file so the existing brief.Parse path is exercised verbatim (same regex,
// same validation, same error shape), then runs the predictor. Tempfile
// instead of an in-memory parser to avoid touching the brief package
// surface — that's not session-3's territory this round.
func (s *Server) toolPredict(_ context.Context, _ *mcp.CallToolRequest, args PredictArgs) (*mcp.CallToolResult, PredictResult, error) {
	if len(args.Plan) == 0 {
		return nil, PredictResult{}, fmt.Errorf("plan is required")
	}
	if len(args.Plan) > maxPredictPlanBytes {
		return nil, PredictResult{}, fmt.Errorf("plan size %d exceeds limit %d bytes", len(args.Plan), maxPredictPlanBytes)
	}

	tmp, err := os.CreateTemp("", "bosun-predict-*.md")
	if err != nil {
		return nil, PredictResult{}, fmt.Errorf("temp plan: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.WriteString(args.Plan); err != nil {
		_ = tmp.Close()
		return nil, PredictResult{}, fmt.Errorf("write temp plan: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, PredictResult{}, fmt.Errorf("close temp plan: %w", err)
	}

	briefs, err := brief.Parse(tmpPath)
	if err != nil {
		// Replace the temp-file path in the error message with a stable
		// placeholder so callers see "<plan>: ..." not the temp path.
		stable := stripTempPath(err.Error(), tmpPath)
		return nil, PredictResult{}, fmt.Errorf("parse plan: %s", stable)
	}
	if len(briefs) == 0 {
		return nil, PredictResult{}, fmt.Errorf("plan has no `## session-N` headings")
	}

	predictor := predictPredictor
	if predictor == nil {
		predictor = predict.New()
	}
	predictions, overlaps, err := predictor.Predict(briefs)
	if err != nil {
		return nil, PredictResult{}, fmt.Errorf("predict: %w", err)
	}

	// Normalise nil slices so the JSON wire shape is always [], never null.
	if predictions == nil {
		predictions = []predict.Prediction{}
	}
	if overlaps == nil {
		overlaps = []predict.Overlap{}
	}

	summary := fmt.Sprintf("predicted %d session(s); %d overlap(s)", len(predictions), len(overlaps))
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
		},
	}, PredictResult{Predictions: predictions, Overlaps: overlaps}, nil
}

// stripTempPath rewrites errors that embed the tempfile path so external
// callers don't see a leaked `/var/folders/.../bosun-predict-1234.md`
// fragment in their error messages.
func stripTempPath(msg, tmpPath string) string {
	if tmpPath == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, tmpPath, "<plan>")
	msg = strings.ReplaceAll(msg, filepath.Base(tmpPath), "<plan>")
	return msg
}
