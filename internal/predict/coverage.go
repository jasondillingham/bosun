package predict

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Default flag category names. These are stable identifiers — they appear
// in CoverageFinding.Category and as the TOML section name an operator
// uses to disable or tune a built-in flag (`[flags.personal-path]
// enabled = false`).
const (
	FlagPersonalPath   = "personal-path"
	FlagInternalHost   = "internal-host"
	FlagPossibleSecret = "possible-secret"
	FlagTodo           = "todo"
)

// CoverageFinding is one flagged content match in a repo file that no
// claimed lane covers. The report sorts findings by File:Line so two
// runs over the same tree produce identical output.
type CoverageFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Category string `json:"category"`
	Match    string `json:"match,omitempty"`
}

// FlagDef defines one heuristic. Built-in flags get a stable Name from
// the Flag* constants above; custom flags from .bosun/predict-flags.toml
// use whatever section name the operator chose.
//
// Excludes are glob patterns evaluated against the path being scanned
// (slash-separated, repo-relative). They stack on top of the global
// CoverageConfig.Excludes — a file matching either is skipped for this
// flag only.
type FlagDef struct {
	Name        string
	Regex       *regexp.Regexp
	Description string
	Excludes    []string
}

// CoverageConfig is the active flag set plus the global exclude list.
// Flags is order-preserving (built-ins first, then customs in TOML order)
// so the rendered report has stable category ordering.
type CoverageConfig struct {
	Flags    []FlagDef
	Excludes []string
}

// DefaultCoverageConfig returns the built-in flag set with global excludes
// of `.git/**` and `vendor/**`. The four built-in flags target the
// "you'd be embarrassed if a stranger saw this" cases the architect-mcp
// dogfood surfaced: leaked personal paths, internal hostnames, secret
// shapes, and forgotten TODOs.
func DefaultCoverageConfig() CoverageConfig {
	return CoverageConfig{
		Flags: []FlagDef{
			{
				Name: FlagPersonalPath,
				// /Users/<name>/, /home/<name>/, C:\Users\<name>\.
				// <name> is lowercase letter then [a-z0-9-] — matches real
				// usernames without flagging /Users/.local or /home/ alone.
				Regex:       regexp.MustCompile(`(?:/Users/|/home/|C:\\Users\\)[a-z][a-z0-9-]*[/\\]`),
				Description: "leaked personal filesystem path",
			},
			{
				Name: FlagInternalHost,
				// Two shapes: well-known homelab-flavoured short names that
				// shouldn't ship in public code, and any `<host>.local` mDNS
				// hostname. The short-name list is intentionally tight to
				// keep false positives low; the override mechanism is the
				// right place for per-repo additions.
				Regex:       regexp.MustCompile(`\b(?:thor|vault|valkey|prometheus|grafana|loki|synology|truenas|pihole)\b|\b[a-z][a-z0-9-]+\.local\b`),
				Description: "looks like an internal hostname",
			},
			{
				Name: FlagPossibleSecret,
				// Shapes only — we don't validate the token, just flag it
				// for human review. The 32+ hex string branch is gated on
				// surrounding quotes so plain-prose SHA-1s and commit
				// hashes in markdown don't trip it.
				Regex:       regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9_-]{20,}|glp_[A-Za-z0-9_-]{20,}|AKIA[A-Z0-9]{16})\b|"[0-9a-fA-F]{32,}"`),
				Description: "shape matches a common API token / secret",
			},
			{
				Name:        FlagTodo,
				Regex:       regexp.MustCompile(`\b(?:TODO|FIXME|XXX)\b`),
				Description: "TODO / FIXME / XXX marker",
			},
		},
		Excludes: []string{".git/**", "vendor/**"},
	}
}

// LoadCoverageConfig reads `.bosun/predict-flags.toml` from repoRoot and
// returns the active CoverageConfig. If the file is absent, the built-in
// set is returned unchanged. Override semantics:
//
//   - `[flags.<built-in>]` with `enabled = false` removes the built-in.
//   - `[flags.<built-in>]` with a `regex = "…"` replaces the built-in's
//     pattern (keeping the same Name so the report category is stable).
//   - `[flags.<custom>]` defines a new flag; `regex` is required.
//   - `exclude = ["…"]` on any flag adds per-flag globs.
//
// Errors are returned for malformed TOML, an unparseable regex, or a
// custom flag missing its regex.
func LoadCoverageConfig(repoRoot string) (CoverageConfig, error) {
	cfg := DefaultCoverageConfig()
	path := filepath.Join(repoRoot, ".bosun", "predict-flags.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	sections, err := parseFlagsTOML(data)
	if err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	// Index built-ins by name so overrides find them in O(1).
	byName := make(map[string]int, len(cfg.Flags))
	for i, f := range cfg.Flags {
		byName[f.Name] = i
	}
	keep := make([]bool, len(cfg.Flags))
	for i := range keep {
		keep[i] = true
	}

	for _, s := range sections {
		if !s.enabled {
			if idx, ok := byName[s.name]; ok {
				keep[idx] = false
			}
			continue
		}
		if idx, ok := byName[s.name]; ok {
			// Built-in override: keep the name, optionally swap regex /
			// description, append excludes. Operators almost always just
			// want to silence a category — handle the bare `enabled =
			// true` case as a no-op rather than requiring all the
			// supporting fields.
			if s.regex != "" {
				re, err := regexp.Compile(s.regex)
				if err != nil {
					return cfg, fmt.Errorf("flag %q regex: %w", s.name, err)
				}
				cfg.Flags[idx].Regex = re
			}
			if s.description != "" {
				cfg.Flags[idx].Description = s.description
			}
			cfg.Flags[idx].Excludes = append(cfg.Flags[idx].Excludes, s.excludes...)
			continue
		}
		// Custom flag — regex required.
		if s.regex == "" {
			return cfg, fmt.Errorf("custom flag %q is missing a regex", s.name)
		}
		re, err := regexp.Compile(s.regex)
		if err != nil {
			return cfg, fmt.Errorf("flag %q regex: %w", s.name, err)
		}
		cfg.Flags = append(cfg.Flags, FlagDef{
			Name:        s.name,
			Regex:       re,
			Description: s.description,
			Excludes:    s.excludes,
		})
	}

	// Apply disables in reverse so indices stay valid.
	filtered := cfg.Flags[:0]
	for i, f := range cfg.Flags {
		if i < len(keep) && !keep[i] {
			continue
		}
		filtered = append(filtered, f)
	}
	cfg.Flags = filtered
	return cfg, nil
}

