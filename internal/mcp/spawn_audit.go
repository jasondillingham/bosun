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

// spawnAuditEntry is one JSON line written to .bosun/audit/spawn.log.
// Trial #3b/#3c ended with operators unable to answer "what happened?"
// because bosun_spawn calls and refusals left no trace; this is the
// record that closes that gap.
type spawnAuditEntry struct {
	Time           string `json:"time"`
	Parent         string `json:"parent"`
	RequestedLabel string `json:"requested_label,omitempty"`
	Outcome        string `json:"outcome"`
	RefusalGate    string `json:"refusal_gate,omitempty"`
	RefusalMessage string `json:"refusal_message,omitempty"`
}

// Refusal gate identifiers — must match the set documented in the
// brief so log consumers can rely on a stable vocabulary.
const (
	spawnGateConfigDisabled     = "config-disabled"
	spawnGateAllowedForSessions = "allowed-for-sessions"
	spawnGateParentLiveness     = "parent-liveness"
	spawnGateDepthCeiling       = "depth-ceiling"
	spawnGateMaxDepth           = "max-depth"
	spawnGateConcurrentQuota    = "concurrent-quota"
	spawnGateInvalidArgs        = "invalid-args"
)

const (
	spawnAuditDirRel  = ".bosun/audit"
	spawnAuditLogName = "spawn.log"

	// spawnAuditMaxBytes triggers rotation when the active log would
	// exceed this size after the next write. 10MB matches the pattern
	// cribbed from homelab-status-mcp's internal/audit.
	spawnAuditMaxBytes = 10 * 1024 * 1024
	// spawnAuditMaxFiles caps how many rotated copies are retained
	// (spawn.log.1 … spawn.log.5). The active spawn.log is in addition.
	spawnAuditMaxFiles = 5
)

// spawnAuditMu serializes writes from goroutines within a single
// process. Cross-process serialization rides on lockfile.WithLock
// inside writeSpawnAuditLine.
var spawnAuditMu sync.Mutex

// logSpawnAttempt appends a single audit entry to
// <repoRoot>/.bosun/audit/spawn.log. Fail-open by contract: any I/O
// error is reported to stderr and swallowed so the caller — always
// bosun_spawn — never inherits an audit-pipeline failure.
//
// Auditing is observability; spawning is the load-bearing operation.
func logSpawnAttempt(repoRoot string, entry spawnAuditEntry) {
	if entry.Time == "" {
		entry.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if err := writeSpawnAuditLine(repoRoot, entry); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: spawn audit log write failed: %v\n", err)
	}
}

// writeSpawnAuditLine is the error-returning core. Split out so tests
// can assert on the failure mode without exercising the stderr path.
func writeSpawnAuditLine(repoRoot string, entry spawnAuditEntry) error {
	if repoRoot == "" {
		return errors.New("repoRoot is empty")
	}
	dir := filepath.Join(repoRoot, spawnAuditDirRel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir audit dir: %w", err)
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	line = append(line, '\n')

	logPath := filepath.Join(dir, spawnAuditLogName)
	lockPath := logPath + ".lock"

	spawnAuditMu.Lock()
	defer spawnAuditMu.Unlock()

	return lockfile.WithLock(lockPath, func() error {
		if err := rotateSpawnAuditIfNeeded(logPath, len(line)); err != nil {
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

// rotateSpawnAuditIfNeeded rotates spawn.log → spawn.log.1 → … →
// spawn.log.5 when appending `incoming` bytes would push spawn.log
// past spawnAuditMaxBytes. The oldest copy (.5) is dropped. Missing
// rotated files are skipped, matching the convention from the
// homelab-status-mcp pattern.
func rotateSpawnAuditIfNeeded(logPath string, incoming int) error {
	info, err := os.Stat(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat log: %w", err)
	}
	if info.Size()+int64(incoming) <= spawnAuditMaxBytes {
		return nil
	}
	// Drop the oldest, shift the rest down one slot.
	oldest := fmt.Sprintf("%s.%d", logPath, spawnAuditMaxFiles)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", oldest, err)
	}
	for i := spawnAuditMaxFiles - 1; i >= 1; i-- {
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
