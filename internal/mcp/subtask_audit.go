package mcp

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

// subtaskAuditEntry is one JSON line written to .bosun/audit/subtask.log.
// The spec (docs/v1.0-sub-task-spec.md §3) calls for the same schema
// shape as spawn's audit log — "extend, don't fork" — so the field set
// matches spawnAuditEntry exactly. The two are kept as separate Go
// types because they live in distinct files and the gate vocabularies
// diverge (per-tool refusal reasons); a shared struct would force the
// gate constants to leak across files.
type subtaskAuditEntry struct {
	Time           string `json:"time"`
	Parent         string `json:"parent"`
	RequestedLabel string `json:"requested_label,omitempty"`
	Outcome        string `json:"outcome"`
	RefusalGate    string `json:"refusal_gate,omitempty"`
	RefusalMessage string `json:"refusal_message,omitempty"`
}

// Refusal gate identifiers for bosun_subtask. Stable vocabulary so log
// consumers can rely on the same strings the brief documents.
const (
	subtaskGateConfigDisabled = "config-disabled"
	subtaskGateInvalidArgs    = "invalid-args"
	subtaskGateParentLiveness = "parent-liveness"
	subtaskGateConcurrentCap  = "concurrent-quota"
	subtaskGateInternal       = "internal"
)

const (
	subtaskAuditDirRel  = ".bosun/audit"
	subtaskAuditLogName = "subtask.log"

	// subtaskAuditMaxBytes and subtaskAuditMaxFiles match the spawn-audit
	// rotation thresholds. The two logs share .bosun/audit/; keeping the
	// rotation policy aligned avoids one log filling the directory while
	// the other rotates aggressively.
	subtaskAuditMaxBytes = 10 * 1024 * 1024
	subtaskAuditMaxFiles = 5
)

// subtaskAuditMu serializes writes from goroutines within a single
// process. Cross-process serialization rides on lockfile.WithLock.
var subtaskAuditMu sync.Mutex

// logSubtaskAttempt appends a single audit entry to
// <repoRoot>/.bosun/audit/subtask.log. Fail-open by contract — same
// stance spawn's audit logger takes: observability must never block the
// load-bearing operation. Errors print to stderr and are swallowed.
func logSubtaskAttempt(repoRoot string, entry subtaskAuditEntry) {
	if entry.Time == "" {
		entry.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if err := writeSubtaskAuditLine(repoRoot, entry); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bosun: subtask audit log write failed: %v\n", err)
	}
}

func writeSubtaskAuditLine(repoRoot string, entry subtaskAuditEntry) error {
	if repoRoot == "" {
		return errors.New("repoRoot is empty")
	}
	dir := filepath.Join(repoRoot, subtaskAuditDirRel)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir audit dir: %w", err)
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	line = append(line, '\n')

	logPath := filepath.Join(dir, subtaskAuditLogName)
	lockPath := logPath + ".lock"

	subtaskAuditMu.Lock()
	defer subtaskAuditMu.Unlock()

	return lockfile.WithLock(lockPath, func() error {
		if err := rotateSubtaskAuditIfNeeded(logPath, len(line)); err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open log: %w", err)
		}
		defer f.Close()
		if _, err := f.Write(line); err != nil {
			return fmt.Errorf("write line: %w", err)
		}
		return nil
	})
}

// rotateSubtaskAuditIfNeeded mirrors rotateSpawnAuditIfNeeded. Two
// rotation routines (one per log file) is cheaper to read than a
// shared helper threaded through both — the bodies are 20 lines of
// straightforward Rename calls.
func rotateSubtaskAuditIfNeeded(logPath string, incoming int) error {
	info, err := os.Stat(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat log: %w", err)
	}
	if info.Size()+int64(incoming) <= subtaskAuditMaxBytes {
		return nil
	}
	oldest := fmt.Sprintf("%s.%d", logPath, subtaskAuditMaxFiles)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", oldest, err)
	}
	for i := subtaskAuditMaxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", logPath, i)
		dst := fmt.Sprintf("%s.%d", logPath, i+1)
		if err := os.Rename(src, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rename %s -> %s: %w", src, dst, err)
		}
	}
	if err := os.Rename(logPath, logPath+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("rename %s -> %s.1: %w", logPath, logPath, err)
	}
	return nil
}
