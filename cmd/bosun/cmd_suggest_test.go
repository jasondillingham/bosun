package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/suggest"
)

// stubInspector lets tests preload the RepoIntel + error suggest.Inspector
// is expected to return. Records the root passed by the runner so tests
// can assert the canonical-path contract.
type stubInspector struct {
	intel    suggest.RepoIntel
	err      error
	callRoot string
	calls    int
}

func (s *stubInspector) Inspect(root string) (suggest.RepoIntel, error) {
	s.calls++
	s.callRoot = root
	return s.intel, s.err
}

// stubProposer mirrors the same pattern for suggest.Proposer. Records
// the goal + session count so tests can verify the runner threaded them
// through correctly.
type stubProposer struct {
	proposal suggest.LaneProposal
	err      error
	callGoal string
	callN    int
	calls    int
}

func (p *stubProposer) Propose(_ context.Context, goal string, _ suggest.RepoIntel, n int) (suggest.LaneProposal, error) {
	p.calls++
	p.callGoal = goal
	p.callN = n
	return p.proposal, p.err
}

// validProposal builds a two-lane LaneProposal that passes Validate.
func validProposal() suggest.LaneProposal {
	return suggest.LaneProposal{
		Version: "v1",
		Goal:    "demo",
		Sessions: []suggest.Lane{
			{
				Label:      "session-1",
				Scope:      "ground floor",
				OwnedFiles: []string{"internal/a/**"},
				WorkToDo:   []string{"do a thing"},
				Rationale:  "because",
			},
			{
				Label:      "session-2",
				Scope:      "upper floor",
				OwnedFiles: []string{"internal/b/**"},
				DependsOn:  []string{"session-1"},
				WorkToDo:   []string{"do another thing"},
				Rationale:  "because",
			},
		},
	}
}

// overlapProposal forces validateFileOverlap to fire — both lanes own
// the exact same path.
func overlapProposal() suggest.LaneProposal {
	p := validProposal()
	p.Sessions[1].OwnedFiles = []string{"internal/a/**"}
	return p
}

// cycleProposal forces validateDependencies to find a cycle by making
// session-1 depend on session-2 (which already depends on session-1 in
// validProposal()).
func cycleProposal() suggest.LaneProposal {
	p := validProposal()
	p.Sessions[0].DependsOn = []string{"session-2"}
	return p
}

func TestRunSuggest_InspectOnly_ShortCircuits(t *testing.T) {
	intel := suggest.RepoIntel{
		Root:       "/tmp/repo",
		Languages:  []string{"go"},
		FileCount:  42,
		FileSample: []string{"main.go", "go.mod"},
	}
	insp := &stubInspector{intel: intel}
	prop := &stubProposer{} // would explode the test if called

	var buf bytes.Buffer
	err := runSuggest(context.Background(), &buf, "/tmp/repo", suggestOpts{
		goal:        "ignored",
		sessions:    2,
		inspectOnly: true,
		out:         filepath.Join(t.TempDir(), "should-not-be-written.md"),
	}, suggestDeps{inspector: insp, proposer: prop})
	if err != nil {
		t.Fatalf("inspect-only should succeed: %v", err)
	}
	if prop.calls != 0 {
		t.Fatalf("proposer must not be called in --inspect-only mode (got %d calls)", prop.calls)
	}
	if insp.calls != 1 {
		t.Fatalf("inspector should be called exactly once, got %d", insp.calls)
	}

	// Output must be the marshal-indented RepoIntel JSON.
	var got suggest.RepoIntel
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid RepoIntel JSON: %v\n%s", err, buf.String())
	}
	if got.FileCount != intel.FileCount || len(got.Languages) != 1 || got.Languages[0] != "go" {
		t.Errorf("round-tripped RepoIntel doesn't match input:\n got: %+v\nwant: %+v", got, intel)
	}
}

