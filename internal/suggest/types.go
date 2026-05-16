// Package suggest implements bosun's brief-authoring assistant: a repo
// inspector, a Claude-backed lane proposer, a structural validator, and
// a markdown templater that together turn a high-level goal into a plan
// the operator can hand to `bosun init --brief`.
//
// This file holds the package contract — the shared types and interfaces
// every lane in the v0.5 round-1 plan codes against. Per the round plan:
// the lane that lands first creates this file; later lanes import it.
// Coordinate via `bosun_check` before changing anything here.
package suggest

import (
	"context"
	"time"
)

// RepoIntel is the compact snapshot the model receives. Fields are
// ordered roughly by usefulness to the lane-planning task: language(s)
// first (frames the whole prompt), then shape (file count, histogram,
// top dirs), then a sampled file list (concrete handles the model can
// reference in owned/avoid globs), then recent activity and deps.
//
// The struct is JSON-serialized into the user prompt. Cap is ~6KB;
// fields are truncated in order (file sample first, then dependencies)
// when the serialized payload would exceed it.
type RepoIntel struct {
	// Root is the absolute path to the repository root. Optional —
	// callers that don't set it leave it empty; the proposer doesn't
	// require it, but `bosun suggest --inspect-only` surfaces it for
	// the operator and session-2's claude-client tests reference it.
	Root string `json:"root,omitempty"`

	// GeneratedAt is the wall-clock time the snapshot was produced.
	// Helps reproducibility audits when the same goal yields different
	// proposals across runs.
	GeneratedAt time.Time `json:"generated_at,omitempty"`

	// Languages lists the languages detected from manifest presence
	// (e.g. "go" for go.mod, "node" for package.json). Multi-language
	// repos return every language detected; ordering is stable
	// (alphabetical) so the same repo state produces the same snapshot.
	Languages []string `json:"languages"`

	// FileCount is the total number of tracked files (git ls-files).
	FileCount int `json:"file_count"`

	// ExtensionHistogram is the top 10 extensions by file count.
	// Extensions are lower-cased and include the leading dot (".go").
	// Files without an extension are bucketed under "" and may appear
	// if they fall in the top 10.
	ExtensionHistogram []ExtCount `json:"extension_histogram"`

	// TopDirs is first-level directories under the repo root, sorted
	// by descending file count. Skips .git, .bosun, node_modules,
	// vendor, and any directory starting with a dot.
	TopDirs []DirCount `json:"top_dirs"`

	// FileSample is up to 200 tracked file paths (relative to repo
	// root, forward slashes). For repos with more than 200 files, the
	// sample is deterministic — seeded from the HEAD SHA — so the same
	// goal + repo state produces the same proposal.
	FileSample []string `json:"file_sample"`

	// RecentCommits is the last 30 commit subjects (oldest last) from
	// `git log -30 --pretty=format:'%s'`. Newlines stripped.
	RecentCommits []string `json:"recent_commits"`

	// Dependencies is a flat list of third-party deps parsed from
	// language-specific manifests (go.mod require, package.json
	// dependencies + devDependencies, etc.). Capped at ~50 entries.
	Dependencies []string `json:"dependencies"`

	// TestLayoutHints is a list of human-readable hints describing
	// where this repo puts its tests (e.g. "Go tests co-located",
	// "Python-style tests dir", "Jest-style __tests__/").
	TestLayoutHints []string `json:"test_layout_hints"`
}

// ExtCount is one bucket of the extension histogram.
type ExtCount struct {
	Ext   string `json:"ext"`
	Count int    `json:"count"`
}

// DirCount is one row of the top-directories list.
type DirCount struct {
	Dir   string `json:"dir"`
	Count int    `json:"count"`
}

// LaneProposal is the structured output of the proposer — N lanes the
// operator can review then turn into a plan markdown.
type LaneProposal struct {
	Version  string `json:"version"`
	Goal     string `json:"goal"`
	Sessions []Lane `json:"sessions"`
}

// Lane is one proposed session in a LaneProposal.
type Lane struct {
	Label      string   `json:"label"`
	Scope      string   `json:"scope"`
	OwnedFiles []string `json:"owned_files"`
	AvoidFiles []string `json:"avoid_files"`
	DependsOn  []string `json:"depends_on"`
	Rationale  string   `json:"rationale"`
	WorkToDo   []string `json:"work_to_do"`
	Notes      string   `json:"notes"`
}

// Inspector is the interface CLI wiring depends on. Production
// implementation lives in inspect.go; tests stub it.
type Inspector interface {
	Inspect(repoRoot string) (RepoIntel, error)
}

// Proposer is the interface CLI wiring depends on. Production
// implementation lives in claude.go; tests stub it.
type Proposer interface {
	Propose(ctx context.Context, goal string, intel RepoIntel, n int) (LaneProposal, error)
}
