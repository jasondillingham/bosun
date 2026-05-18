// Package subtask manages the .bosun/subtasks/<parent>/<id>.json
// registry that backs the v1.0 bosun_subtask MCP tool.
//
// Sub-tasks are the lightweight counterpart to bosun_spawn: a parent
// agent registers a sub-task here so bosun can record + audit + display
// it, then runs the work itself (typically via Claude Code's Agent
// tool). There is no fork, no branch, no merge cycle — the registry is
// the load-bearing artifact. See docs/v1.0-sub-task-spec.md for the
// full motivation and the spawn-vs-subtask contrast table.
//
// The package is intentionally narrow: data access only. Auth gates,
// quota enforcement, and the MCP wiring live in internal/mcp. The
// package mirrors internal/spawntree's pattern so anyone who can
// reason about the spawn tree can reason about this one.
package subtask

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/lockfile"
)

const (
	// dirRelative is the root of the subtask registry under the repo.
	dirRelative = ".bosun/subtasks"

	// lockRelative is the POSIX flock companion file that serializes
	// writes to the registry as a whole. Same pattern internal/state,
	// internal/claims, and internal/spawntree use. One lock covers
	// every parent's subdir because the registry's read-modify-write
	// invariant — "count then create" — has to be atomic across parents
	// when a future feature ever needs cross-parent inspection. Single
	// lock keeps that future cheap; the contention cost today is
	// negligible since sub-task registration is microseconds.
	lockRelative = ".bosun/subtasks/.lock"

	// statusRunning is the only status the registry-only lane writes.
	// Future lanes (cancellation, completion) flip this to "completed"
	// or "cancelled"; quota counting treats anything != "running" as
	// not-counting-against-the-cap.
	statusRunning = "running"

	// idSuffixBytes governs the random portion of the subtask ID.
	// 6 bytes → 12 hex chars → 2^48 keyspace; the registry only needs
	// uniqueness per parent for the lifetime of the worktree, so this
	// is comfortably overkill without padding the IDs ergonomically.
	idSuffixBytes = 6
)

// Subtask is one registered sub-task. The JSON shape is the on-disk
// contract — see .bosun/subtasks/<parent>/<id>.json.
type Subtask struct {
	// ID is the synthetic label assigned at registration time. Format
	// is "<parent>.<12hex>" so the parent is recoverable from the ID
	// alone and the ID sorts alphabetically with its siblings.
	ID string `json:"id"`
	// Parent is the calling session's label (e.g. "session-3"). Stored
	// redundantly with the directory layout so a single record file is
	// self-describing for ad-hoc grep / jq / log shipping.
	Parent string `json:"parent"`
	// Description is the one-paragraph human-readable explanation of
	// what the sub-task should do. The MCP tool's Description arg
	// flows in here verbatim.
	Description string `json:"description"`
	// Files is the optional path scope the parent supplied. Sub-tasks
	// are *encouraged but not gated* to stay within these paths — the
	// registry records the intent so a future audit can flag drift.
	Files []string `json:"files,omitempty"`
	// Started is the RFC3339-UTC registration timestamp.
	Started string `json:"started"`
	// Status is "running" in the registry-only lane. Cancellation and
	// completion will write "cancelled" / "completed" in later lanes
	// without changing the schema.
	Status string `json:"status"`
}

// Store reads and writes the subtask registry under a repo root.
// Concurrent callers serialize through lockfile.WithLock on
// .bosun/subtasks/.lock so a "count then create" race between two
// MCP handlers can't smuggle a record past the max_concurrent cap.
type Store struct {
	repoRoot string
}

// NewStore returns a Store rooted at repoRoot. Doesn't touch disk.
func NewStore(repoRoot string) *Store { return &Store{repoRoot: repoRoot} }

// RepoRoot returns the repo root this store was constructed against.
func (s *Store) RepoRoot() string { return s.repoRoot }

func (s *Store) dir() string         { return filepath.Join(s.repoRoot, dirRelative) }
func (s *Store) lockPath() string    { return filepath.Join(s.repoRoot, lockRelative) }
func (s *Store) parentDir(p string) string {
	return filepath.Join(s.dir(), p)
}
func (s *Store) recordPath(parent, id string) string {
	return filepath.Join(s.parentDir(parent), id+".json")
}

