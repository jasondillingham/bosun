package suggest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// intelByteBudget caps the JSON-serialized RepoIntel so the prompt
// budget stays predictable. ~6KB matches the v0.5 spec.
const intelByteBudget = 6144

// inspectGitTimeout caps every git invocation in this file so a hung
// git (under fsync pressure, slow disk, locked index) can't wedge
// `bosun suggest` forever. 30s mirrors the default git_op_timeout
// used elsewhere; suggest is meant to be a few-second pre-flight.
const inspectGitTimeout = 30 * time.Second

// fileSampleCap is the maximum file-sample size before deterministic
// down-sampling kicks in.
const fileSampleCap = 200

// dependencyCap is the maximum number of dependency entries we'll
// surface to the model.
const dependencyCap = 50

// topDirsLimit caps how many first-level directories we report.
const topDirsLimit = 15

// extHistogramLimit caps the extension histogram length.
const extHistogramLimit = 10

// skipDirs are first-level directories excluded from the TopDirs list.
// Anything starting with "." is also skipped (handled in code).
var skipDirs = map[string]struct{}{
	".git":         {},
	".bosun":       {},
	"node_modules": {},
	"vendor":       {},
}

// Inspect gathers a RepoIntel snapshot for repoRoot. The caller is
// expected to pass an existing git worktree root; non-git roots return
// an error.
func Inspect(repoRoot string) (RepoIntel, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return RepoIntel{}, fmt.Errorf("resolve repo root: %w", err)
	}

	files, err := listTrackedFiles(abs)
	if err != nil {
		return RepoIntel{}, err
	}

	intel := RepoIntel{
		Languages:          detectLanguages(abs),
		FileCount:          len(files),
		ExtensionHistogram: extensionHistogram(files),
		TopDirs:            topDirs(files),
		RecentCommits:      recentCommits(abs),
		Dependencies:       capStrings(collectDependencies(abs), dependencyCap),
		TestLayoutHints:    testLayoutHints(files),
	}
	intel.FileSample = sampleFiles(files, fileSampleCap, headRef(abs))

	truncateToBudget(&intel)
	return intel, nil
}