// ClaimedPaths flattens every per-session prediction into one slice of
// raw path tokens. ScanCoverage uses it to decide whether a flagged file
// is already covered by a lane. Duplicates are dropped; ordering is not
// significant (the matcher iterates the slice).
func ClaimedPaths(predictions []Prediction) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, p := range predictions {
		for _, pp := range p.Paths {
			if pp.Path == "" {
				continue
			}
			if _, dup := seen[pp.Path]; dup {
				continue
			}
			seen[pp.Path] = struct{}{}
			out = append(out, pp.Path)
		}
	}
	return out
}

// ScanCoverage walks repoRoot and returns one CoverageFinding per
// (file, flag) hit in files that aren't covered by any claimed path.
//
// Performance contract from the brief: a repo of 100k+ LOC must complete
// in under 10s (most finish in under 2s). To hit that, the scanner:
//
//   - skips excluded directories at WalkDir time so their trees aren't
//     stat'd at all
//   - skips files larger than maxScanBytes (default 2 MiB) — secrets in
//     a 10MB minified blob aren't what this is for
//   - sniffs the first 512 bytes for a NUL byte and skips binaries
//   - reads line-by-line with bufio.Scanner and bails on the first
//     match per (file, flag) — we don't need every match, just whether
//     this file is a coverage gap
func ScanCoverage(repoRoot string, claimed []string, cfg CoverageConfig) ([]CoverageFinding, error) {
	return scanCoverageWithLimit(repoRoot, claimed, cfg, maxScanBytes)
}

// maxScanBytes caps the per-file size we'll read into the regex. 2 MiB
// is generous for source files; anything larger is almost certainly a
// generated artifact or a fixture.
const maxScanBytes = 2 << 20

func scanCoverageWithLimit(repoRoot string, claimed []string, cfg CoverageConfig, sizeLimit int64) ([]CoverageFinding, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("repoRoot is empty")
	}
	repoRoot = filepath.Clean(repoRoot)
	matcher := newClaimMatcher(claimed)

	var findings []CoverageFinding
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A single unreadable directory mid-walk shouldn't abort the
			// whole scan; the operator gets a partial report rather than
			// nothing at all.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if anyGlobMatches(cfg.Excludes, rel) || anyGlobMatches(cfg.Excludes, rel+"/") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if anyGlobMatches(cfg.Excludes, rel) {
			return nil
		}
		if matcher.covers(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > sizeLimit {
			return nil
		}
		hits, err := scanFile(p, rel, cfg.Flags)
		if err != nil {
			// Permission denied / disappeared mid-walk — skip the file but
			// keep scanning. A torn read shouldn't break the report.
			return nil
		}
		findings = append(findings, hits...)
		return nil
	})
	if err != nil {
		return findings, err
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Category < findings[j].Category
	})
	return findings, nil
}

