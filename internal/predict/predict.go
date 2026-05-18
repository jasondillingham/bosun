package predict

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
)

// Heuristic implements Predictor purely via regex/heuristic parsing of
// brief bodies. No LSP, no AST.
//
// The forecast is intentionally conservative: a brief that doesn't name
// any files (e.g. "refactor the auth module") produces zero predictions.
// Per the v0.1 spec, we'd rather miss an overlap than fabricate one and
// teach the operator to ignore false positives.
type Heuristic struct{}

// New returns a default heuristic predictor.
func New() *Heuristic { return &Heuristic{} }

// Predict implements [Predictor].
func (h *Heuristic) Predict(briefs []brief.Brief) ([]Prediction, []Overlap, error) {
	preds := make([]Prediction, 0, len(briefs))
	for _, b := range briefs {
		label := labelOf(b)
		paths, sources, avoid, warned := extractPaths(b.Body)
		pred := Prediction{
			Session:   label,
			Scope:     firstNonEmptyLine(b.Body),
			Paths:     make([]PredictedPath, 0, len(paths)),
			Predicted: paths,
			Source:    sources,
			Avoid:     avoid,
			Warned:    warned,
		}
		for i, p := range paths {
			reason := ""
			if i < len(sources) {
				reason = sources[i]
			}
			pred.Paths = append(pred.Paths, PredictedPath{Path: p, Reason: reason})
		}
		preds = append(preds, pred)
	}
	sort.Slice(preds, func(i, j int) bool {
		return preds[i].Session < preds[j].Session
	})
	overlaps := detectOverlaps(preds)
	return preds, overlaps, nil
}

// firstNonEmptyLine returns the first non-blank line of body, trimmed.
// Used for Prediction.Scope — gives the operator a glanceable summary
// without dragging in the whole brief.
func firstNonEmptyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return trimmed
		}
	}
	return ""
}

func labelOf(b brief.Brief) string {
	if b.Label != "" {
		return b.Label
	}
	if b.Session > 0 {
		return fmt.Sprintf("session-%d", b.Session)
	}
	return "session-?"
}

// pathRe matches path-like tokens with at least one slash. The first
// segment must start with a letter or underscore (filters protocol-like
// `http://…` strings since they'd start with `h` but contain a `:`,
// which isn't in our charset). Subsequent segments allow dots, dashes,
// stars, and other path-safe glob chars.
var pathRe = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_.-]*(?:/[a-zA-Z0-9_.*?-]+)+/?`)

// fenceRe finds the body of every fenced code block. Captures the
// content so we can mark paths inside as high-confidence — code fences
// in briefs are nearly always concrete file references.
var fenceRe = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n(.*?)\n```")

// inlineCodeRe matches single-backtick code spans like `foo/bar.go`.
// The contract: a path the operator wrapped in backticks is an
// intentional, concrete reference — count it as a claim even in
// "Do NOT modify `X`" constraint clauses. Unquoted prose paths fall
// through to the warned-off bucket instead. Excludes backtick and
// newline so a span can't run away across paragraphs.
var inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")

// knownExts gates which extensions we accept as "this is probably a
// file path." A bare token like `path/filepath` (a Go import path, not
// a file) has no extension and would otherwise be predicted; gating on
// known extensions keeps the forecast conservative.
var knownExts = map[string]bool{
	".go": true, ".md": true, ".json": true, ".yaml": true, ".yml": true,
	".toml": true, ".sh": true, ".bash": true, ".zsh": true, ".txt": true,
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true,
	".css": true, ".scss": true, ".html": true, ".htm": true, ".sql": true,
	".py": true, ".rs": true, ".rb": true, ".java": true, ".kt": true,
	".c": true, ".h": true, ".cpp": true, ".cc": true, ".hpp": true,
	".tmpl": true, ".tpl": true, ".tf": true, ".proto": true, ".env": true,
	".dockerfile": true, ".lock": true, ".mod": true, ".sum": true,
}