// Create registers a new sub-task for parent under the registry. The
// quota check is the caller's job (internal/mcp's bosun_subtask gate
// reads CountActive before calling Create); Create itself is the
// atomic write-the-record half of that pair, serialized via flock so
// two concurrent registrations can't collide on the same generated ID.
//
// Returns the populated Subtask record (with ID and Started filled in)
// so the MCP tool can return it directly without re-reading the file.
func (s *Store) Create(parent, description string, files []string) (Subtask, error) {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return Subtask{}, errors.New("parent is required")
	}
	if strings.TrimSpace(description) == "" {
		return Subtask{}, errors.New("description is required")
	}

	var record Subtask
	err := lockfile.WithLock(s.lockPath(), func() error {
		if err := os.MkdirAll(s.parentDir(parent), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", s.parentDir(parent), err)
		}
		// Generate a fresh ID. Collisions inside the 2^48 keyspace
		// are vanishingly rare, but the loop guards against the
		// pathological case so a hostile RNG can't crash the
		// handler — at worst we burn a few iterations.
		var id string
		for attempt := 0; attempt < 8; attempt++ {
			suffix, err := randHex(idSuffixBytes)
			if err != nil {
				return fmt.Errorf("generate id: %w", err)
			}
			candidate := parent + "." + suffix
			if _, err := os.Stat(s.recordPath(parent, candidate)); errors.Is(err, fs.ErrNotExist) {
				id = candidate
				break
			} else if err != nil {
				return fmt.Errorf("stat %s: %w", s.recordPath(parent, candidate), err)
			}
		}
		if id == "" {
			return errors.New("could not generate unique subtask id after 8 attempts")
		}
		record = Subtask{
			ID:          id,
			Parent:      parent,
			Description: description,
			Files:       append([]string(nil), files...),
			Started:     time.Now().UTC().Format(time.RFC3339),
			Status:      statusRunning,
		}
		return writeRecord(s.recordPath(parent, id), record)
	})
	if err != nil {
		return Subtask{}, err
	}
	return record, nil
}

// CountActive returns the number of sub-tasks for parent that are
// still in "running" status. The MCP bosun_subtask gate consults this
// to enforce agent_subtask.max_concurrent — every record file is read
// rather than relying on directory cardinality so a future lane that
// keeps completed records around for observability doesn't double-count.
//
// A missing parent directory is not an error: parent has zero active
// sub-tasks. Same shape internal/state's LoadAll uses.
func (s *Store) CountActive(parent string) (int, error) {
	entries, err := os.ReadDir(s.parentDir(parent))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", s.parentDir(parent), err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, err := readRecord(filepath.Join(s.parentDir(parent), e.Name()))
		if err != nil {
			// A malformed record file shouldn't block quota checks
			// (the registry is observability; the quota is a soft
			// guard rail). Skip and move on; the operator notices
			// when the count diverges from `bosun show`.
			continue
		}
		if rec.Status == statusRunning {
			count++
		}
	}
	return count, nil
}

// ListActive returns the running sub-tasks for parent, sorted by ID.
// Used by `bosun show <session>` (when that wiring lands) and by the
// test suite to assert the registry's shape after each call. A missing
// parent directory returns nil with no error — same convention
// CountActive uses.
func (s *Store) ListActive(parent string) ([]Subtask, error) {
	entries, err := os.ReadDir(s.parentDir(parent))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.parentDir(parent), err)
	}
	var out []Subtask
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, err := readRecord(filepath.Join(s.parentDir(parent), e.Name()))
		if err != nil {
			continue
		}
		if rec.Status == statusRunning {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// writeRecord persists a single subtask JSON record atomically via the
// write-tmp-then-rename pattern (matches internal/spawntree.writeLocked).
// Caller must hold the flock — invoked only inside lockfile.WithLock
// callbacks above.
func writeRecord(path string, rec Subtask) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal subtask: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// readRecord loads a single subtask record. Returned errors propagate
// to the caller; the count/list paths swallow them so malformed files
// don't break the broader call. ReadRecord stays strict so direct
// callers (and tests) can assert on the corruption shape.
func readRecord(path string) (Subtask, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Subtask{}, fmt.Errorf("read %s: %w", path, err)
	}
	var rec Subtask
	if err := json.Unmarshal(data, &rec); err != nil {
		return Subtask{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return rec, nil
}

// randHex returns 2*n hex characters drawn from crypto/rand. Bubbled
// out so tests can swap it for a deterministic source if a future lane
// needs reproducible IDs; today's lane lets crypto/rand do its job.
func randHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
