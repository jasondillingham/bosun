package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/predict"
	"github.com/spf13/cobra"
)

// predictReport is the wire shape for `bosun predict --json` and the
// matching MCP tool. Mirrors the slices session-2's predictor returns so
// callers can deserialise into the same types they would import from
// internal/predict.
type predictReport struct {
	Predictions []predict.Prediction `json:"predictions"`
	Overlaps    []predict.Overlap    `json:"overlaps"`
}

// predictDeps is the injection seam tests use to swap in a stub
// Predictor. Production wiring fills it with predict.DefaultPredictor.
type predictDeps struct {
	predictor predict.Predictor
}

func newPredictCmd() *cobra.Command {
	return newPredictCmdWithDeps(predictDeps{predictor: predict.New()})
}

// newPredictCmdWithDeps is the testable constructor — accepts an injected
// Predictor so unit tests don't have to exercise session-2's real
// heuristic to validate the report rendering and exit-code behaviour.
func newPredictCmdWithDeps(deps predictDeps) *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "predict <plan-file>",
		Short: "Heuristically predict per-session paths and overlaps for a plan",
		Long: `Reads a plan markdown, runs the predictor's heuristic over each
brief, and reports the predicted paths per session plus any flagged
overlaps. The heuristic is filename-mention / package-pattern only — it
catches the obvious collisions, not every static-analysis edge case.
Exit code is 0 when no overlaps are predicted, 1 when any are flagged
(operator decides severity).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPredict(cmd.OutOrStdout(), args[0], jsonOut, deps)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON ({predictions, overlaps})")

	return cmd
}

// runPredict is the testable command body. Parses the plan, calls the
// injected Predictor, then prints either the human report or JSON. The
// return value is a userErr/internalErr — main maps that to an exit code.
func runPredict(w io.Writer, planPath string, jsonOut bool, deps predictDeps) error {
	if deps.predictor == nil {
		return internalErr("predict", fmt.Errorf("predictor not configured"))
	}

	briefs, err := brief.Parse(planPath)
	if err != nil {
		return userErr("%v", err)
	}
	if len(briefs) == 0 {
		return userErr("plan %s has no `## session-N` headings", planPath)
	}

	predictions, overlaps, err := deps.predictor.Predict(briefs)
	if err != nil {
		return internalErr("predict", err)
	}

	if jsonOut {
		report := predictReport{Predictions: predictions, Overlaps: overlaps}
		// Initialise nil slices to empty so consumers see [] not null.
		if report.Predictions == nil {
			report.Predictions = []predict.Prediction{}
		}
		if report.Overlaps == nil {
			report.Overlaps = []predict.Overlap{}
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return internalErr("encode json", err)
		}
		fmt.Fprintln(w, string(data))
	} else {
		renderPredictReport(w, planPath, predictions, overlaps)
	}

	if len(overlaps) > 0 {
		// Use a non-zero exit without an extra error message — the report
		// itself names the colliding lanes and mitigation.
		return userErr("predicted %d overlap(s) — see report above", len(overlaps))
	}
	return nil
}

// renderPredictReport writes a human-readable summary: a per-session block
// listing scope + predicted paths with reasons, then an Overlaps section
// if any were flagged.
func renderPredictReport(w io.Writer, planPath string, predictions []predict.Prediction, overlaps []predict.Overlap) {
	fmt.Fprintf(w, "Predicted conflict report for %s\n", planPath)
	fmt.Fprintln(w)

	// Stable per-session order so two runs over the same plan produce
	// byte-identical reports — easier for operators eyeballing diffs.
	sorted := append([]predict.Prediction(nil), predictions...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Session < sorted[j].Session
	})

	for _, p := range sorted {
		fmt.Fprintf(w, "%s\n", p.Session)
		if p.Scope != "" {
			fmt.Fprintf(w, "  scope: %s\n", p.Scope)
		}
		if len(p.Paths) == 0 {
			fmt.Fprintln(w, "  paths: (none predicted)")
			continue
		}
		fmt.Fprintln(w, "  paths:")
		for _, pp := range p.Paths {
			if pp.Reason != "" {
				fmt.Fprintf(w, "    - %s  (%s)\n", pp.Path, pp.Reason)
			} else {
				fmt.Fprintf(w, "    - %s\n", pp.Path)
			}
		}
	}

	fmt.Fprintln(w)
	if len(overlaps) == 0 {
		fmt.Fprintln(w, "Overlaps: none predicted.")
		return
	}

	fmt.Fprintf(w, "Overlaps: %d\n", len(overlaps))
	for _, o := range overlaps {
		sessions := strings.Join(o.Sessions, ", ")
		severity := o.Severity
		if severity == "" {
			severity = "unknown"
		}
		fmt.Fprintf(w, "  - [%s] %s (sessions: %s)\n", severity, o.Path, sessions)
		if o.Mitigation != "" {
			fmt.Fprintf(w, "      suggestion: %s\n", o.Mitigation)
		}
	}
}
