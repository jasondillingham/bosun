// Package history archives a session's metadata under .bosun/history/
// just before cleanup/remove/merge would otherwise wipe it. The archive
// gives operators a way to grep "what did session-2 do last week" after
// the worktree, branch, claims, and brief are all gone.
//
// Archiving is best-effort: callers in cleanup/remove/merge log failures
// and continue so a permission-denied here can't block the load-bearing
// git side of those commands.
package history

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// dirRelative is the on-disk root, relative to the bosun repo root.
const dirRelative = ".bosun/history"

// timestampLayout is the compact ISO8601 form used for archive directory
// prefixes — mirrors the format cmd_rescue.go / cmd_remove.go already use
// for human-readable on-disk timestamps. Length is exactly 16 chars.
const timestampLayout = "20060102T150405Z"

// timestampLen is the rune-count of a timestampLayout-formatted string.
// Used by parseDirName to split <ts>-<label>.
const timestampLen = 16

// End-reason constants — written verbatim into metadata.json.
const (
	ReasonMerged  = "merged"
	ReasonRemoved = "removed"
	ReasonCleanup = "cleanup"
	ReasonPurged  = "purged"
)

// Metadata is the JSON-serialized contents of metadata.json.
type Metadata struct {
	Label     string    `json:"label"`
	Branch    string    `json:"branch,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at"`
	EndReason string    `json:"end_reason"`
	MergeSHA  string    `json:"merge_sha,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// ArchiveInput gathers everything Archive() can capture. Fields are all
// optional except RepoRoot, Label, and EndReason.
type ArchiveInput struct {
	RepoRoot     string
	Label        string
	Branch       string
	WorktreePath string
	EndReason    string
	MergeSHA     string
	Detail       string
	// Now overrides the archive timestamp — for tests. Zero means time.Now().UTC().
	Now time.Time
	// CommitsLog overrides the captured `git log --oneline <branch>` output —
	// for tests, or for callers that already have a log string in hand.
	CommitsLog string
}

// Archive writes one archive directory under .bosun/history/. Returns the
// absolute path of the written directory and a (possibly nil) error
// describing per-file failures. The metadata.json write is the
// load-bearing step — if that succeeds, err is nil even if individual
// brief/claims/log copies were absent. Missing source files are not an
// error.
func Archive(ctx context.Context, in ArchiveInput) (string, error) {
	if in.RepoRoot == "" {
		return "", errors.New("history: repo root required")
	}
	if in.Label == "" {
		return "", errors.New("history: label required")
	}
	if in.EndReason == "" {
		return "", errors.New("history: end reason required")
	}

	ts := in.Now
	if ts.IsZero() {
		ts = time.Now().UTC()
	} else {
		ts = ts.UTC()
	}

	dirName := ts.Format(timestampLayout) + "-" + in.Label
	archDir := filepath.Join(in.RepoRoot, dirRelative, dirName)
	if err := os.MkdirAll(archDir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir history dir: %w", err)
	}

	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	// brief.md — copied from the worktree if it still exists. Bosun
	// writes BOSUN_BRIEF.md to the worktree root at session creation.
	if in.WorktreePath != "" {
		src := filepath.Join(in.WorktreePath, "BOSUN_BRIEF.md")
		if err := copyFileIfExists(src, filepath.Join(archDir, "brief.md")); err != nil {
			warn("brief.md: %v", err)
		}
	}

	// claims.json — copied from .bosun/claims/<label>.json.
	claimsSrc := filepath.Join(in.RepoRoot, ".bosun", "claims", in.Label+".json")
	if err := copyFileIfExists(claimsSrc, filepath.Join(archDir, "claims.json")); err != nil {
		warn("claims.json: %v", err)
	}

	// commits.log — either an explicit override (used by merge to
	// snapshot before the branch is deleted) or a fresh `git log`.
	commits := in.CommitsLog
	if commits == "" && in.Branch != "" {
		out, err := gitLogOneline(ctx, in.RepoRoot, in.Branch)
		if err != nil {
			warn("commits.log: %v", err)
		}
		commits = out
	}
	if commits != "" {
		if err := os.WriteFile(filepath.Join(archDir, "commits.log"), []byte(commits), 0o600); err != nil {
			warn("commits.log: %v", err)
		}
	}

	// merged.txt — only present for the merge path.
	if in.MergeSHA != "" {
		body := in.MergeSHA
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := os.WriteFile(filepath.Join(archDir, "merged.txt"), []byte(body), 0o600); err != nil {
			warn("merged.txt: %v", err)
		}
	}

	// Try to learn the session start time from the claims file's
	// updated_at field — it's a reasonable proxy for "when this
	// session was active". Best-effort: missing or unparseable is OK.
	started := readClaimsUpdatedAt(claimsSrc)

	meta := Metadata{
		Label:     in.Label,
		Branch:    in.Branch,
		StartedAt: started,
		EndedAt:   ts,
		EndReason: in.EndReason,
		MergeSHA:  in.MergeSHA,
		Detail:    in.Detail,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return archDir, fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "metadata.json"), data, 0o600); err != nil {
		return archDir, fmt.Errorf("write metadata.json: %w", err)
	}

	if len(warnings) > 0 {
		return archDir, fmt.Errorf("partial archive: %s", strings.Join(warnings, "; "))
	}
	return archDir, nil
}

// Entry is one archive directory.
type Entry struct {
	DirName   string
	Path      string
	Timestamp time.Time
	Label     string
	Metadata  *Metadata
}

// List returns every archive under .bosun/history/, newest first.
// A missing history directory returns (nil, nil) — there are no archives
// yet, not an error.
func List(repoRoot string) ([]Entry, error) {
	root := filepath.Join(repoRoot, dirRelative)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read history dir: %w", err)
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ts, label, ok := parseDirName(e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(root, e.Name())
		entry := Entry{
			DirName:   e.Name(),
			Path:      path,
			Timestamp: ts,
			Label:     label,
		}
		entry.Metadata, _ = readMetadata(path)
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		// Newest first; tie-break by directory name for stable ordering.
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.After(out[j].Timestamp)
		}
		return out[i].DirName < out[j].DirName
	})
	return out, nil
}

// Lookup resolves identifier to a single archive entry. identifier can be:
//   - the full <timestamp>-<label> directory name
//   - a bare label, in which case the most recent matching entry wins
//   - a timestamp prefix, e.g. "20260518T", in which case a unique match wins
//
// Returns (nil, candidates, nil) if multiple non-newest-label matches exist
// so the caller can list them in an error message. Returns
// (nil, nil, fs.ErrNotExist) when there is no match at all.
func Lookup(repoRoot, identifier string) (*Entry, []string, error) {
	entries, err := List(repoRoot)
	if err != nil {
		return nil, nil, err
	}
	if identifier == "" {
		return nil, nil, errors.New("history: identifier required")
	}
	// Exact dir name match.
	for i := range entries {
		if entries[i].DirName == identifier {
			return &entries[i], nil, nil
		}
	}
	// Label match — newest wins (entries are sorted newest first).
	for i := range entries {
		if entries[i].Label == identifier {
			return &entries[i], nil, nil
		}
	}
	// Prefix match against directory name.
	var hits []int
	for i := range entries {
		if strings.HasPrefix(entries[i].DirName, identifier) {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil, fs.ErrNotExist
	case 1:
		return &entries[hits[0]], nil, nil
	default:
		names := make([]string, 0, len(hits))
		for _, i := range hits {
			names = append(names, entries[i].DirName)
		}
		return nil, names, fmt.Errorf("ambiguous identifier %q matches %d archives", identifier, len(hits))
	}
}

// GrepHit is one ripgrep-shaped match: archive + file + line + text.
type GrepHit struct {
	DirName string
	File    string
	Line    int
	Text    string
}

// Grep searches every archive's textual contents (brief.md, commits.log,
// merged.txt, metadata.json) for pattern. It shells out to ripgrep when
// `rg` is on PATH (faster on large archive sets); otherwise falls back to
// a Go regexp scan that uses the same regexp dialect as `rg --no-pcre2`.
//
// Pattern is a Go-flavored regular expression. An empty pattern returns
// no hits and no error.
func Grep(ctx context.Context, repoRoot, pattern string) ([]GrepHit, error) {
	if pattern == "" {
		return nil, nil
	}
	root := filepath.Join(repoRoot, dirRelative)
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if _, err := exec.LookPath("rg"); err == nil {
		hits, rerr := grepRipgrep(ctx, root, pattern)
		if rerr == nil {
			return hits, nil
		}
		// Fall through to Go scan on ripgrep failure — bad regex
		// surface or missing files shouldn't kill the whole command.
	}
	return grepGo(root, pattern)
}

// Prune deletes every archive whose directory mtime is older than
// (now - olderThan). Returns the names of the deleted archives. A
// zero or negative olderThan returns (nil, error) — prune never deletes
// everything implicitly.
func Prune(repoRoot string, olderThan time.Duration) ([]string, error) {
	if olderThan <= 0 {
		return nil, errors.New("history: prune duration must be positive")
	}
	cutoff := time.Now().Add(-olderThan)
	root := filepath.Join(repoRoot, dirRelative)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read history dir: %w", err)
	}
	var deleted []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, _, ok := parseDirName(e.Name()); !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(root, e.Name())
			if rerr := os.RemoveAll(path); rerr != nil {
				return deleted, fmt.Errorf("remove %s: %w", path, rerr)
			}
			deleted = append(deleted, e.Name())
		}
	}
	sort.Strings(deleted)
	return deleted, nil
}

// ParseDuration accepts the friendly suffixes the CLI takes for
// --older-than: e.g. "30d", "12h", "2w". Falls back to time.ParseDuration
// for anything time itself understands (so "30m" still works).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	// Find the trailing unit. We accept multi-digit numbers followed by
	// one of d/D/w/W; anything else delegates to time.ParseDuration.
	unit := s[len(s)-1]
	switch unit {
	case 'd', 'D':
		n, err := parseLeadingInt(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("parse days: %w", err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	case 'w', 'W':
		n, err := parseLeadingInt(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("parse weeks: %w", err)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func parseLeadingInt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty number")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

// --- internals ---

func parseDirName(name string) (time.Time, string, bool) {
	if len(name) < timestampLen+2 || name[timestampLen] != '-' {
		return time.Time{}, "", false
	}
	tsPart := name[:timestampLen]
	label := name[timestampLen+1:]
	ts, err := time.ParseInLocation(timestampLayout, tsPart, time.UTC)
	if err != nil {
		return time.Time{}, "", false
	}
	if label == "" {
		return time.Time{}, "", false
	}
	return ts, label, true
}

func readMetadata(archDir string) (*Metadata, error) {
	data, err := os.ReadFile(filepath.Join(archDir, "metadata.json"))
	if err != nil {
		return nil, err
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func copyFileIfExists(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()
	// 0o600 — history archives may contain operator-visible brief
	// contents; on multi-user hosts group/world-readable defaults
	// would leak.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func gitLogOneline(ctx context.Context, repoRoot, branch string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git binary not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "--oneline", branch)
	out, err := cmd.Output()
	if err != nil {
		// A missing branch (already deleted) is the common case during
		// merge archival — surface a friendly message so the warning
		// is actionable rather than scary.
		return "", fmt.Errorf("git log %s: %w", branch, err)
	}
	return string(out), nil
}

func readClaimsUpdatedAt(path string) time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	// We only need updated_at; decode into a permissive shape so a
	// schema change in claims doesn't break archival.
	var probe struct {
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return time.Time{}
	}
	return probe.UpdatedAt
}

func grepRipgrep(ctx context.Context, root, pattern string) ([]GrepHit, error) {
	args := []string{
		"--no-heading",
		"-n",            // include line numbers
		"--color=never", // safe for parsing
		"-e", pattern,
		root,
	}
	cmd := exec.CommandContext(ctx, "rg", args...) //nolint:gosec // G204: bosun searches user's own repo with a fixed binary and locally-derived args
	out, err := cmd.Output()
	if err != nil {
		// rg exits 1 when there are no matches; that's not a real error.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	return parseRipgrepOutput(root, string(out)), nil
}

// parseRipgrepOutput parses `rg --no-heading -n` lines into GrepHit values.
// Format: <path>:<line>:<text>. On Windows the path may contain a colon
// after the drive letter, so we split from the right after the path.
func parseRipgrepOutput(root, output string) []GrepHit {
	var hits []GrepHit
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		hit, ok := parseGrepLine(root, line)
		if !ok {
			continue
		}
		hits = append(hits, hit)
	}
	return hits
}

// parseGrepLine splits a `<path>:<line>:<text>` row. Returns ok=false when
// the row doesn't have the expected shape (e.g. ripgrep printed a
// permission-denied warning).
func parseGrepLine(root, line string) (GrepHit, bool) {
	// Find "path:lineno:" by walking from the end of the path prefix.
	// We need at least two colons to the right of the absolute path.
	rest := line
	// Strip an absolute-path prefix when present (works for the
	// ripgrep-emits-absolute case from `rg ... <root>`).
	if strings.HasPrefix(rest, root) {
		rest = strings.TrimPrefix(rest, root)
		rest = strings.TrimPrefix(rest, string(filepath.Separator))
	}
	// Now rest should be `<rel-path>:<lineno>:<text>` or
	// `<file-name>:<lineno>:<text>`. Find the first non-Windows-drive colon.
	i1 := strings.IndexByte(rest, ':')
	if i1 < 0 {
		return GrepHit{}, false
	}
	i2 := strings.IndexByte(rest[i1+1:], ':')
	if i2 < 0 {
		return GrepHit{}, false
	}
	pathPart := rest[:i1]
	linePart := rest[i1+1 : i1+1+i2]
	textPart := rest[i1+1+i2+1:]
	ln, err := parseLeadingInt(linePart)
	if err != nil {
		return GrepHit{}, false
	}
	dir, file := splitArchPath(pathPart)
	if dir == "" {
		return GrepHit{}, false
	}
	return GrepHit{DirName: dir, File: file, Line: int(ln), Text: textPart}, true
}

// splitArchPath splits "<archive-dir>/<file>" (with either OS separator).
func splitArchPath(p string) (string, string) {
	p = filepath.ToSlash(p)
	parts := strings.SplitN(p, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func grepGo(root, pattern string) ([]GrepHit, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}
	var hits []GrepHit
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, de := range dirEntries {
		if !de.IsDir() {
			continue
		}
		if _, _, ok := parseDirName(de.Name()); !ok {
			continue
		}
		archDir := filepath.Join(root, de.Name())
		files, err := os.ReadDir(archDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			full := filepath.Join(archDir, f.Name())
			data, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			if !isProbablyText(data) {
				continue
			}
			scanner := bufio.NewScanner(strings.NewReader(string(data)))
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			ln := 0
			for scanner.Scan() {
				ln++
				line := scanner.Text()
				if re.MatchString(line) {
					hits = append(hits, GrepHit{
						DirName: de.Name(),
						File:    f.Name(),
						Line:    ln,
						Text:    line,
					})
				}
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].DirName != hits[j].DirName {
			return hits[i].DirName < hits[j].DirName
		}
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	return hits, nil
}

// isProbablyText returns false when the first chunk of data looks
// binary (any NUL byte). Keeps Grep from emitting nonsense lines from
// future binary artifacts under an archive dir.
func isProbablyText(data []byte) bool {
	n := len(data)
	if n > 4096 {
		n = 4096
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return false
		}
	}
	return true
}