// scanFile reads p and returns one CoverageFinding per flag that matches
// anywhere in the file. Once a flag fires the scanner moves to the next
// flag — the report doesn't need every match per file.
func scanFile(absPath, relPath string, flags []FlagDef) ([]CoverageFinding, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 4096)
	// Binary sniff: a NUL byte in the first 512 bytes is a strong
	// "this is not source" signal. We peek so the bytes stay available
	// to the line scanner.
	head, _ := br.Peek(512)
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, nil
	}

	// Filter the flag set to those that apply to this file once, instead
	// of re-checking per line.
	applicable := make([]FlagDef, 0, len(flags))
	hit := make([]bool, 0, len(flags))
	for _, f := range flags {
		if anyGlobMatches(f.Excludes, relPath) {
			continue
		}
		applicable = append(applicable, f)
		hit = append(hit, false)
	}
	if len(applicable) == 0 {
		return nil, nil
	}

	scanner := bufio.NewScanner(br)
	// Source lines can run long (minified or generated). Allow up to 1 MiB
	// per line before giving up, which keeps us robust on real-world files
	// without inviting unbounded allocation.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var findings []CoverageFinding
	lineNo := 0
	remaining := len(applicable)
	for scanner.Scan() {
		lineNo++
		if remaining == 0 {
			break
		}
		line := scanner.Bytes()
		for i := range applicable {
			if hit[i] {
				continue
			}
			loc := applicable[i].Regex.FindIndex(line)
			if loc == nil {
				continue
			}
			hit[i] = true
			remaining--
			findings = append(findings, CoverageFinding{
				File:     relPath,
				Line:     lineNo,
				Category: applicable[i].Name,
				Match:    truncateMatch(string(line[loc[0]:loc[1]])),
			})
		}
	}
	// scanner.Err() — ignore truncation and IO errors here; partial
	// results are still useful.
	return findings, nil
}

// truncateMatch trims a long match to keep the report scannable. The
// brief's example output shows short snippets in parentheses, so 64
// chars is plenty. Multi-line matches collapse to single-line.
func truncateMatch(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 64 {
		s = s[:61] + "..."
	}
	return s
}

// claimMatcher answers "is this file claimed by any lane?" Built once
// per scan so the per-file check is O(N) over a small N. Three claim
// shapes are recognised, matching the predictor's output vocabulary:
//
//   - directory: trailing '/' — claims everything beneath it
//   - glob: contains '*' or '?'  — matched via globContains
//   - literal: equality (plus matching a file inside if the path is the
//     literal)
type claimMatcher struct {
	literals map[string]bool
	dirs     []string
	globs    []string
}

func newClaimMatcher(claimed []string) *claimMatcher {
	m := &claimMatcher{literals: make(map[string]bool)}
	for _, c := range claimed {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		// Strip leading "./" so claims written like "./internal/foo.go"
		// align with the slash-form relative paths the walker emits.
		c = strings.TrimPrefix(c, "./")
		if isDir(c) {
			m.dirs = append(m.dirs, strings.TrimSuffix(c, "/"))
			continue
		}
		if hasGlob(c) {
			m.globs = append(m.globs, c)
			continue
		}
		m.literals[c] = true
	}
	return m
}

func (m *claimMatcher) covers(rel string) bool {
	if m == nil {
		return false
	}
	if m.literals[rel] {
		return true
	}
	for _, d := range m.dirs {
		if d == "" {
			continue
		}
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	for _, g := range m.globs {
		if globContains(g, rel) {
			return true
		}
	}
	return false
}

// anyGlobMatches returns true if any pattern in patterns matches target.
// Patterns use the same `**` / `*` / `?` vocabulary as globContains. An
// empty pattern slice means "nothing matches."
func anyGlobMatches(patterns []string, target string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if globContains(p, target) {
			return true
		}
		// A pattern like `vendor/**` should also match the directory
		// `vendor` itself (so the walker can SkipDir before descending);
		// globContains strips the trailing slash from target but won't
		// otherwise relate "vendor" to "vendor/**". Add an explicit
		// shortcut for that case.
		if stripped := strings.TrimSuffix(p, "/**"); stripped != p && (target == stripped || strings.HasPrefix(target, stripped+"/")) {
			return true
		}
	}
	return false
}

// flagSection is the in-flight parser output for one `[flags.<name>]`
// block in the override TOML. enabled defaults to true so a section that
// just supplies a regex doesn't have to repeat `enabled = true`.
type flagSection struct {
	name        string
	enabled     bool
	regex       string
	description string
	excludes    []string
}

