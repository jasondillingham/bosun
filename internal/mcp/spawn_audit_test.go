package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// readSpawnAuditLines parses every JSON line in
// .bosun/audit/spawn.log under repoRoot. Helper used by the
// table-style tests below.
func readSpawnAuditLines(t *testing.T, repoRoot string) []spawnAuditEntry {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot, spawnAuditDirRel, spawnAuditLogName))
	if err != nil {
		t.Fatalf("open spawn.log: %v", err)
	}
	defer f.Close()
	var out []spawnAuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e spawnAuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan spawn.log: %v", err)
	}
	return out
}

// TestLogSpawnAttempt_SuccessLineShape pins the wire shape of a
// success row: refusal_gate and refusal_message must be omitted, and
// time must round-trip as RFC3339.
func TestLogSpawnAttempt_SuccessLineShape(t *testing.T) {
	repo := t.TempDir()
	logSpawnAttempt(repo, spawnAuditEntry{
		Parent:         "session-1",
		RequestedLabel: "session-1.auth",
		Outcome:        "success",
	})

	lines := readSpawnAuditLines(t, repo)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line; got %d", len(lines))
	}
	got := lines[0]
	if got.Outcome != "success" {
		t.Errorf("outcome: want %q, got %q", "success", got.Outcome)
	}
	if got.Parent != "session-1" {
		t.Errorf("parent: want %q, got %q", "session-1", got.Parent)
	}
	if got.RequestedLabel != "session-1.auth" {
		t.Errorf("requested_label: want %q, got %q", "session-1.auth", got.RequestedLabel)
	}
	if got.RefusalGate != "" {
		t.Errorf("success row must not carry refusal_gate; got %q", got.RefusalGate)
	}
	if got.RefusalMessage != "" {
		t.Errorf("success row must not carry refusal_message; got %q", got.RefusalMessage)
	}
	if _, err := time.Parse(time.RFC3339, got.Time); err != nil {
		t.Errorf("time %q not RFC3339: %v", got.Time, err)
	}

	// Re-marshal the parsed entry and confirm the raw bytes on disk
	// do NOT mention refusal_gate/refusal_message — omitempty must
	// drop them, not emit empty strings.
	raw, err := os.ReadFile(filepath.Join(repo, spawnAuditDirRel, spawnAuditLogName))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	for _, banned := range []string{"refusal_gate", "refusal_message"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("success row leaked %q into JSON: %s", banned, raw)
		}
	}
}

// TestLogSpawnAttempt_RefusedLineShapes covers every gate identifier
// the brief documents. Each gate-name string is the authoritative
// vocabulary the operator filters on; if any of these names drift,
// downstream log queries break silently.
func TestLogSpawnAttempt_RefusedLineShapes(t *testing.T) {
	gates := []string{
		spawnGateConfigDisabled,
		spawnGateAllowedForSessions,
		spawnGateParentLiveness,
		spawnGateDepthCeiling,
		spawnGateMaxDepth,
		spawnGateConcurrentQuota,
		spawnGateInvalidArgs,
	}
	for _, gate := range gates {
		t.Run(gate, func(t *testing.T) {
			repo := t.TempDir()
			logSpawnAttempt(repo, spawnAuditEntry{
				Parent:         "session-1",
				Outcome:        "refused",
				RefusalGate:    gate,
				RefusalMessage: "synthetic message for " + gate,
			})
			lines := readSpawnAuditLines(t, repo)
			if len(lines) != 1 {
				t.Fatalf("expected 1 line; got %d", len(lines))
			}
			got := lines[0]
			if got.Outcome != "refused" {
				t.Errorf("outcome: want refused, got %q", got.Outcome)
			}
			if got.RefusalGate != gate {
				t.Errorf("refusal_gate: want %q, got %q", gate, got.RefusalGate)
			}
			if got.RefusalMessage != "synthetic message for "+gate {
				t.Errorf("refusal_message: want %q, got %q", "synthetic message for "+gate, got.RefusalMessage)
			}
		})
	}
}

