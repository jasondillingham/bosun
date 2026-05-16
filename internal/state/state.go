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
	"strings"
	"sync"
	"time"

	"github.com/jasondillingham/bosun/internal/session"
)

const dirRelative = ".bosun/state"

// Store reads and writes session state markers under repoRoot/.bosun/state/.
//
// MarkDone/MarkStuck each do write-then-remove on the opposite marker;
// without serialization, an interleaving of two concurrent calls can clear
// both markers (leaving the session as WORKING when the operator
// expected DONE or STUCK). mu covers in-process callers; cross-process
// callers (e.g. a `bosun done` CLI invocation racing the MCP daemon's
// bosun_done tool) serialize via the POSIX flock on .bosun/state/.lock
// — see lock_unix.go for the full rationale.
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
	return withStateLock(s.dir(), func() error {
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
	return withStateLock(s.dir(), func() error {
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
	return withStateLock(s.dir(), func() error {
		for _, suffix := range []string{"done", "stuck"} {
			err := os.Remove(s.path(sessionName, suffix))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", s.path(sessionName, suffix), err)
			}
		}
		return nil
	})
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