// extractPaths returns four parallel views of the brief body:
//
//   - paths:   the predicted claims (owned list, fenced code blocks,
//     single-backtick spans).
//   - sources: a parallel slice of source labels explaining why each
//     was predicted.
//   - avoid:   paths the brief explicitly forbids ("Files (avoid)").
//   - warned:  path-like tokens that appeared only in plain prose with
//     no backticks — informational, not claims. Surfaced separately so
//     the overlap calculation doesn't fire on constraint clauses like
//     "Do NOT modify internal/config/..." appearing in multiple briefs.
//
// Predicted entries are deduped preserving the first occurrence
// (highest-confidence source wins: owned list → code block → backtick).
// Avoid-listed paths are excluded from predicted and warned. A path
// already claimed is never duplicated into warned.
//
// The context split is the fix for the architect-mcp regression
// (issue #17): treating prose mentions as claims produced 17
// overlap warnings on a plan with 3 real overlaps because every
// brief's "do not touch X" constraint prose was matching every other
// brief's constraint prose. Backticked references remain claims —
// the operator chose to call them out.
func extractPaths(body string) ([]string, []string, []string, []string) {
	type entry struct {
		path   string
		source string
	}
	var entries []entry
	seen := make(map[string]struct{})

	// Pre-compute the avoid set so later scans skip anything the brief
	// has explicitly told the session NOT to touch.
	avoidRaw := extractListSection(body, "Files (avoid)")
	avoidSet := make(map[string]struct{}, len(avoidRaw))
	avoid := make([]string, 0, len(avoidRaw))
	for _, p := range avoidRaw {
		clean := cleanPath(p)
		if clean == "" || !isPathLike(clean) {
			continue
		}
		if _, dup := avoidSet[clean]; dup {
			continue
		}
		avoidSet[clean] = struct{}{}
		avoid = append(avoid, clean)
	}

	add := func(raw, src string) {
		p := cleanPath(raw)
		if p == "" || !isPathLike(p) {
			return
		}
		if _, blocked := avoidSet[p]; blocked {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		entries = append(entries, entry{path: p, source: src})
	}

	// 1. Explicit owned list — highest confidence. The avoid list is
	//    intentionally NOT added to predicted: it documents what the
	//    session must steer clear of, not what it's going to edit.
	for _, p := range extractListSection(body, "Files (own)") {
		add(p, "owned list")
	}

	// 2. Code-block fences — concrete file references almost always.
	for _, m := range fenceRe.FindAllStringSubmatch(body, -1) {
		for _, p := range pathRe.FindAllString(m[1], -1) {
			add(p, "code block")
		}
	}

	// Strip fenced blocks before scanning inline backticks so a fence
	// delimiter doesn't accidentally close-then-reopen as a span.
	stripped := fenceRe.ReplaceAllString(body, "")

	// 3. Single-backtick spans — claims. Operator chose to wrap the
	//    path in backticks, which signals an intentional reference even
	//    inside a constraint clause.
	for _, m := range inlineCodeRe.FindAllStringSubmatch(stripped, -1) {
		for _, p := range pathRe.FindAllString(m[1], -1) {
			add(p, "backtick reference")
		}
	}

	// 4. Plain prose — informational only. Strip out the backtick spans
	//    first so any remaining path-like tokens are guaranteed prose,
	//    then collect them into the warned-off bucket. These never feed
	//    the overlap calc — see the function header for the rationale.
	prose := inlineCodeRe.ReplaceAllString(stripped, " ")
	warnedSeen := make(map[string]struct{})
	var warned []string
	for _, raw := range pathRe.FindAllString(prose, -1) {
		p := cleanPath(raw)
		if p == "" || !isPathLike(p) {
			continue
		}
		if _, claimed := seen[p]; claimed {
			continue
		}
		if _, blocked := avoidSet[p]; blocked {
			continue
		}
		if _, dup := warnedSeen[p]; dup {
			continue
		}
		warnedSeen[p] = struct{}{}
		warned = append(warned, p)
	}

	// 5. Test co-location — every claimed non-test `.go` file implies
	//    the matching `_test.go` even if the brief never names it.
	//    Runs over claims only (not warned), so prose mentions don't
	//    drag tests into the prediction either. Snapshot the loop
	//    bound so the additions don't recurse.
	for i, n := 0, len(entries); i < n; i++ {
		e := entries[i]
		if strings.HasSuffix(e.path, ".go") && !strings.HasSuffix(e.path, "_test.go") {
			testPath := strings.TrimSuffix(e.path, ".go") + "_test.go"
			add(testPath, "test co-location for "+e.path)
		}
	}

	paths := make([]string, len(entries))
	sources := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
		sources[i] = e.source
	}
	return paths, sources, avoid, warned
}

