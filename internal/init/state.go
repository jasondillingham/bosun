// Package initstate persists the in-progress state of a `bosun init` run
// to `.bosun/init.state` so a partially-failed init can be resumed cleanly.
//
// Why this exists: v0.5 dogfood surfaced a foot-gun where `bosun init`
// would create some branches and worktrees, then fail mid-loop (Spotlight
// pressure, a wedged `git worktree add`, an operator-killed run). On
// re-invoke, the operator would either hit "worktree path already
// exists" errors or, worse, not notice the half-completion at all and
// build on top of an inconsistent layout. The state file lets us refuse
// to plough on top of a half-finished run, and gives `--resume` enough
// information to continue exactly where the previous attempt stopped.
//
// The file is intentionally small and JSON: easy to read by hand, easy
// to delete if an operator wants a forced reset, and easy to roundtrip
// in tests. Cross-process writers serialize via a POSIX flock on
// `.bosun/init.lock` — the same pattern internal/state uses — so a
// concurrent `bosun init` and an MCP-side helper can't tear the file.
package initstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jasondillingham/bosun/internal/lockfile"
)

// Step labels for the per-session pipeline. Stored in InitState.CurrentStep
// so `--resume` knows where the last attempt stopped within the current
// session. Hook firing is repo-level (after every session) so it gets its
// own step rather than living under a session.
const (
	StepBranchCreate   = "branch_create"
	StepGitWorktreeAdd = "git_worktree_add"
	StepStateFileWrite = "state_file_write"
	StepHookPostInit   = "hook_post_init"
)

const (
	dirRelative  = ".bosun"
	fileRelative = ".bosun/init.state"
	lockRelative = ".bosun/init.lock"

	// stateVersion is bumped when the on-disk schema changes in a way that
	// older binaries can't safely consume. v0.6 introduces v0.6; later
	// rounds may have to migrate or refuse to resume across versions.
	stateVersion = "v0.6"
)

// InitState is the on-disk shape persisted under `.bosun/init.state`.
//
// Field ordering matches the human-readable JSON we want operators to be
// able to skim: when something is, what's done, what's next.
type InitState struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	// RoundTimestamp is the UTC `YYYYMMDD-HHMMSS` token captured at init
	// invocation. It's substituted into the worktree suffix pattern so
	// each round's on-disk dirs are unique (scheme C from
	// docs/uid-worktree-design.md). Persisted here so `--resume` after a
	// partial init reproduces the exact same paths — re-deriving from
	// time.Now() on resume would create a fresh second worktree alongside
	// the half-finished one. Empty for state files written by pre-UID
	// versions of bosun (legacy paths still resolve fine in that case).
	RoundTimestamp    string   `json:"round_timestamp,omitempty"`
	PlanPath          string   `json:"plan_path,omitempty"`
	TotalSessions     int      `json:"total_sessions"`
	Labels            []string `json:"labels,omitempty"`
	CompletedSessions []string `json:"completed_sessions"`
	CurrentSession    string   `json:"current_session,omitempty"`
	CurrentStep       string   `json:"current_step,omitempty"`

	// mu protects in-process concurrent mutations. Cross-process serialization
	// is via the flock in writeLocked — the two cover different races.
	mu sync.Mutex `json:"-"`
}

// New returns a freshly-stamped InitState ready to be Save'd. It does not
// touch the filesystem; the caller decides when to persist.
//
// roundTimestamp is the `YYYYMMDD-HHMMSS` UTC token the caller captured
// at init invocation; persisting it here is what lets `--resume`
// reproduce the exact same on-disk worktree paths. Empty roundTimestamp
// is accepted for callers that don't yet thread the value (tests, older
// code paths) — those produce legacy paths.
func New(labels []string, planPath, roundTimestamp string) *InitState {
	stored := append([]string(nil), labels...)
	return &InitState{
		Version:           stateVersion,
		StartedAt:         time.Now().UTC(),
		RoundTimestamp:    roundTimestamp,
		PlanPath:          planPath,
		TotalSessions:     len(labels),
		Labels:            stored,
		CompletedSessions: []string{},
	}
}

