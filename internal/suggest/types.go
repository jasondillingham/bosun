// Package suggest builds the structured proposal that `bosun suggest`
// hands to a model. Phase 1 (this file + inspect.go) gathers a compact
// snapshot of the repo so the model has shape, recent activity, and
// dependency hints to reason about without burning input tokens on noise.
//
// The contract is intentionally narrow: RepoIntel is everything a model
// needs to propose disjoint parallel lanes, JSON-serialized and capped
// so the prompt stays under budget.
package suggest

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
