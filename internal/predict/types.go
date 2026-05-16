// Package predict implements bosun's predictive-conflict heuristic.
// It scans the parsed brief markdown for each session and forecasts
// which file paths the agent is likely to touch, then reports any
// overlaps between sessions so the operator can resolve collisions
// before spawning the worktrees.
//
// This file holds the package contract — the shared types every
// caller (CLI, MCP) depends on. The lane that lands first creates
// this file; later lanes import it. Coordinate via `bosun_check`
// before changing anything here.
package predict

import "github.com/jasondillingham/bosun/internal/brief"

// Prediction is the heuristic forecast for one session — files we
// suspect the agent will touch based on the brief text.
type Prediction struct {
	Session   string   // "session-1" / "auth" / etc.
	Predicted []string // path globs the heuristic extracted from the brief
	Source    []string // why each path was predicted (for operator
	// review — pairs 1:1 with Predicted)
}

// Overlap is one pair of sessions whose predictions intersect.
type Overlap struct {
	Path     string   // the colliding pattern (or first concrete file)
	Sessions []string // labels of the colliding sessions
	Severity string   // "high" (concrete file) / "medium" (dir glob) / "low" (heuristic guess)
}

// Predictor is the interface CLI + MCP tools depend on.
type Predictor interface {
	Predict(briefs []brief.Brief) ([]Prediction, []Overlap, error)
}