// SessionLabels returns the full ordered list of session labels this init
// was created with. Newer state files store labels explicitly; older or
// hand-edited ones may only carry TotalSessions, so we fall back to the
// numbered form (`session-1..session-N`) — the only label scheme bosun
// generates without an explicit list — so `--resume` can still recover.
func (s *InitState) SessionLabels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Labels) > 0 {
		out := make([]string, len(s.Labels))
		copy(out, s.Labels)
		return out
	}
	out := make([]string, 0, s.TotalSessions)
	for i := 1; i <= s.TotalSessions; i++ {
		out = append(out, fmt.Sprintf("session-%d", i))
	}
	return out
}

// Load reads `.bosun/init.state` from repoRoot. Returns (nil, fs.ErrNotExist)
// when the file is absent so callers can branch on errors.Is(err, fs.ErrNotExist)
// without inspecting filesystem layout. Any other parse/IO error surfaces
// verbatim — a corrupt state file is a hard signal to the operator that
// something is off; bosun should refuse to guess.
func Load(repoRoot string) (*InitState, error) {
	path := filepath.Join(repoRoot, fileRelative)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s InitState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

// Exists is a cheap stat that says whether init.state is present without
// reading or parsing it. Used by `bosun init` (no --resume) to refuse to
// run on top of a half-finished prior attempt.
func Exists(repoRoot string) bool {
	_, err := os.Stat(filepath.Join(repoRoot, fileRelative))
	return err == nil
}

// Path returns the absolute path to the state file under repoRoot. Exposed
// so callers (and tests) can reference the file in error messages without
// re-deriving the path.
func Path(repoRoot string) string {
	return filepath.Join(repoRoot, fileRelative)
}

// Save writes the current state to `.bosun/init.state` under repoRoot.
// Atomic via write-then-rename so a crash mid-save can't leave a
// truncated/half-written file readable by a future Load.
func (s *InitState) Save(repoRoot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(repoRoot)
}

// MarkComplete records label in CompletedSessions, clears CurrentSession /
// CurrentStep if they match, and persists. Idempotent: calling it twice
// with the same label is a no-op past the first call.
func (s *InitState) MarkComplete(repoRoot, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.CompletedSessions {
		if c == label {
			// Already recorded; still persist in case CurrentSession was
			// stale or callers rely on the file being fresh-on-disk.
			if s.CurrentSession == label {
				s.CurrentSession = ""
				s.CurrentStep = ""
			}
			return s.writeLocked(repoRoot)
		}
	}
	s.CompletedSessions = append(s.CompletedSessions, label)
	if s.CurrentSession == label {
		s.CurrentSession = ""
		s.CurrentStep = ""
	}
	return s.writeLocked(repoRoot)
}

// SetCurrent records that we are about to perform step on label and
// persists. The combination of (CurrentSession, CurrentStep) is what
// `--resume` uses to decide whether to re-run the step.
func (s *InitState) SetCurrent(repoRoot, label, step string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CurrentSession = label
	s.CurrentStep = step
	return s.writeLocked(repoRoot)
}

// IsCompleted reports whether label has been marked complete.
func (s *InitState) IsCompleted(label string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.CompletedSessions {
		if c == label {
			return true
		}
	}
	return false
}

// Clear removes `.bosun/init.state` from repoRoot. Called on successful
// completion of an init run so the next plain `bosun init` is not refused.
// Missing file is not an error — Clear is also called from defensive
// cleanup paths where the file may already be gone.
func (s *InitState) Clear(repoRoot string) error {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	return clearFile(repoRoot)
}

// ClearFile is a package-level form of Clear for callers that have no
// InitState in hand (e.g. operator-driven recovery: read state, decide
// to discard, no need to construct a struct).
func ClearFile(repoRoot string) error {
	return clearFile(repoRoot)
}

func clearFile(repoRoot string) error {
	path := filepath.Join(repoRoot, fileRelative)
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// writeLocked persists the state to disk under the cross-process flock.
// Caller must hold s.mu — the flock covers concurrent processes; the
// mutex covers concurrent goroutines in this process.
func (s *InitState) writeLocked(repoRoot string) error {
	if err := os.MkdirAll(filepath.Join(repoRoot, dirRelative), 0o750); err != nil {
		return fmt.Errorf("mkdir .bosun: %w", err)
	}
	return lockfile.WithLock(filepath.Join(repoRoot, lockRelative), func() error {
		data, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal init state: %w", err)
		}
		data = append(data, '\n')
		path := filepath.Join(repoRoot, fileRelative)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
		}
		return nil
	})
}
