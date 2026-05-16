// Package claims manages the advisory per-session path claim files under
// .bosun/claims/<session>.json. Claims do not enforce anything — they
// surface in `bosun status --with-overlaps` so the operator can intervene
// when two sessions land on overlapping paths.
package claims

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const dirRelative = ".bosun/claims"

// Claim is one persisted claim file.
type Claim struct {
	Session   string    `json:"session"`
	Paths     []string  `json:"paths"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store reads and writes claim files under repoRoot/.bosun/claims/.
type Store struct {
	repoRoot string
}

// NewStore returns a Store rooted at repoRoot.
func NewStore(repoRoot string) *Store { return &Store{repoRoot: repoRoot} }

func (s *Store) dir() string  { return filepath.Join(s.repoRoot, dirRelative) }
func (s *Store) file(session string) string {
	return filepath.Join(s.dir(), session+".json")
}

// Add merges the given paths into session's claim file, deduplicating.
// Creates the file (and parent dir) if needed.
func (s *Store) Add(session string, paths []string) error {
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir(), err)
	}
	existing, err := s.Read(session)
	if err != nil {
		return err
	}
	if existing == nil {
		existing = &Claim{Session: session}
	}
	merged := dedupe(append(existing.Paths, normalizeAll(paths)...))
	sort.Strings(merged)
	existing.Paths = merged
	existing.UpdatedAt = time.Now().UTC()
	return s.write(existing)
}

// Replace overwrites session's claim file with exactly these paths.
func (s *Store) Replace(session string, paths []string) error {
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir(), err)
	}
	merged := dedupe(normalizeAll(paths))
	sort.Strings(merged)
	c := &Claim{Session: session, Paths: merged, UpdatedAt: time.Now().UTC()}
	return s.write(c)
}

// Remove drops the given paths from session's claim file. Paths not currently
// claimed are silently ignored. If the resulting claim has no paths left, the
// file is removed (mirrors Clear). A missing claim file is not an error.
// Returns the number of paths actually removed.
func (s *Store) Remove(session string, paths []string) (int, error) {
	existing, err := s.Read(session)
	if err != nil {
		return 0, err
	}
	if existing == nil || len(existing.Paths) == 0 {
		return 0, nil
	}
	drop := map[string]struct{}{}
	for _, p := range normalizeAll(paths) {
		drop[p] = struct{}{}
	}
	kept := make([]string, 0, len(existing.Paths))
	removed := 0
	for _, p := range existing.Paths {
		if _, ok := drop[p]; ok {
			removed++
			continue
		}
		kept = append(kept, p)
	}
	if removed == 0 {
		return 0, nil
	}
	if len(kept) == 0 {
		if err := s.Clear(session); err != nil {
			return 0, err
		}
		return removed, nil
	}
	sort.Strings(kept)
	existing.Paths = kept
	existing.UpdatedAt = time.Now().UTC()
	if err := s.write(existing); err != nil {
		return 0, err
	}
	return removed, nil
}

// Clear removes session's claim file. Missing is not an error.
func (s *Store) Clear(session string) error {
	err := os.Remove(s.file(session))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", s.file(session), err)
	}
	return nil
}

// Read returns session's claim, or nil if there is no file. Returns an error
// if the file exists but cannot be parsed.
func (s *Store) Read(session string) (*Claim, error) {
	data, err := os.ReadFile(s.file(session))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.file(session), err)
	}
	var c Claim
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.file(session), err)
	}
	return &c, nil
}

// CountFor returns the number of distinct paths claimed by session. Missing => 0.
func (s *Store) CountFor(repoRoot, session string) (int, error) {
	if repoRoot != s.repoRoot {
		// Allow callers to pass repoRoot explicitly for interface symmetry,
		// but in practice we always use s.repoRoot. Tolerate mismatch by
		// using the explicit one (cheap to do).
		tmp := NewStore(repoRoot)
		return tmp.CountFor(repoRoot, session)
	}
	c, err := s.Read(session)
	if err != nil {
		return 0, err
	}
	if c == nil {
		return 0, nil
	}
	return len(c.Paths), nil
}

// All returns every claim file in the store, keyed by session name.
func (s *Store) All() (map[string]*Claim, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]*Claim{}, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", s.dir(), err)
	}
	out := map[string]*Claim{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		session := strings.TrimSuffix(e.Name(), ".json")
		c, err := s.Read(session)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out[session] = c
		}
	}
	return out, nil
}

// Overlap is one row in an overlap report.
type Overlap struct {
	Path     string
	Sessions []string
}

// Overlaps returns the set of paths claimed by more than one session.
// Glob patterns are not expanded — overlap is detected by literal path
// equality and by glob-vs-glob/file matching via filepath.Match.
func (s *Store) Overlaps() ([]Overlap, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	// Build path -> sessions[] map, including expanding each session's
	// claims against every other session's claims via Match.
	hits := map[string]map[string]struct{}{}
	addHit := func(p, sess string) {
		if _, ok := hits[p]; !ok {
			hits[p] = map[string]struct{}{}
		}
		hits[p][sess] = struct{}{}
	}
	for sessA, ca := range all {
		for _, pa := range ca.Paths {
			addHit(pa, sessA)
			for sessB, cb := range all {
				if sessA == sessB {
					continue
				}
				for _, pb := range cb.Paths {
					if matches(pa, pb) {
						addHit(pa, sessB)
					}
				}
			}
		}
	}
	var result []Overlap
	for p, set := range hits {
		if len(set) < 2 {
			continue
		}
		sessions := make([]string, 0, len(set))
		for s := range set {
			sessions = append(sessions, s)
		}
		sort.Strings(sessions)
		result = append(result, Overlap{Path: p, Sessions: sessions})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func (s *Store) write(c *Claim) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claim: %w", err)
	}
	return os.WriteFile(s.file(c.Session), data, 0o644)
}

func normalizeAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Use forward slashes for portability across OSes — claims live in
		// a JSON file that may be consumed on a different platform than the
		// one it was written on.
		p = strings.ReplaceAll(p, `\`, "/")
		p = strings.TrimPrefix(p, "./")
		out = append(out, p)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// matches reports whether pa and pb overlap. It is symmetric and handles
// the common cases: equality, directory prefix containment, and glob match.
func matches(pa, pb string) bool {
	if pa == pb {
		return true
	}
	// Directory containment: "internal/auth/" or "internal/auth" covers "internal/auth/foo.go".
	if isPrefixDir(pa, pb) || isPrefixDir(pb, pa) {
		return true
	}
	// Glob match either direction.
	if ok, _ := path.Match(pa, pb); ok {
		return true
	}
	if ok, _ := path.Match(pb, pa); ok {
		return true
	}
	return false
}

func isPrefixDir(prefix, p string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return false
	}
	return strings.HasPrefix(p, prefix+"/")
}