// parseFlagsTOML parses the constrained TOML dialect the override file
// uses. It is intentionally NOT a full TOML parser — the goal is to
// avoid pulling a third-party dep for a tiny config schema (see
// CLAUDE.md's dependency policy). Recognised forms:
//
//   - `# comment` / `key = value # trailing comment`
//   - `[flags.<name>]` section headers (the only sections supported)
//   - `key = true` / `key = false`
//   - `key = "double-quoted string"` (with `\\`, `\"`, `\n`, `\t`, `\r`)
//   - `key = ["str1", "str2"]` (single-line array of strings)
//
// Anything else is an error so an operator who mistypes the schema gets
// a precise complaint instead of a silent ignore.
func parseFlagsTOML(data []byte) ([]flagSection, error) {
	var sections []flagSection
	var cur *flagSection
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		stripped := stripComment(raw)
		stripped = strings.TrimSpace(stripped)
		if stripped == "" {
			continue
		}
		if strings.HasPrefix(stripped, "[") && strings.HasSuffix(stripped, "]") {
			name := strings.TrimSpace(stripped[1 : len(stripped)-1])
			if !strings.HasPrefix(name, "flags.") {
				return nil, fmt.Errorf("line %d: only [flags.<name>] sections are supported, got [%s]", lineNo, name)
			}
			name = strings.TrimPrefix(name, "flags.")
			if name == "" {
				return nil, fmt.Errorf("line %d: empty flag name in [flags.]", lineNo)
			}
			if cur != nil {
				sections = append(sections, *cur)
			}
			cur = &flagSection{name: name, enabled: true}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: %q is outside any [flags.<name>] section", lineNo, stripped)
		}
		eq := strings.IndexByte(stripped, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected `key = value`, got %q", lineNo, stripped)
		}
		key := strings.TrimSpace(stripped[:eq])
		val := strings.TrimSpace(stripped[eq+1:])
		switch key {
		case "enabled":
			b, err := parseTOMLBool(val)
			if err != nil {
				return nil, fmt.Errorf("line %d: enabled: %w", lineNo, err)
			}
			cur.enabled = b
		case "regex":
			s, err := parseTOMLString(val)
			if err != nil {
				return nil, fmt.Errorf("line %d: regex: %w", lineNo, err)
			}
			cur.regex = s
		case "description":
			s, err := parseTOMLString(val)
			if err != nil {
				return nil, fmt.Errorf("line %d: description: %w", lineNo, err)
			}
			cur.description = s
		case "exclude":
			arr, err := parseTOMLStringArray(val)
			if err != nil {
				return nil, fmt.Errorf("line %d: exclude: %w", lineNo, err)
			}
			cur.excludes = append(cur.excludes, arr...)
		default:
			return nil, fmt.Errorf("line %d: unknown key %q", lineNo, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if cur != nil {
		sections = append(sections, *cur)
	}
	return sections, nil
}

// stripComment removes a `#` comment when the `#` is not inside a
// quoted string. Bare `#` characters in regexes need to be quoted, which
// the TOML string form already requires anyway.
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch c {
		case '\\':
			if inQuote && i+1 < len(line) {
				i++ // skip the escaped char
			}
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

func parseTOMLBool(v string) (bool, error) {
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false, got %q", v)
}

func parseTOMLString(v string) (string, error) {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", fmt.Errorf("expected double-quoted string, got %q", v)
	}
	return unescapeTOMLString(v[1 : len(v)-1])
}

func unescapeTOMLString(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			return "", fmt.Errorf("trailing backslash in string")
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			// Pass unrecognised escapes through verbatim (regex backslashes
			// like \b, \d, \s). The TOML spec is stricter; we trade
			// strictness for ergonomic regex authoring.
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}

func parseTOMLStringArray(v string) ([]string, error) {
	if len(v) < 2 || v[0] != '[' || v[len(v)-1] != ']' {
		return nil, fmt.Errorf("expected `[...]`, got %q", v)
	}
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	// Hand-rolled split: respect quoted strings so a comma inside a
	// pattern (rare but legal) doesn't split the array.
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(inner):
			cur.WriteByte(c)
			cur.WriteByte(inner[i+1])
			i++
		case c == '"':
			cur.WriteByte(c)
			inQuote = !inQuote
		case c == ',' && !inQuote:
			s, err := parseTOMLString(strings.TrimSpace(cur.String()))
			if err != nil {
				return nil, err
			}
			out = append(out, s)
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if rem := strings.TrimSpace(cur.String()); rem != "" {
		s, err := parseTOMLString(rem)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
