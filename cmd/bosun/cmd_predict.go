package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/predict"
	"github.com/spf13/cobra"
)

// predictReport is the wire shape for `bosun predict --json` and the
// matching MCP tool. Mirrors the slices session-2's predictor returns so
// callers can deserialise into the same types they would import from
// internal/predict.
type predictReport struct {
	Predictions []predict.Prediction       `json:"predictions"`
	Overlaps    []predict.Overlap          `json:"overlaps"`
	Coverage    []predict.CoverageFinding  `json:"coverage,omitempty"`
}

// predictDeps is the injection seam tests use to swap in a stub
// Predictor (and, for --coverage, a stubbed scanner so unit tests don't
// have to materialise a temp repo on every run).
type predictDeps struct {
	predictor predict.Predictor

	// coverageScanner runs the scanner against the repo. The default
	// wires through to predict.ScanCoverage + LoadCoverageConfig; tests
	// override it with a closure that returns canned findings.
	coverageScanner func(repoRoot string, claimed []string) ([]predict.CoverageFinding, error)

	// repoRootFn returns the repo root used by coverageScanner. Default
	// resolves to `git rev-parse --show-toplevel` of cwd; tests pass a
	// stub so the test doesn't need a real git checkout.
	repoRootFn func() (string, error)
}

func newPredictCmd() *cobra.Command {
	return newPredictCmdWithDeps(predictDeps{
		predictor:       predict.New(),
		coverageScanner: defaultCoverageScanner,
		repoRootFn:      defaultRepoRootFn,
	})
}

// defaultCoverageScanner loads the override config and runs the scanner.
// Wired as the production coverageScanner so unit tests can replace it.
func defaultCoverageScanner(repoRoot string, claimed []string) ([]predict.CoverageFinding, error) {
	cfg, err := predict.LoadCoverageConfig(repoRoot)
	if err != nil {
		return nil, err
	}
	return predict.ScanCoverage(repoRoot, claimed, cfg)
}

// defaultRepoRootFn resolves the current working directory's repo root.
// Returns a clean userErr when bosun is invoked outside a git checkout —
// the coverage scanner needs a tree to walk.
func defaultRepoRootFn() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	c := git.New()
	root, err := c.RepoRoot(context.Background(), cwd)
	if err != nil {
		return "", fmt.Errorf("not inside a git repository (cwd=%s)", cwd)
	}
	return root, nil
}

// newPredictCmdWithDeps is the testable constructor — accepts an injected
// Predictor so unit tests don't have to exercise session-2's real
// heuristic to validate the report rendering and exit-code behaviour.
func newPredictCmdWithDeps(deps predictDeps) *cobra.Command {
	var jsonOut bool
	var coverage bool

	cmd := &cobra.Command{
		Use:   "predict <plan-file>",
		Short: "Heuristically predict per-session paths and overlaps for a plan",
		Long: `Reads a plan markdown, runs the predictor's heuristic over each
brief, and reports the predicted paths per session plus any flagged
overlaps. The heuristic is filename-mention / package-pattern only — it
catches the obvious collisions, not every static-analysis edge case.
Exit code is 0 when no overlaps are predicted, 1 when any are flagged
(operator decides severity).

With --coverage, the predictor also walks the repo for content that
"a stranger shouldn't see" (personal paths, internal hostnames,
secret-shaped tokens, TODOs) and reports any file with such content
that no lane claims. The flag set is configurable via
.bosun/predict-flags.toml — see docs/predict-coverage-flags.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPredict(cmd.OutOrStdout(), args[0], jsonOut, coverage, deps)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON ({predictions, overlaps, coverage?})")
	cmd.Flags().BoolVar(&coverage, "coverage", false, "scan the repo for files with flagged content that no lane claims")

	cmd.GroupID = "wiring"
	return cmd
}

// runPredict is the testable command body. Parses the plan, calls the
// injected Predictor, then prints either the human report or JSON. The
// return value is a userErr/internalErr — main maps that to an exit code.
//
// When coverage is true, the scanner runs after the predictor and the
// gap list is appended to the human report (or to the JSON
// "coverage" field). Exit code is 1 if EITHER overlaps OR coverage gaps
// are found — the brief specifies the same shape for both signals.
func runPredict(w io.Writer, planPath string, jsonOut, coverage bool, deps predictDeps) error {
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

	var coverageFindings []predict.CoverageFinding
	if coverage {
		if deps.coverageScanner == nil || deps.repoRootFn == nil {
			return internalErr("predict", fmt.Errorf("coverage scanner not configured"))
		}
		root, err := deps.repoRootFn()
		if err != nil {
			return userErr("%v", err)
		}
		claimed := predict.ClaimedPaths(predictions)
		coverageFindings, err = deps.coverageScanner(root, claimed)
		if err != nil {
			return userErr("scan coverage: %v", err)
		}
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
		if coverage {
			report.Coverage = coverageFindings
			if report.Coverage == nil {
				report.Coverage = []predict.CoverageFinding{}
			}
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return internalErr("encode json", err)
		}
		fmt.Fprintln(w, string(data))
	} else {
		renderPredictReport(w, planPath, predictions, overlaps)
		if coverage {
			renderCoverageReport(w, coverageFindings)
		}
	}

	if len(overlaps) > 0 || len(coverageFindings) > 0 {
		// Use a non-zero exit without an extra error message — the report
		// itself names the colliding lanes and mitigation.
		switch {
		case len(overlaps) > 0 && len(coverageFindings) > 0:
			return userErr("predicted %d overlap(s) and %d coverage gap(s) — see report above", len(overlaps), len(coverageFindings))
		case len(overlaps) > 0:
			return userErr("predicted %d overlap(s) — see report above", len(overlaps))
		default:
			return userErr("found %d coverage gap(s) — see report above", len(coverageFindings))
		}
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

// renderCoverageReport prints the Coverage gaps block. Layout matches
// the brief's example output: one entry per file:line with the category
// and (when available) the matched substring, plus a one-line
// suggestion when gaps exist. A zero-finding scan prints a "no gaps"
// confirmation so the operator can tell the flag actually ran.
func renderCoverageReport(w io.Writer, findings []predict.CoverageFinding) {
	fmt.Fprintln(w)
	if len(findings) == 0 {
		fmt.Fprintln(w, "Coverage gaps: none — every flagged file is in some lane's scope.")
		return
	}

	// Width-pad "file:line" so the category column lines up across rows
	// — easier to scan when one filename is much longer than another.
	maxLeft := 0
	leftCol := make([]string, len(findings))
	for i, f := range findings {
		leftCol[i] = fmt.Sprintf("%s:%d", f.File, f.Line)
		if n := len(leftCol[i]); n > maxLeft {
			maxLeft = n
		}
	}

	noun := "file has"
	if len(findings) > 1 {
		noun = "files have"
	}
	fmt.Fprintf(w, "Coverage gaps (%d %s flagged content but no lane claims them):\n", len(findings), noun)
	for i, f := range findings {
		tag := f.Category
		if f.Match != "" && f.Category != predict.FlagTodo {
			tag = fmt.Sprintf("%s: '%s'", f.Category, f.Match)
		}
		fmt.Fprintf(w, "  %-*s  (%s)\n", maxLeft, leftCol[i], tag)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Suggestion: add these to a lane's scope, or assign an audit lane.")
}
