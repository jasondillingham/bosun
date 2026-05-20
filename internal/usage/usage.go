// Package usage records per-session LLM token + cost usage as an
// append-only ledger at .bosun/state/<sessionName>.usage. Each agent
// turn appends one JSON-encoded line; readers sum the lines to
// produce session totals.
//
// Why append-only vs a single rolling totals file:
//
//   - Auditability. The ledger preserves every turn's cost so an
//     operator can answer "what did session-2 spend its budget on?"
//     without needing to retain conversation transcripts.
//   - Concurrency safety. Multiple agent processes (parent + spawned
//     sub-tasks, or a flaky agent that double-emits) can call the
//     bosun_usage MCP tool concurrently. POSIX guarantees atomic
//     append-mode writes under PIPE_BUF (512 bytes on macOS, 4096
//     on Linux); a single JSON entry is well under that.
//   - Recovery. Partial state from a crashed turn is at most one
//     line of corrupt JSON, which Read silently drops.
package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one recorded turn's usage. Marshalled to a single JSON
// line for the append-only ledger; TurnLabel + Model are free-form
// strings the operator's agent wrapper sets.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	CostUSD   float64   `json:"cost_usd"`
	Model     string    `json:"model,omitempty"`
	// TurnLabel is an optional human-readable tag the agent wrapper
	// can attach (e.g. "design-phase", "implementation", "test-fix").
	// Empty when not set. Useful for breaking down cost by phase.
	TurnLabel string `json:"turn_label,omitempty"`
}

// Totals is the summed view of all entries for one session.
type Totals struct {
	TokensIn  int
	TokensOut int
	CostUSD   float64
	TurnCount int
	// LastModel is the model from the most recent entry. Renderers
	// use it to hint at "what's currently running" without listing
	// every model used historically.
	LastModel string
	// LastAt is the timestamp of the most recent entry. Zero time
	// when no entries exist. Used by status to decide whether usage
	// is stale.
	LastAt time.Time
}

// stateDir is the relative path under repoRoot where session-state
// files live. Kept here rather than imported from internal/state so
// the usage package has no cross-dep on the state store (which would
// need to grow yet another method to expose the path).
const stateDirRel = ".bosun/state"

// usagePath returns the absolute path of the ledger file for
// sessionName under repoRoot.
func usagePath(repoRoot, sessionName string) string {
	return filepath.Join(repoRoot, stateDirRel, sessionName+".usage")
}

// ListSessions returns the session labels that have a usage ledger
// in repoRoot, sorted alphabetically. Missing state directory or
// no .usage files yields an empty slice — not an error.
//
// Used by `bosun cost` to roll up across every session bosun has
// tracked, without having to derive the live session list (which
// would miss merged/cleaned-up sessions whose ledger still lives
// in .bosun/state/ until next round).
func ListSessions(repoRoot string) ([]string, error) {
	dir := filepath.Join(repoRoot, stateDirRel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("usage: read state dir: %w", err)
	}
	var labels []string
	for _, e := range entries {
		name := e.Name()
		const suffix = ".usage"
		if !strings.HasSuffix(name, suffix) || e.IsDir() {
			continue
		}
		labels = append(labels, strings.TrimSuffix(name, suffix))
	}
	sort.Strings(labels)
	return labels, nil
}

// Append records one Entry to the session's ledger. Creates the
// state directory and ledger file as needed. Concurrent-safe: each
// call is a single sub-PIPE_BUF write opened with O_APPEND, which
// POSIX guarantees won't interleave with other appenders.
//
// Validation:
//   - sessionName must be non-empty
//   - tokens_in / tokens_out must be >= 0
//   - cost_usd must be >= 0
//   - model may be empty (operator's wrapper decides)
//
// The timestamp is captured at Append time when Entry.Timestamp is
// the zero value; callers can also pass an explicit timestamp for
// deterministic tests.
func Append(repoRoot, sessionName string, e Entry) error {
	if sessionName == "" {
		return fmt.Errorf("usage: sessionName is required")
	}
	if e.TokensIn < 0 || e.TokensOut < 0 {
		return fmt.Errorf("usage: token counts must be >= 0 (got in=%d, out=%d)", e.TokensIn, e.TokensOut)
	}
	if e.CostUSD < 0 {
		return fmt.Errorf("usage: cost_usd must be >= 0 (got %f)", e.CostUSD)
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}

	dir := filepath.Join(repoRoot, stateDirRel)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("usage: mkdir %s: %w", dir, err)
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("usage: marshal entry: %w", err)
	}
	line = append(line, '\n')

	path := usagePath(repoRoot, sessionName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("usage: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("usage: write %s: %w", path, err)
	}
	return nil
}

// Read returns all entries for sessionName in append order. Missing
// file returns (nil, nil) — no usage recorded is not an error.
// Malformed lines are silently skipped so a partially-written turn
// from a crash doesn't poison the whole ledger.
func Read(repoRoot, sessionName string) ([]Entry, error) {
	path := usagePath(repoRoot, sessionName)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("usage: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	// Allow longer lines than the default 64KB in case an agent
	// emits a particularly verbose turn_label or model name. 1MB
	// is more than any sensible usage ledger entry would need.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			// Malformed line — skip silently. The ledger is best-
			// effort observability, not load-bearing state. Other
			// well-formed lines still contribute to the totals.
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("usage: scan %s: %w", path, err)
	}
	return entries, nil
}

// ReadTotals sums every entry's tokens + cost for sessionName.
// Returns zeroed Totals (no error) when the ledger file is absent.
//
// Implements the UsageReader contract used by session.Derive — the
// indirection lets callers swap a fake in tests without touching
// the on-disk format.
func ReadTotals(repoRoot, sessionName string) (Totals, error) {
	entries, err := Read(repoRoot, sessionName)
	if err != nil {
		return Totals{}, err
	}
	var t Totals
	for _, e := range entries {
		t.TokensIn += e.TokensIn
		t.TokensOut += e.TokensOut
		t.CostUSD += e.CostUSD
		t.TurnCount++
		if e.Timestamp.After(t.LastAt) {
			t.LastAt = e.Timestamp
			t.LastModel = e.Model
		}
	}
	return t, nil
}

// Clear removes the usage ledger for sessionName. Called by
// state.Store.Clear when a session is reaped so a future session
// with the same label starts with a clean ledger. Missing file is
// not an error.
func Clear(repoRoot, sessionName string) error {
	if sessionName == "" {
		return fmt.Errorf("usage: sessionName is required")
	}
	path := usagePath(repoRoot, sessionName)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("usage: remove %s: %w", path, err)
	}
	return nil
}

// PerModel returns CostUSD broken down by model name, summed across
// all entries. Useful for renderers that want to show "claude-opus:
// $1.42 · claude-haiku: $0.03" instead of one opaque total.
// Entries with empty Model land under the "unknown" key so the
// total still reconciles.
func PerModel(entries []Entry) map[string]float64 {
	out := map[string]float64{}
	for _, e := range entries {
		key := e.Model
		if key == "" {
			key = "unknown"
		}
		out[key] += e.CostUSD
	}
	return out
}

// SortedModels returns the model keys from m in descending cost
// order. Tie-broken alphabetically for determinism. Renderers iterate
// this to display the most expensive models first.
func SortedModels(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}