func TestRunSuggest_InspectOnly_DoesNotWriteOutFile(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "no-write.md")
	insp := &stubInspector{intel: suggest.RepoIntel{Languages: []string{"go"}}}

	var buf bytes.Buffer
	if err := runSuggest(context.Background(), &buf, "/repo", suggestOpts{
		goal:        "x",
		sessions:    1,
		inspectOnly: true,
		out:         outPath,
	}, suggestDeps{inspector: insp}); err != nil {
		t.Fatalf("inspect-only: %v", err)
	}
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--inspect-only should not write the plan file, but %s exists", outPath)
	}
}

func TestRunSuggest_HappyPath_WritesPlanAndPrintsNextStep(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "plan.md")
	insp := &stubInspector{intel: suggest.RepoIntel{Languages: []string{"go"}}}
	prop := &stubProposer{proposal: validProposal()}

	var buf bytes.Buffer
	err := runSuggest(context.Background(), &buf, "/repo", suggestOpts{
		goal:     "ship a thing",
		sessions: 2,
		out:      outPath,
	}, suggestDeps{inspector: insp, proposer: prop})
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}

	if prop.callGoal != "ship a thing" {
		t.Errorf("proposer received goal %q, want %q", prop.callGoal, "ship a thing")
	}
	if prop.callN != 2 {
		t.Errorf("proposer received n=%d, want 2", prop.callN)
	}
	if insp.callRoot != "/repo" {
		t.Errorf("inspector received root %q, want %q", insp.callRoot, "/repo")
	}

	out := buf.String()
	for _, want := range []string{
		"Wrote plan to " + outPath,
		"2 sessions",
		"bosun init --brief " + outPath,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("success summary missing %q:\n%s", want, out)
		}
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	planText := string(data)
	for _, want := range []string{"# Plan: demo", "## session-1", "## session-2"} {
		if !strings.Contains(planText, want) {
			t.Errorf("plan markdown missing %q:\n%s", want, planText)
		}
	}
}

func TestRunSuggest_InspectorErrorPropagates(t *testing.T) {
	insp := &stubInspector{err: errors.New("git boom")}
	prop := &stubProposer{}

	var buf bytes.Buffer
	err := runSuggest(context.Background(), &buf, "/repo", suggestOpts{
		goal: "x", sessions: 1, out: filepath.Join(t.TempDir(), "x.md"),
	}, suggestDeps{inspector: insp, proposer: prop})
	if err == nil {
		t.Fatalf("expected inspector error, got nil")
	}
	if !strings.Contains(err.Error(), "inspect repo") || !strings.Contains(err.Error(), "git boom") {
		t.Errorf("error should mention inspect failure and underlying cause, got: %v", err)
	}
	if prop.calls != 0 {
		t.Errorf("proposer should not be called when inspector fails")
	}
}

func TestRunSuggest_ProposerErrorPropagates(t *testing.T) {
	insp := &stubInspector{intel: suggest.RepoIntel{Languages: []string{"go"}}}
	prop := &stubProposer{err: errors.New("anthropic 500")}

	var buf bytes.Buffer
	err := runSuggest(context.Background(), &buf, "/repo", suggestOpts{
		goal: "x", sessions: 1, out: filepath.Join(t.TempDir(), "x.md"),
	}, suggestDeps{inspector: insp, proposer: prop})
	if err == nil {
		t.Fatalf("expected proposer error, got nil")
	}
	if !strings.Contains(err.Error(), "propose lanes") || !strings.Contains(err.Error(), "anthropic 500") {
		t.Errorf("error should mention proposal failure and underlying cause, got: %v", err)
	}
}

