// Package state manages the DONE/STUCK marker files for bosun sessions
// under .bosun/state/<session>.{done,stuck}. Presence of a file is the
// signal; the body holds an optional message.
package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasondillingham/bosun/internal/lockfile"
	"github.com/jasondillingham/bosun/internal/phantom"
	"github.com/jasondillingham/bosun/internal/session"
)

const (
	dirRelative = ".bosun/state"
	// bosunDirRelative is the parent .bosun/ directory; the Spotlight
	// "do not index" marker lives at its root so the marker applies to
	// state/, claims/, rescues/, and everything else bosun writes here.
	bosunDirRelative = ".bosun"
	// spotlightMarkerName is the Apple-documented filename that tells
	// Spotlight to skip indexing the containing directory. Dropping it
	// at .bosun/.metadata_never_index stops macOS from creating the
	// duplicate state files (`session-1 2.done`, …) the LoadAll filter
	// has to fall back on.
	spotlightMarkerName = ".metadata_never_index"
)

// EnsureSpotlightMarker writes an empty `.bosun/.metadata_never_index`
// file under repoRoot if one isn't already there. macOS Spotlight,
// Time Machine, and iCloud Drive honor this marker and stop indexing
// the directory — eliminating the source of the duplicated `*N.done`
// files that phantom out as extra sessions otherwise. Existing repos
// still rely on the LoadAll filter for files Spotlight already created.
//
// Best-effort: a write failure is returned to the caller but is safe to
// log-and-continue; the LoadAll filter is the belt-and-suspenders for
// any duplicates that slip through.
func EnsureSpotlightMarker(repoRoot string) error {
	dir := filepath.Join(repoRoot, bosunDirRelative)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	marker := filepath.Join(dir, spotlightMarkerName)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", marker, err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return fmt.Errorf("write spotlight marker: %w", err)
	}
	return nil
}

// stateMarkerExts is the allow-list passed to phantom.IsLikelyPhantom
// when filtering enumeration of the state directory. Constrains the
// match so a hypothetical operator-supplied file named "Section 2.txt"
// dropped under .bosun/state/ wouldn't be silently ignored.
var stateMarkerExts = []string{"done", "stuck", "heartbeat", "json"}

// isPhantomStateFile reports whether name looks like a Finder/Spotlight/
// iCloud duplicate of a state marker. Thin wrapper over
// phantom.IsLikelyPhantom to keep call sites in this package readable.
func isPhantomStateFile(name string) bool {
	return phantom.IsLikelyPhantom(name, stateMarkerExts...)
}

// Store reads and writes session state markers under repoRoot/.bosun/state/.
//
// MarkDone/MarkStuck each do write-then-remove on the opposite marker;
// without serialization, an interleaving of two concurrent calls can clear
// both markers (leaving the session as WORKING when the operator
// expected DONE or STUCK). mu covers in-process callers; cross-process
// callers (e.g. a `bosun done` CLI invocation racing the MCP daemon's
// bosun_done tool) serialize via the POSIX flock on .bosun/state/.lock
// — see internal/lockfile for the shared implementation.
type Store struct {
	repoRoot string
	mu       sync.Mutex
}

func NewStore(repoRoot string) *Store { return &Store{repoRoot: repoRoot} }

// RepoRoot returns the repo root this store was constructed against.
// Callers that need to read other repo-scoped resources (e.g. config or
// the git worktree list) can use this to stay aligned with the store.
func (s *Store) RepoRoot() string { return s.repoRoot }

func (s *Store) dir() string { return filepath.Join(s.repoRoot, dirRelative) }

func (s *Store) path(sessionName string, suffix string) string {
	return filepath.Join(s.dir(), sessionName+"."+suffix)
}

// MarkDone writes a `.done` marker for sessionName with an optional message.
// Removes any prior `.stuck` marker on the same session.
//
// In-process callers serialize via s.mu; cross-process callers serialize
// via the flock on .bosun/state/.lock so a MarkDone interleaving a
// MarkStuck from another bosun process can't strip both markers.
func (s *Store) MarkDone(sessionName, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir(), err)
	}
	return lockfile.WithLock(filepath.Join(s.dir(), ".lock"), func() error {
		body := buildBody(message)
		if err := os.WriteFile(s.path(sessionName, "done"), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write done marker: %w", err)
		}
		_ = os.Remove(s.path(sessionName, "stuck"))
		return nil
	})
}

