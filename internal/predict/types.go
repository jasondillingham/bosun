// Package predict implements bosun's predictive-conflict heuristic.
// It scans the parsed brief markdown for each session and forecasts
// which file paths the agent is likely to touch, then reports any
// overlaps between sessions so the operator can resolve collisions
// before spawning the worktrees.
//
// This file holds the package contract — the shared types every
// caller (CLI, MCP) depends on. Coordinate via `bosun_check` before
// changing anything here.
package predict

import "github.com/jasondillingham/bosun/internal/brief"

// Predictor returns predictions and overlaps for a slice of briefs. Used
// as a dependency-injection seam by CLI and MCP tool callers so tests
// can swap in a canned-result stub without exercising the real heuristic.
type Predictor interface {
	Predict(briefs []brief.Brief) ([]Prediction, []Overlap, error)
}

// Prediction summarises one session's expected scope and the per-path
// reasons the heuristic flagged each entry. Scope is a free-form one-liner
// pulled from the brief body (typically the first non-empty line).
type Prediction struct {
	Session string          `json:"session"`
	Scope   string          `json:"scope,omitempty"`
	Paths   []PredictedPath `json:"paths"`

	// Predicted and Source are JSON-ignored parallel-array views of Paths,
	// populated alongside it by the heuristic. They exist because the
	// session-2 predictor tests pre-date the richer PredictedPath shape;
	// keeping the views around avoids rewriting that test surface in
	// this merge. New callers should use Paths.
	Predicted []string `json:"-"`
	Source    []string `json:"-"`

	// Avoid is the set of paths the brief explicitly told this session
	// NOT to touch (parsed from the "Files (avoid)" list). These paths
	// are excluded from Predicted / Paths — the predictor surfaces them
	// here so an operator-side check or future MCP tool can flag a
	// claim that crosses into another session's avoid lane.
	Avoid []string `json:"avoid,omitempty"`
}

// PredictedPath is one path the heuristic expects a session to touch and
// the short reason it was inferred ("mentioned in brief", "matched
// internal/<pkg>/", etc.).
type PredictedPath struct {
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

// Overlap is one predicted cross-session collision. Severity is an
// implementation-defined string ("high"/"medium"/"low" is the expected
// vocabulary). Mitigation is the operator-facing suggestion — e.g.
// "narrow lane X to avoid the internal/auth/** glob."
type Overlap struct {
	Path       string   `json:"path"`
	Sessions   []string `json:"sessions"`
	Severity   string   `json:"severity"`
	Mitigation string   `json:"mitigation,omitempty"`
}
