package subtask

// Cancellation + counting surface for the sub-task registry.
//
// The Store type in subtask.go writes records as `<parent>/<id>.json`
// under `.bosun/subtasks/`. This file's functions consume those records:
// cancel marks them via a sibling `.cancelled` file (so the original
// record stays intact for forensics), count active records by walking
// the directory and excluding any whose sibling `.cancelled` marker
// exists, and find the parent owning a given id for the parent-match
// gate in bosun_subtask_cancel.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cancelledSuffix is the marker extension. Kept as a constant so the
// count + write code can't drift.
const cancelledSuffix = ".cancelled"

// CancelOutcome captures the registry's view of one cancel attempt.
// Returned by Cancel so the MCP tool can map each branch to the
// correct refusal gate without grepping error strings.
type CancelOutcome struct {
	// Found is true when the id existed (under any parent). False
	// means invalid-id.
	Found bool
	// ParentMatches is true when the id was found under the parent
	// the caller supplied. False with Found=true means parent-
	// mismatch (only the parent may cancel its own sub-tasks).
	ParentMatches bool
	// AlreadyCancelled is true when a `.cancelled` marker already
	// exists for the (parent, id) pair.
	AlreadyCancelled bool
	// Wrote is true when this call created the marker file. False
	// in every refusal branch.
	Wrote bool
}

// Cancel writes the cancellation marker for (parent, id) when the
// id exists under parent and isn't already cancelled. The three
// refusal modes are signaled in the returned outcome rather than
// via error returns — those are reserved for real I/O failures so
// the caller can distinguish "registry says no" from "disk is
// broken."
func Cancel(repoRoot, parent, id string) (CancelOutcome, error) {
	parent = strings.TrimSpace(parent)
	id = strings.TrimSpace(id)
	if parent == "" || id == "" {
		return CancelOutcome{}, errors.New("parent and id are required")
	}

	owner, found, err := FindParent(repoRoot, id)
	if err != nil {
		return CancelOutcome{}, err
	}
	if !found {
		return CancelOutcome{}, nil
	}
	out := CancelOutcome{Found: true, ParentMatches: owner == parent}
	if !out.ParentMatches {
		return out, nil
	}

	already, err := IsCancelled(repoRoot, parent, id)
	if err != nil {
		return out, err
	}
	if already {
		out.AlreadyCancelled = true
		return out, nil
	}

	marker := markerPath(repoRoot, parent, id)
	if err := os.MkdirAll(filepath.Dir(marker), 0o750); err != nil {
		return out, fmt.Errorf("mkdir subtask dir: %w", err)
	}
	body := []byte(time.Now().UTC().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(marker, body, 0o644); err != nil {
		return out, fmt.Errorf("write cancel marker: %w", err)
	}
	out.Wrote = true
	return out, nil
}

// IsCancelled reports whether a `.cancelled` marker exists for the
// (parent, id) pair. Doesn't require the base record to exist —
// callers that need that signal should use FindParent + Found.
func IsCancelled(repoRoot, parent, id string) (bool, error) {
	_, err := os.Stat(markerPath(repoRoot, parent, id))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat cancel marker: %w", err)
}

// FindParent locates the parent whose sub-directory contains id. A
// sub-task id is globally unique by construction (the Store package
// assigns `<parent>.sub.<seq>`), so scanning every parent directory
// is cheap and the right semantic shape for the parent-mismatch gate.
//
// Returns (parent, true, nil) on a hit. Returns ("", false, nil)
// when the id isn't present anywhere. A missing registry root is
// not an error — it just means no sub-tasks have ever been recorded.
func FindParent(repoRoot, id string) (string, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false, errors.New("id is required")
	}
	root := filepath.Join(repoRoot, dirRelative)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read subtask root: %w", err)
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		// Records are written as <id>.json by Store.Create.
		recordPath := filepath.Join(root, ent.Name(), id+".json")
		if _, err := os.Stat(recordPath); err == nil {
			return ent.Name(), true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("stat %s: %w", recordPath, err)
		}
	}
	return "", false, nil
}

// ActiveCount returns the number of un-cancelled sub-task records
// in `.bosun/subtasks/<label>/`. Records are `<id>.json` files; a
// record is "cancelled" when a sibling `<id>.cancelled` file
// exists.
//
// A missing directory returns 0, no error — the operator should
// never see "subtasks count failed" in `bosun status` just because
// a session has never run a sub-task.
func ActiveCount(repoRoot, label string) (int, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return 0, nil
	}
	dir := filepath.Join(repoRoot, dirRelative, label)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", dir, err)
	}
	cancelled := make(map[string]bool, len(entries))
	records := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if base, ok := strings.CutSuffix(name, cancelledSuffix); ok {
			cancelled[base] = true
			continue
		}
		if base, ok := strings.CutSuffix(name, ".json"); ok {
			records = append(records, base)
		}
	}
	active := 0
	for _, r := range records {
		if !cancelled[r] {
			active++
		}
	}
	return active, nil
}

// CountsForSessions returns the active sub-task count for each
// label in one pass. The returned map always has an entry for
// every input label (zero when the session has no subtasks dir).
func CountsForSessions(repoRoot string, labels []string) (map[string]int, error) {
	out := make(map[string]int, len(labels))
	for _, label := range labels {
		n, err := ActiveCount(repoRoot, label)
		if err != nil {
			return nil, err
		}
		out[label] = n
	}
	return out, nil
}

// CreateForTest stages an active sub-task record under (parent, id).
// Exported for tests only — production sub-task creation goes
// through Store.Create. The record body matches the JSON shape
// Store.Create writes so tests exercising Cancel/IsCancelled don't
// need to know the Store internals.
func CreateForTest(repoRoot, parent, id string) error {
	parent = strings.TrimSpace(parent)
	id = strings.TrimSpace(id)
	if parent == "" || id == "" {
		return errors.New("parent and id are required")
	}
	dir := filepath.Join(repoRoot, dirRelative, parent)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Minimal record body — enough for FindParent + ActiveCount to
	// see the file. Tests that need the full Subtask shape should
	// use Store.Create instead.
	body := []byte("{}\n")
	if err := os.WriteFile(filepath.Join(dir, id+".json"), body, 0o644); err != nil {
		return fmt.Errorf("write subtask record: %w", err)
	}
	return nil
}

// markerPath returns the canonical .cancelled marker path for
// (parent, id) under repoRoot.
func markerPath(repoRoot, parent, id string) string {
	return filepath.Join(repoRoot, dirRelative, parent, id+cancelledSuffix)
}

// Roots is a small helper for callers that want to walk every
// parent in the registry (e.g. dashboards aggregating across
// sessions). Returns parent labels in lexicographic order.
func Roots(repoRoot string) ([]string, error) {
	root := filepath.Join(repoRoot, dirRelative)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	out := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		out = append(out, ent.Name())
	}
	sort.Strings(out)
	return out, nil
}