// listTrackedFiles runs `git ls-files` in dir and returns the tracked
// paths (forward-slash, relative to repo root). An empty repo returns
// an empty slice, not an error. Bounded by inspectGitTimeout.
func listTrackedFiles(dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), inspectGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-files")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git ls-files: timed out after %s", inspectGitTimeout)
		}
		return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	out := strings.TrimRight(stdout.String(), "\n")
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// detectLanguages returns the alphabetized list of languages inferred
// from manifest presence at repo root.
func detectLanguages(dir string) []string {
	type probe struct {
		filename string
		lang     string
	}
	probes := []probe{
		{"go.mod", "go"},
		{"package.json", "node"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
		{"setup.py", "python"},
		{"Gemfile", "ruby"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"build.gradle.kts", "java"},
		{"composer.json", "php"},
	}
	seen := map[string]struct{}{}
	for _, p := range probes {
		if _, err := os.Stat(filepath.Join(dir, p.filename)); err == nil {
			seen[p.lang] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for lang := range seen {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// extensionHistogram bins paths by lower-cased extension and returns
// the top extHistogramLimit buckets. Files without an extension bucket
// under "".
func extensionHistogram(files []string) []ExtCount {
	counts := map[string]int{}
	for _, f := range files {
		counts[strings.ToLower(path.Ext(f))]++
	}
	out := make([]ExtCount, 0, len(counts))
	for ext, n := range counts {
		out = append(out, ExtCount{Ext: ext, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Ext < out[j].Ext
	})
	if len(out) > extHistogramLimit {
		out = out[:extHistogramLimit]
	}
	return out
}

// topDirs counts files per first-level directory, skipping dotted dirs
// and the skipDirs set. Files at the repo root are not counted (no
// directory).
func topDirs(files []string) []DirCount {
	counts := map[string]int{}
	for _, f := range files {
		i := strings.IndexByte(f, '/')
		if i <= 0 {
			continue
		}
		d := f[:i]
		if strings.HasPrefix(d, ".") {
			continue
		}
		if _, skip := skipDirs[d]; skip {
			continue
		}
		counts[d]++
	}
	out := make([]DirCount, 0, len(counts))
	for d, n := range counts {
		out = append(out, DirCount{Dir: d, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Dir < out[j].Dir
	})
	if len(out) > topDirsLimit {
		out = out[:topDirsLimit]
	}
	return out
}

// sampleFiles picks at most n entries from files. For oversized
// inputs the selection is seeded by the HEAD SHA so the same repo
// state always produces the same sample (which is what makes the
// downstream proposal stable across invocations). Output preserves
// the original ls-files order.
func sampleFiles(files []string, n int, headSHA string) []string {
	if len(files) == 0 {
		return []string{}
	}
	if len(files) <= n {
		out := make([]string, len(files))
		copy(out, files)
		return out
	}
	seed := seedFromSHA(headSHA, len(files))
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // G404: deterministic file sampling, not security-sensitive
	// Shuffle indices, take the first n, re-sort so output order
	// matches input order — keeps the sample readable.
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	rng.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	picked := idx[:n]
	sort.Ints(picked)
	out := make([]string, n)
	for i, k := range picked {
		out[i] = files[k]
	}
	return out
}

// seedFromSHA derives a stable int64 seed from a hex-encoded git
// object id. Empty / unparseable SHAs fall back to a constant
// derived from the file count so sampling is still deterministic on
// repos without a HEAD ref.
func seedFromSHA(sha string, fallback int) int64 {
	if sha != "" {
		if b, err := hex.DecodeString(sha); err == nil && len(b) >= 8 {
			return int64(binary.BigEndian.Uint64(b[:8])) //nolint:gosec // G115: intentional bit-pattern reuse as a PRNG seed
		}
	}
	// fallback: hash the file count so the same empty-headed repo
	// shape produces the same sample.
	sum := sha256.Sum256([]byte(fmt.Sprintf("bosun-suggest-fallback-%d", fallback)))
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // G115: intentional bit-pattern reuse as a PRNG seed
}

// headRef returns the resolved HEAD SHA as a hex string, or "" if HEAD
// can't be resolved (e.g. empty repo, no commits yet). Bounded by
// inspectGitTimeout; a timeout collapses to "" so suggest can proceed
// with the fallback sampling path.
func headRef(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), inspectGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

// recentCommits returns up to the last 30 commit subjects. Errors
// (e.g. empty repo, hung git) are swallowed — recent activity is a
// hint, not a hard requirement. Bounded by inspectGitTimeout.
func recentCommits(dir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), inspectGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "log", "-30", "--pretty=format:%s")
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil
	}
	out := strings.TrimRight(stdout.String(), "\n")
	if out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return lines
}

// collectDependencies parses dependency lists from the manifests we
// understand (go.mod, package.json). Returns a flat string list.
// Unparseable manifests are skipped — dependency intel is best-effort.
func collectDependencies(dir string) []string {
	var deps []string
	deps = append(deps, parseGoMod(filepath.Join(dir, "go.mod"))...)
	deps = append(deps, parsePackageJSON(filepath.Join(dir, "package.json"))...)
	// Dedupe while preserving order so the first manifest's order wins.
	seen := map[string]struct{}{}
	out := deps[:0]
	for _, d := range deps {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// goRequireLine matches a single require line inside go.mod, with or
// without parentheses, picking up the module path before the version.
var goRequireLine = regexp.MustCompile(`^\s*([^\s/]+/[^\s]+|[^\s]+)\s+v[0-9]`)

func parseGoMod(file string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var (
		out     []string
		inBlock bool
	)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "require ("):
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case strings.HasPrefix(line, "require "):
			// single-line require: "require foo/bar v1.2.3"
			rest := strings.TrimSpace(strings.TrimPrefix(line, "require"))
			if mod := firstField(rest); mod != "" {
				out = append(out, mod)
			}
		case inBlock:
			if m := goRequireLine.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// firstField returns the first whitespace-delimited token of s.
func firstField(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}

func parsePackageJSON(file string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	names := make([]string, 0, len(pkg.Dependencies)+len(pkg.DevDependencies))
	for k := range pkg.Dependencies {
		names = append(names, k)
	}
	for k := range pkg.DevDependencies {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// testLayoutHints inspects the file list for common test-layout
// signatures and returns human-readable strings. The order is stable
// so RepoIntel JSON is deterministic.
func testLayoutHints(files []string) []string {
	var (
		hasGoTest   bool
		hasTestsDir bool
		hasJestDir  bool
		hasJsTestCo bool
		hasRSpec    bool
	)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			hasGoTest = true
		}
		if strings.HasPrefix(f, "tests/") || strings.Contains(f, "/tests/") {
			hasTestsDir = true
		}
		if strings.HasPrefix(f, "__tests__/") || strings.Contains(f, "/__tests__/") {
			hasJestDir = true
		}
		if strings.HasSuffix(f, ".test.ts") || strings.HasSuffix(f, ".test.tsx") ||
			strings.HasSuffix(f, ".test.js") || strings.HasSuffix(f, ".test.jsx") {
			hasJsTestCo = true
		}
		if strings.HasPrefix(f, "spec/") || strings.Contains(f, "/spec/") {
			hasRSpec = true
		}
	}
	var hints []string
	if hasGoTest {
		hints = append(hints, "Go tests co-located")
	}
	if hasTestsDir {
		hints = append(hints, "Python-style tests dir")
	}
	if hasJestDir {
		hints = append(hints, "Jest-style __tests__/")
	}
	if hasJsTestCo {
		hints = append(hints, "Co-located *.test.ts/js")
	}
	if hasRSpec {
		hints = append(hints, "Ruby-style spec/ dir")
	}
	return hints
}

// capStrings returns at most n entries of s. n <= 0 leaves s unchanged.
func capStrings(s []string, n int) []string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

// truncateToBudget shrinks the most expendable fields of intel until
// its JSON-serialized form fits intelByteBudget. File sample is
// halved first (it's the noisiest field), then dependencies. Other
// fields are never dropped because each shapes the prompt materially.
func truncateToBudget(intel *RepoIntel) {
	for !fits(intel) {
		switch {
		case len(intel.FileSample) > 0:
			intel.FileSample = halve(intel.FileSample)
		case len(intel.Dependencies) > 0:
			intel.Dependencies = halve(intel.Dependencies)
		default:
			return
		}
	}
}

func fits(intel *RepoIntel) bool {
	b, err := json.Marshal(intel)
	if err != nil {
		return true
	}
	return len(b) <= intelByteBudget
}

func halve(s []string) []string {
	if len(s) <= 1 {
		return nil
	}
	return s[:len(s)/2]
}