// MarkStuck writes a `.stuck` marker with an optional message. Removes any
// prior `.done` marker. Cross-process safe via the same flock MarkDone uses.
func (s *Store) MarkStuck(sessionName, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir(), err)
	}
	return lockfile.WithLock(filepath.Join(s.dir(), ".lock"), func() error {
		body := buildBody(message)
		if err := os.WriteFile(s.path(sessionName, "stuck"), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write stuck marker: %w", err)
		}
		_ = os.Remove(s.path(sessionName, "done"))
		return nil
	})
}

// Clear removes both done and stuck markers for sessionName. Missing is OK.
// Cross-process safe via the flock MarkDone/MarkStuck use — without it,
// Clear racing MarkDone could remove the marker MarkDone just wrote.
func (s *Store) Clear(sessionName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lockfile.WithLock(filepath.Join(s.dir(), ".lock"), func() error {
		for _, suffix := range []string{"done", "stuck"} {
			err := os.Remove(s.path(sessionName, suffix))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", s.path(sessionName, suffix), err)
			}
		}
		return nil
	})
}

// WriteHeartbeat records the current time as the session's latest heartbeat
// in .bosun/state/<sessionName>.heartbeat. The file body is the RFC3339-Nano
// timestamp; bosun status / Derive read it to decide whether to flag a
// session STALE.
//
// Heartbeats live under the same state directory as DONE/STUCK markers so
// they share the same cross-process flock (.bosun/state/.lock). Two
// concurrent bosun_heartbeat calls — common when an agent process and a
// detached MCP daemon race — therefore can't tear each other's writes.
func (s *Store) WriteHeartbeat(sessionName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir(), err)
	}
	return lockfile.WithLock(filepath.Join(s.dir(), ".lock"), func() error {
		body := time.Now().UTC().Format(time.RFC3339Nano) + "\n"
		if err := os.WriteFile(s.path(sessionName, "heartbeat"), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write heartbeat: %w", err)
		}
		return nil
	})
}

// Heartbeat returns the most recent heartbeat timestamp for sessionName.
// `exists` is false (and at is the zero time) when no heartbeat file is
// present. A malformed body returns an error so callers can surface it; a
// missing file is the silent "no heartbeat yet" path. Implements the
// session.StateReader interface.
func (s *Store) Heartbeat(repoRoot, sessionName string) (time.Time, bool, error) {
	store := s
	if repoRoot != s.repoRoot {
		store = NewStore(repoRoot)
	}
	body, ok, err := readIfExists(store.path(sessionName, "heartbeat"))
	if err != nil {
		return time.Time{}, false, err
	}
	if !ok {
		return time.Time{}, false, nil
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339Nano, body)
	if err != nil {
		// Fall back to RFC3339 (without nanos) for forward-compat with any
		// agent that records second-resolution timestamps.
		t, err = time.Parse(time.RFC3339, body)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse heartbeat %q: %w", body, err)
		}
	}
	return t, true, nil
}

// LoadAll enumerates the state directory and returns the sorted list of
// distinct session names that have at least one valid marker file
// (.done / .stuck / .heartbeat). Phantom duplicates created by
// Finder/Spotlight/Time Machine/iCloud (`session-1 2.done`,
// `session-1 (1).done`, …) are filtered out so callers don't surface
// them as additional sessions.
//
// Missing state dir → empty result, no error: a repo that hasn't run
// any session shouldn't be a hard error for callers iterating markers.
func (s *Store) LoadAll() ([]string, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state dir: %w", err)
	}
	seen := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isPhantomStateFile(name) {
			continue
		}
		ext := filepath.Ext(name)
		switch ext {
		case ".done", ".stuck", ".heartbeat", ".json":
		default:
			continue
		}
		base := strings.TrimSuffix(name, ext)
		if base == "" {
			continue
		}
		seen[base] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Read returns the session's current state plus the marker body.
// Returns (WORKING, "", nil) if no marker is present.
func (s *Store) Read(repoRoot, sessionName string) (session.State, string, error) {
	// Allow callers to pass repoRoot explicitly for interface symmetry.
	store := s
	if repoRoot != s.repoRoot {
		store = NewStore(repoRoot)
	}

	if body, ok, err := readIfExists(store.path(sessionName, "done")); err != nil {
		return session.StateWorking, "", err
	} else if ok {
		return session.StateDone, body, nil
	}
	if body, ok, err := readIfExists(store.path(sessionName, "stuck")); err != nil {
		return session.StateWorking, "", err
	} else if ok {
		return session.StateStuck, body, nil
	}
	return session.StateWorking, "", nil
}

func readIfExists(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimRight(string(data), "\n"), true, nil
}

func buildBody(message string) string {
	stamp := time.Now().UTC().Format(time.RFC3339)
	if message == "" {
		return stamp + "\n"
	}
	return stamp + "\n" + message + "\n"
}
