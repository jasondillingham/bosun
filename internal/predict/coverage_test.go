package predict

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeTree materialises a map of relative-path → content into a temp
// repo root. Every test in this file uses it so the scanner runs against
// real bytes on disk rather than a stubbed walker.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// findingsByCategory groups a finding slice for assertions that don't
// care about per-line ordering, just "this category fired on this file."
func findingsByCategory(f []CoverageFinding) map[string][]CoverageFinding {
	out := make(map[string][]CoverageFinding)
	for _, x := range f {
		out[x.Category] = append(out[x.Category], x)
	}
	return out
}

func TestScanCoverage_DefaultFlags_AllFourCategories(t *testing.T) {
	root := writeTree(t, map[string]string{
		"personal.go":      "package main\n\n// path is /Users/jason/code/foo.go\n",
		"host.go":          "package main\n\nconst host = \"thor:11434\"\n",
		"hostlocal.go":     "package main\n\nconst host = \"raspi.local\"\n",
		"secret.go":        "package main\n\nconst k = \"sk-abc123def456ghi789jkl0\"\n",
		"todo.go":          "package main\n\n// TODO: replace with real impl\n",
		"clean.go":         "package main\n\nfunc Add(a, b int) int { return a + b }\n",
	})

	got, err := ScanCoverage(root, nil, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	by := findingsByCategory(got)

	cases := []struct {
		category string
		file     string
	}{
		{FlagPersonalPath, "personal.go"},
		{FlagInternalHost, "host.go"},
		{FlagInternalHost, "hostlocal.go"},
		{FlagPossibleSecret, "secret.go"},
		{FlagTodo, "todo.go"},
	}
	for _, c := range cases {
		hits := by[c.category]
		found := false
		for _, h := range hits {
			if h.File == c.file {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in %s; got findings=%+v", c.category, c.file, got)
		}
	}
	for _, h := range got {
		if h.File == "clean.go" {
			t.Errorf("clean.go shouldn't trigger any flag, got %+v", h)
		}
	}
}

func TestScanCoverage_ClaimedFilesAreCovered(t *testing.T) {
	root := writeTree(t, map[string]string{
		"internal/auth/handlers.go":   "// TODO: refactor\n",
		"internal/storage/db.go":      "// TODO: migrate\n",
		"docs/notes.md":               "TODO sort this out\n",
	})

	// "internal/auth/" claims the directory; "internal/storage/db.go"
	// claims a literal file. docs/notes.md is unclaimed and should
	// appear as a finding.
	claimed := []string{
		"internal/auth/",
		"internal/storage/db.go",
	}
	got, err := ScanCoverage(root, claimed, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	if len(got) != 1 || got[0].File != "docs/notes.md" {
		t.Fatalf("expected one finding in docs/notes.md, got %+v", got)
	}
}

func TestScanCoverage_GlobClaimCovers(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cmd/bosun/cmd_predict.go": "// TODO: implement coverage\n",
		"cmd/bosun/cmd_status.go":  "// FIXME: wire watch mode\n",
		"internal/other.go":         "// XXX: hack\n",
	})
	claimed := []string{"cmd/bosun/cmd_*.go"}
	got, err := ScanCoverage(root, claimed, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	if len(got) != 1 || got[0].File != "internal/other.go" {
		t.Fatalf("expected only internal/other.go uncovered; got %+v", got)
	}
}

func TestScanCoverage_GlobalExcludesSkipDirs(t *testing.T) {
	root := writeTree(t, map[string]string{
		".git/HEAD":           "// TODO: ignored — under .git\n",
		"vendor/lib/lib.go":   "// TODO: ignored — vendored\n",
		"internal/keep.go":    "// TODO: surfaced\n",
	})
	got, err := ScanCoverage(root, nil, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	for _, h := range got {
		if strings.HasPrefix(h.File, ".git/") || strings.HasPrefix(h.File, "vendor/") {
			t.Errorf("excluded path leaked through: %+v", h)
		}
	}
	if len(got) != 1 || got[0].File != "internal/keep.go" {
		t.Fatalf("expected one finding in internal/keep.go; got %+v", got)
	}
}

func TestScanCoverage_PerFlagExclude(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/notes.md":     "TODO sort this later\n",
		"internal/foo.go":   "// TODO real code\n",
	})
	cfg := DefaultCoverageConfig()
	// Per-flag exclude for TODOs in docs/**.
	for i := range cfg.Flags {
		if cfg.Flags[i].Name == FlagTodo {
			cfg.Flags[i].Excludes = []string{"docs/**"}
		}
	}
	got, err := ScanCoverage(root, nil, cfg)
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	if len(got) != 1 || got[0].File != "internal/foo.go" {
		t.Fatalf("docs/** should be excluded for todo; got %+v", got)
	}
}

func TestScanCoverage_BinaryFilesSkipped(t *testing.T) {
	// File with embedded NUL byte should be skipped even if the bytes
	// contain a string that would otherwise trigger a flag.
	root := writeTree(t, map[string]string{
		"image.bin": "GIF89a\x00\x00\x00TODO sneaky\n",
	})
	got, err := ScanCoverage(root, nil, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("binary file shouldn't be scanned; got %+v", got)
	}
}

func TestScanCoverage_OneFindingPerFileFlagPair(t *testing.T) {
	// Three TODOs in one file should produce one finding (the first
	// match wins per file/flag pair). Per the brief: a coverage gap is
	// about whether the file is unowned, not how many lines hit.
	root := writeTree(t, map[string]string{
		"x.go": "// TODO one\n// TODO two\n// TODO three\n",
	})
	got, err := ScanCoverage(root, nil, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected single finding, got %+v", got)
	}
	if got[0].Line != 1 {
		t.Errorf("expected first-match line 1, got line %d", got[0].Line)
	}
}

func TestScanCoverage_FindingsSortedByFileLine(t *testing.T) {
	root := writeTree(t, map[string]string{
		"b.go": "// TODO b\n",
		"a.go": "// TODO a\n",
		"c.go": "// TODO c\n",
	})
	got, err := ScanCoverage(root, nil, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 findings, got %d (%+v)", len(got), got)
	}
	want := []string{"a.go", "b.go", "c.go"}
	for i, w := range want {
		if got[i].File != w {
			t.Errorf("findings[%d].File = %q, want %q", i, got[i].File, w)
		}
	}
}

func TestScanCoverage_ArchitectMCPRegression(t *testing.T) {
	// The brief calls out this specific dogfood case: a plan that
	// claimed cmd/bosun/** and internal/storage/** but missed
	// internal/screenshot/screenshot.go (vault reference) and
	// internal/mcp/blueprint_tool.go (personal path string). The
	// scanner must surface both files as coverage gaps so the
	// operator catches them before init.
	root := writeTree(t, map[string]string{
		"cmd/bosun/cmd_predict.go":             "package main\n\nfunc main() {}\n",
		"internal/storage/db.go":               "package storage\n",
		"internal/screenshot/screenshot.go":    "package screenshot\n\nconst url = \"http://vault:8200/v1\"\n",
		"internal/mcp/blueprint_tool.go":       "package mcp\n\nconst sample = \"/Users/jason/.config/bosun.json\"\n",
	})
	claimed := []string{"cmd/bosun/", "internal/storage/"}

	got, err := ScanCoverage(root, claimed, DefaultCoverageConfig())
	if err != nil {
		t.Fatalf("ScanCoverage: %v", err)
	}
	by := findingsByCategory(got)

	hostHit := false
	for _, h := range by[FlagInternalHost] {
		if h.File == "internal/screenshot/screenshot.go" {
			hostHit = true
		}
	}
	if !hostHit {
		t.Errorf("regression: screenshot.go should fire internal-host for 'vault'; got %+v", got)
	}

	pathHit := false
	for _, h := range by[FlagPersonalPath] {
		if h.File == "internal/mcp/blueprint_tool.go" {
			pathHit = true
		}
	}
	if !pathHit {
		t.Errorf("regression: blueprint_tool.go should fire personal-path for '/Users/jason/'; got %+v", got)
	}

	// And neither cmd/bosun/cmd_predict.go nor internal/storage/db.go
	// should appear (they're covered by lane claims even though they
	// contain no flagged content).
	for _, h := range got {
		if strings.HasPrefix(h.File, "cmd/bosun/") || strings.HasPrefix(h.File, "internal/storage/") {
			t.Errorf("claimed path leaked into report: %+v", h)
		}
	}
}

func TestLoadCoverageConfig_NoFile_ReturnsDefaults(t *testing.T) {
	root := t.TempDir()
	cfg, err := LoadCoverageConfig(root)
	if err != nil {
		t.Fatalf("LoadCoverageConfig: %v", err)
	}
	want := DefaultCoverageConfig()
	if len(cfg.Flags) != len(want.Flags) {
		t.Errorf("flags = %d, want %d", len(cfg.Flags), len(want.Flags))
	}
}

func TestLoadCoverageConfig_DisableBuiltin(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `# Disable the todo flag entirely.
[flags.todo]
enabled = false
`
	if err := os.WriteFile(filepath.Join(root, ".bosun", "predict-flags.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadCoverageConfig(root)
	if err != nil {
		t.Fatalf("LoadCoverageConfig: %v", err)
	}
	for _, f := range cfg.Flags {
		if f.Name == FlagTodo {
			t.Errorf("expected todo flag removed, still present")
		}
	}
	if len(cfg.Flags) != len(DefaultCoverageConfig().Flags)-1 {
		t.Errorf("flag count = %d, want one less than default", len(cfg.Flags))
	}
}

func TestLoadCoverageConfig_CustomFlag(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[flags.custom-bad-word]
enabled = true
regex = "(?i)\\b(vault|styx[\\s-]?vanguard)\\b"
description = "project-specific terms that shouldn't ship publicly"
exclude = ["docs/**", "**/testdata/**"]
`
	if err := os.WriteFile(filepath.Join(root, ".bosun", "predict-flags.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadCoverageConfig(root)
	if err != nil {
		t.Fatalf("LoadCoverageConfig: %v", err)
	}

	var custom *FlagDef
	for i := range cfg.Flags {
		if cfg.Flags[i].Name == "custom-bad-word" {
			custom = &cfg.Flags[i]
			break
		}
	}
	if custom == nil {
		t.Fatalf("custom flag not found; flags=%+v", cfg.Flags)
	}
	if !custom.Regex.MatchString("vault") {
		t.Errorf("custom regex should match 'vault'")
	}
	if !custom.Regex.MatchString("styx-vanguard") {
		t.Errorf("custom regex should match 'styx-vanguard'")
	}
	if custom.Description != "project-specific terms that shouldn't ship publicly" {
		t.Errorf("description = %q", custom.Description)
	}
	if len(custom.Excludes) != 2 || custom.Excludes[0] != "docs/**" || custom.Excludes[1] != "**/testdata/**" {
		t.Errorf("excludes = %+v", custom.Excludes)
	}
}

func TestLoadCoverageConfig_OverrideBuiltinRegex(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[flags.internal-host]
regex = "\\b(mybox|otherbox)\\b"
`
	if err := os.WriteFile(filepath.Join(root, ".bosun", "predict-flags.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadCoverageConfig(root)
	if err != nil {
		t.Fatalf("LoadCoverageConfig: %v", err)
	}
	var f *FlagDef
	for i := range cfg.Flags {
		if cfg.Flags[i].Name == FlagInternalHost {
			f = &cfg.Flags[i]
			break
		}
	}
	if f == nil {
		t.Fatal("internal-host missing")
	}
	if f.Regex.MatchString("thor") {
		t.Errorf("overridden regex shouldn't match the default 'thor'")
	}
	if !f.Regex.MatchString("mybox") {
		t.Errorf("overridden regex should match 'mybox'")
	}
}

func TestLoadCoverageConfig_BadRegex_IsError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[flags.bad]
enabled = true
regex = "[unclosed"
`
	if err := os.WriteFile(filepath.Join(root, ".bosun", "predict-flags.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCoverageConfig(root); err == nil {
		t.Fatal("expected error for malformed regex")
	}
}

func TestLoadCoverageConfig_CustomFlagWithoutRegex_IsError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[flags.no-regex]
enabled = true
description = "missing the regex field"
`
	if err := os.WriteFile(filepath.Join(root, ".bosun", "predict-flags.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCoverageConfig(root); err == nil {
		t.Fatal("expected error for custom flag missing regex")
	}
}

func TestLoadCoverageConfig_UnknownKey_IsError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[flags.x]
enabled = true
regex = "x"
mystery = "what is this"
`
	if err := os.WriteFile(filepath.Join(root, ".bosun", "predict-flags.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCoverageConfig(root); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestLoadCoverageConfig_TrailingCommentOnLine(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[flags.personal-path]
enabled = false  # we don't care about leaked paths here
`
	if err := os.WriteFile(filepath.Join(root, ".bosun", "predict-flags.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadCoverageConfig(root)
	if err != nil {
		t.Fatalf("LoadCoverageConfig: %v", err)
	}
	for _, f := range cfg.Flags {
		if f.Name == FlagPersonalPath {
			t.Errorf("personal-path should be disabled")
		}
	}
}

func TestClaimedPaths_DedupesAcrossSessions(t *testing.T) {
	preds := []Prediction{
		{Session: "session-1", Paths: []PredictedPath{
			{Path: "internal/auth/handlers.go"},
			{Path: "internal/auth/"},
		}},
		{Session: "session-2", Paths: []PredictedPath{
			{Path: "internal/auth/handlers.go"}, // dup
			{Path: "internal/storage/"},
		}},
	}
	got := ClaimedPaths(preds)
	if len(got) != 3 {
		t.Fatalf("expected 3 unique paths, got %d: %+v", len(got), got)
	}
}

func TestClaimMatcher_LiteralDirGlob(t *testing.T) {
	m := newClaimMatcher([]string{
		"internal/auth/handlers.go",
		"internal/storage/",
		"cmd/bosun/cmd_*.go",
		"./README.md",
	})
	cases := []struct {
		path string
		want bool
	}{
		{"internal/auth/handlers.go", true},
		{"internal/auth/other.go", false},
		{"internal/storage/db.go", true},
		{"internal/storage/sub/sub.go", true},
		{"cmd/bosun/cmd_predict.go", true},
		{"cmd/bosun/main.go", false},
		{"README.md", true},
		{"docs/note.md", false},
	}
	for _, c := range cases {
		got := m.covers(c.path)
		if got != c.want {
			t.Errorf("covers(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestAnyGlobMatches_HandlesDoubleStar(t *testing.T) {
	patterns := []string{"vendor/**", "docs/**"}
	cases := []struct {
		target string
		want   bool
	}{
		{"vendor/lib.go", true},
		{"vendor", true},
		{"vendor/", true},
		{"docs/notes.md", true},
		{"internal/foo.go", false},
	}
	for _, c := range cases {
		if got := anyGlobMatches(patterns, c.target); got != c.want {
			t.Errorf("anyGlobMatches(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

func TestParseFlagsTOML_StringArrayWithQuotedComma(t *testing.T) {
	// A comma inside a quoted string must not split the array. Edge
	// case but worth pinning so the hand-rolled parser doesn't drift.
	in := []byte(`[flags.x]
enabled = true
regex = ","
exclude = ["a,b", "c"]
`)
	got, err := parseFlagsTOML(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("sections = %d", len(got))
	}
	if len(got[0].excludes) != 2 || got[0].excludes[0] != "a,b" {
		t.Errorf("excludes = %+v, want [a,b c]", got[0].excludes)
	}
}

func TestParseFlagsTOML_Boundary(t *testing.T) {
	// Validate the parser rejects schemas the override file shouldn't
	// be allowed to use — top-level keys, non-flags sections — so an
	// operator typo produces a clear error rather than silent ignore.
	cases := []struct {
		name string
		body string
	}{
		{"top-level-key", `foo = "bar"`},
		{"non-flags-section", "[other.section]\nkey = \"v\"\n"},
		{"empty-flag-name", "[flags.]\nregex = \".\"\n"},
		{"missing-equals", "[flags.x]\nenabled\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseFlagsTOML([]byte(c.body)); err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

// regexUnescapeTOML is a sanity check that the unescape path preserves
// the regex backslashes the brief example uses. Specifically:
// `\\b(vault|styx[\\s-]?vanguard)\\b` should round-trip through to a
// regex that matches both "vault" and "styx-vanguard".
func TestUnescapeTOMLString_PreservesRegexBackslashes(t *testing.T) {
	in := `(?i)\\b(vault|styx[\\s-]?vanguard)\\b`
	out, err := unescapeTOMLString(in)
	if err != nil {
		t.Fatalf("unescape: %v", err)
	}
	re, err := regexp.Compile(out)
	if err != nil {
		t.Fatalf("compile %q: %v", out, err)
	}
	for _, in := range []string{"vault", "STYX VANGUARD", "styx-vanguard"} {
		if !re.MatchString(in) {
			t.Errorf("regex didn't match %q (compiled: %q)", in, out)
		}
	}
}