// TestSpawnAuditRotation pins the size-based rotation contract.
// Constructed by lowering spawnAuditMaxBytes in-process via a
// targeted call to writeSpawnAuditLine after pre-seeding spawn.log
// with a near-cap payload.
func TestSpawnAuditRotation(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, spawnAuditDirRel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(dir, spawnAuditLogName)

	// Pre-seed spawn.log with a payload that is exactly at the cap.
	// The next write — any non-zero number of bytes — should trigger
	// a rotate to spawn.log.1 before writing.
	seed := make([]byte, spawnAuditMaxBytes)
	for i := range seed {
		seed[i] = 'x'
	}
	if err := os.WriteFile(logPath, seed, 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	// Drive a normal log call. After it returns:
	//   spawn.log.1 must hold the old payload (size == cap)
	//   spawn.log  must hold ONLY the new entry (size < cap)
	logSpawnAttempt(repo, spawnAuditEntry{
		Parent:  "session-1",
		Outcome: "success",
	})

	rotated := logPath + ".1"
	st, err := os.Stat(rotated)
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if st.Size() != int64(spawnAuditMaxBytes) {
		t.Errorf("rotated size: want %d, got %d", spawnAuditMaxBytes, st.Size())
	}

	current, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if len(current) == 0 || len(current) >= spawnAuditMaxBytes {
		t.Errorf("current log unexpectedly sized %d", len(current))
	}
	var got spawnAuditEntry
	if err := json.Unmarshal(current[:len(current)-1], &got); err != nil {
		t.Fatalf("unmarshal new entry: %v", err)
	}
	if got.Parent != "session-1" || got.Outcome != "success" {
		t.Errorf("new entry shape unexpected: %+v", got)
	}
}

// TestSpawnAuditRotation_DropsOldest verifies the .5 cap: after five
// rotations the oldest file is dropped rather than retained as a .6.
func TestSpawnAuditRotation_DropsOldest(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, spawnAuditDirRel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(dir, spawnAuditLogName)

	// Seed the maximum number of existing rotated files plus the
	// active log, each at cap. The next write should rotate, and
	// spawn.log.5 must be the previous spawn.log.4 (the prior .5 is
	// dropped). No spawn.log.6 may appear.
	seed := make([]byte, spawnAuditMaxBytes)
	for i := range seed {
		seed[i] = 'x'
	}
	if err := os.WriteFile(logPath, seed, 0o644); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	for i := 1; i <= spawnAuditMaxFiles; i++ {
		marker := []byte(string(rune('A' + i - 1))) // 'A' for .1, 'B' for .2, …
		if err := os.WriteFile(logPath+"."+itoa(i), marker, 0o644); err != nil {
			t.Fatalf("seed .%d: %v", i, err)
		}
	}

	logSpawnAttempt(repo, spawnAuditEntry{
		Parent:  "session-1",
		Outcome: "success",
	})

	// .1 must now hold the previously-active log (cap-sized).
	st1, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("stat .1: %v", err)
	}
	if st1.Size() != int64(spawnAuditMaxBytes) {
		t.Errorf(".1 size: want %d, got %d", spawnAuditMaxBytes, st1.Size())
	}
	// .5 must now hold what was previously .4, i.e. marker 'D'.
	got5, err := os.ReadFile(logPath + ".5")
	if err != nil {
		t.Fatalf("read .5: %v", err)
	}
	if string(got5) != "D" {
		t.Errorf(".5: want %q (previous .4), got %q", "D", string(got5))
	}
	// No .6 may appear — that's the drop-oldest guarantee.
	if _, err := os.Stat(logPath + ".6"); !os.IsNotExist(err) {
		t.Errorf(".6 unexpectedly exists (err=%v)", err)
	}
}

// TestLogSpawnAttempt_FailOpenOnEmptyRepoRoot pins the safety
// contract: a misconfigured caller (empty repoRoot) must NOT cause a
// panic or propagate an error — auditing is best-effort.
func TestLogSpawnAttempt_FailOpenOnEmptyRepoRoot(t *testing.T) {
	// No panic, no error escape — just a stderr log we don't assert on.
	logSpawnAttempt("", spawnAuditEntry{Parent: "session-1", Outcome: "refused", RefusalGate: spawnGateConfigDisabled})
}

// TestLogSpawnAttempt_FailOpenOnUnwritableDir simulates a permissions
// failure by mkdiring .bosun/audit as a 0o000 directory before the
// write. logSpawnAttempt must swallow the error and return cleanly.
// Skips on Windows (POSIX permission semantics don't apply) and when
// running as root (chmod 000 wouldn't actually block root).
func TestLogSpawnAttempt_FailOpenOnUnwritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only permission test")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0o000")
	}
	repo := t.TempDir()
	auditDir := filepath.Join(repo, spawnAuditDirRel)
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(auditDir, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	// Restore so t.TempDir cleanup can remove the tree.
	defer os.Chmod(auditDir, 0o755)

	// No panic, no propagated error. If the function returned an
	// error here it'd take down bosun_spawn — the test asserts that
	// the fail-open contract holds.
	logSpawnAttempt(repo, spawnAuditEntry{Parent: "session-1", Outcome: "success"})
}

// itoa is a one-digit-only int→string helper kept local to avoid
// importing strconv just for the rotation seed test.
func itoa(i int) string { return string(rune('0' + i)) }