// cleanPath strips markdown noise (backticks, surrounding quotes,
// trailing punctuation) that pathRe might otherwise include. Returns
// "" if nothing path-like remains.
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "`'\"()[],;:")
	// Strip trailing punctuation that survives the regex (a path at the
	// end of a sentence: "edit internal/foo.go.").
	for len(p) > 0 {
		last := p[len(p)-1]
		if last == '.' && !strings.HasSuffix(p, "..") {
			// A trailing dot after a known extension is a sentence stop.
			// Otherwise the dot is part of the filename.
			ext := path.Ext(strings.TrimSuffix(p, "."))
			if knownExts[strings.ToLower(ext)] {
				p = p[:len(p)-1]
				continue
			}
		}
		if last == ',' || last == ';' || last == ':' {
			p = p[:len(p)-1]
			continue
		}
		break
	}
	return p
}

// isPathLike gates which tokens count as predicted paths. Three forms
// qualify:
//
//   - Ends with `/` — directory glob ("all work in internal/auth/").
//   - Contains a wildcard — glob pattern ("cmd/bosun/cmd_*.go").
//   - Has a recognized file extension — concrete file ("foo/bar.go").
//
// Bare Go-style import paths ("path/filepath") fall through and aren't
// predicted, which matches the conservative bias the brief calls for.
func isPathLike(p string) bool {
	if strings.HasSuffix(p, "/") {
		return true
	}
	if strings.ContainsAny(p, "*?") {
		return true
	}
	ext := strings.ToLower(path.Ext(p))
	return ext != "" && knownExts[ext]
}

// extractListSection finds a markdown list under a heading like
// `Files (own):` (with or without `**` bold markers) and returns the
// path-like tokens from each bullet. Stops at the next blank line or
// non-list line.
func extractListSection(body, heading string) []string {
	lines := strings.Split(body, "\n")
	var paths []string
	inSection := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		trim = strings.TrimPrefix(trim, "**")
		trim = strings.TrimSuffix(trim, "**")
		trim = strings.TrimSuffix(trim, ":")
		if !inSection {
			if strings.EqualFold(trim, heading) {
				inSection = true
			}
			continue
		}
		raw := strings.TrimSpace(line)
		if raw == "" {
			inSection = false
			continue
		}
		if !strings.HasPrefix(raw, "-") && !strings.HasPrefix(raw, "*") {
			// Not a bullet — either a new heading or a stray line. Stop.
			inSection = false
			continue
		}
		paths = append(paths, pathRe.FindAllString(line, -1)...)
	}
	return paths
}

// detectOverlaps returns the cross-session overlap report. For each
// unordered pair of predictions it walks every (a, b) path combination
// and keeps the strongest classification per colliding path.
func detectOverlaps(preds []Prediction) []Overlap {
	var overlaps []Overlap
	for i := 0; i < len(preds); i++ {
		for j := i + 1; j < len(preds); j++ {
			overlaps = append(overlaps, pairOverlap(preds[i], preds[j])...)
		}
	}
	sort.SliceStable(overlaps, func(i, j int) bool {
		if sevRank(overlaps[i].Severity) != sevRank(overlaps[j].Severity) {
			return sevRank(overlaps[i].Severity) < sevRank(overlaps[j].Severity)
		}
		if overlaps[i].Path != overlaps[j].Path {
			return overlaps[i].Path < overlaps[j].Path
		}
		return strings.Join(overlaps[i].Sessions, ",") <
			strings.Join(overlaps[j].Sessions, ",")
	})
	return overlaps
}

func sevRank(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	}
	return 3
}