// TestRunSuggest_Validation drives the overlap/cycle paths in both the
// strict and --allow-overlaps modes plus the always-fatal schema-error
// path to lock down the exact behavior the brief calls out.
func TestRunSuggest_Validation(t *testing.T) {
	tests := []struct {
		name           string
		proposal       suggest.LaneProposal
		allowOverlaps  bool
		wantErr        bool
		wantInOutput   []string
		wantPlanExists bool
	}{
		{
			name:           "overlap error fails without --allow-overlaps",
			proposal:       overlapProposal(),
			allowOverlaps:  false,
			wantErr:        true,
			wantInOutput:   []string{"validator overlap", `lane "session-1"`, `lane "session-2"`},
			wantPlanExists: false,
		},
		{
			name:           "overlap error proceeds with --allow-overlaps",
			proposal:       overlapProposal(),
			allowOverlaps:  true,
			wantErr:        false,
			wantInOutput:   []string{"validator overlap", "writing plan despite validation error", "Wrote plan to"},
			wantPlanExists: true,
		},
		{
			name:           "cycle error fails without --allow-overlaps",
			proposal:       cycleProposal(),
			allowOverlaps:  false,
			wantErr:        true,
			wantInOutput:   []string{"validator cycle"},
			wantPlanExists: false,
		},
		{
			name:           "cycle error proceeds with --allow-overlaps",
			proposal:       cycleProposal(),
			allowOverlaps:  true,
			wantErr:        false,
			wantInOutput:   []string{"validator cycle", "writing plan despite validation error", "Wrote plan to"},
			wantPlanExists: true,
		},
		{
			name: "schema error is always fatal even with --allow-overlaps",
			// Two lanes when n=3 → validateSchema rejects the count.
			proposal:       validProposal(),
			allowOverlaps:  true,
			wantErr:        true,
			wantInOutput:   []string{"validate plan"},
			wantPlanExists: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "plan.md")
			n := len(tc.proposal.Sessions)
			if tc.name == "schema error is always fatal even with --allow-overlaps" {
				// Force a mismatch: caller asked for 3 but proposer returns 2.
				n = 3
			}

			insp := &stubInspector{intel: suggest.RepoIntel{Languages: []string{"go"}}}
			prop := &stubProposer{proposal: tc.proposal}

			var buf bytes.Buffer
			err := runSuggest(context.Background(), &buf, "/repo", suggestOpts{
				goal:          "g",
				sessions:      n,
				out:           outPath,
				allowOverlaps: tc.allowOverlaps,
			}, suggestDeps{inspector: insp, proposer: prop})

			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil; output:\n%s", buf.String())
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v; output:\n%s", err, buf.String())
			}
			for _, want := range tc.wantInOutput {
				if !strings.Contains(buf.String(), want) && (err == nil || !strings.Contains(err.Error(), want)) {
					t.Errorf("missing %q in output or error.\n  output: %s\n  error: %v",
						want, buf.String(), err)
				}
			}
			_, statErr := os.Stat(outPath)
			planWritten := statErr == nil
			if planWritten != tc.wantPlanExists {
				t.Errorf("plan exists=%v, want=%v (path=%s, statErr=%v)",
					planWritten, tc.wantPlanExists, outPath, statErr)
			}
		})
	}
}

// TestNewSuggestCmd_FlagDefaults pins the flag set and default values so
// a rename or accidental default change shows up in tests rather than
// silently in operator workflows.
func TestNewSuggestCmd_FlagDefaults(t *testing.T) {
	cmd := newSuggestCmd()
	if cmd.Use != "suggest <goal>" {
		t.Errorf("Use = %q, want %q", cmd.Use, "suggest <goal>")
	}
	flags := cmd.Flags()
	cases := []struct {
		name     string
		wantDef  string // string-formatted default
		required bool   // present at all
	}{
		{"sessions", "0", true},
		{"out", "suggested-plan.md", true},
		{"model", "", true},
		{"inspect-only", "false", true},
		{"max-tokens", "0", true},
		{"allow-overlaps", "false", true},
	}
	for _, tc := range cases {
		f := flags.Lookup(tc.name)
		if tc.required && f == nil {
			t.Errorf("flag --%s missing", tc.name)
			continue
		}
		if f == nil {
			continue
		}
		if f.DefValue != tc.wantDef {
			t.Errorf("flag --%s default = %q, want %q", tc.name, f.DefValue, tc.wantDef)
		}
	}
}