func pairOverlap(a, b Prediction) []Overlap {
	// Best severity per colliding path (high < medium < low).
	best := make(map[string]string)
	order := []string{}
	upgrade := func(p, sev string) {
		if cur, ok := best[p]; !ok || sevRank(sev) < sevRank(cur) {
			if _, seen := best[p]; !seen {
				order = append(order, p)
			}
			best[p] = sev
		}
	}
	for _, pa := range a.Paths {
		for _, pb := range b.Paths {
			collide, sev := classifyOverlap(pa.Path, pb.Path)
			if sev == "" {
				continue
			}
			upgrade(collide, sev)
		}
	}

	// Suppress "low" sibling overlaps when a stronger overlap already
	// covers the same directory — once we know two sessions share a
	// concrete file in `internal/auth/`, also reporting "they happen
	// to share the `internal/auth/` directory" is noise.
	coveredDirs := map[string]bool{}
	for _, p := range order {
		if best[p] == "low" {
			continue
		}
		d := p
		if !isDir(d) {
			d = path.Dir(d) + "/"
		}
		coveredDirs[d] = true
	}

	out := make([]Overlap, 0, len(order))
	for _, p := range order {
		if best[p] == "low" && coveredDirs[p] {
			continue
		}
		out = append(out, Overlap{
			Path:     p,
			Sessions: []string{a.Session, b.Session},
			Severity: best[p],
		})
	}
	return out
}

// classifyOverlap returns the colliding identifier and severity for two
// predicted paths, or ("", "") when they don't collide. Severity tiers
// mirror the brief:
//
//   - "high":   exact concrete-file equality.
//   - "medium": one side is a directory or glob and covers the other,
//     or both are globs whose literal prefixes line up.
//   - "low":    two concrete files in the same directory; flagged in
//     case the heuristic missed a shared file lower in the tree.
func classifyOverlap(a, b string) (string, string) {
	na, nb := normalizePath(a), normalizePath(b)
	if na == "" || nb == "" {
		return "", ""
	}
	if na == nb {
		if isDir(na) || hasGlob(na) {
			return na, "medium"
		}
		return na, "high"
	}
	// Directory containment.
	if isDir(na) && pathInDir(nb, na) {
		return na, "medium"
	}
	if isDir(nb) && pathInDir(na, nb) {
		return nb, "medium"
	}
	// Glob containment.
	if hasGlob(na) && globContains(na, nb) {
		return na, "medium"
	}
	if hasGlob(nb) && globContains(nb, na) {
		return nb, "medium"
	}
	// Same parent directory but different files.
	if !isDir(na) && !isDir(nb) && !hasGlob(na) && !hasGlob(nb) {
		da, db := path.Dir(na), path.Dir(nb)
		if da == db && da != "." && da != "/" {
			return da + "/", "low"
		}
	}
	return "", ""
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasSuffix(p, "/**") {
		p = strings.TrimSuffix(p, "**")
	}
	return p
}

func isDir(p string) bool {
	return strings.HasSuffix(p, "/")
}

func hasGlob(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

func pathInDir(p, dir string) bool {
	d := strings.TrimSuffix(dir, "/")
	if d == "" {
		return false
	}
	return strings.HasPrefix(p, d+"/")
}

// globContains reports whether the glob pattern covers the given path.
// Handles `*` (non-slash chars) and `**` (any chars). Falls back to
// path.Match for simple patterns; for `**`-bearing patterns we convert
// to a regex with the same semantics as suggest.globToRegex.
func globContains(pattern, target string) bool {
	target = strings.TrimSuffix(target, "/")
	if !strings.Contains(pattern, "**") {
		if ok, _ := path.Match(pattern, target); ok {
			return true
		}
		// A pattern like `cmd/bosun/cmd_*.go` should also cover a bare
		// directory mention of `cmd/bosun/`; path.Match wouldn't catch
		// that, but the dir-containment branch above already does.
		return false
	}
	re, err := globToRegex(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(target)
}

func globToRegex(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	i := 0
	for i < len(p) {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString(`(?:.*/)?`)
					i += 3
				} else {
					b.WriteString(`.*`)
					i += 2
				}
			} else {
				b.WriteString(`[^/]*`)
				i++
			}
		case '?':
			b.WriteString(`[^/]`)
			i++
		case '.', '+', '(', ')', '|', '{', '}', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}
