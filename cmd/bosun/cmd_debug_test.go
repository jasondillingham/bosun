package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initDebugRepo builds a minimal git repo at t.TempDir() with the
// .bosun/ artifacts the debug bundle is expected to enumerate:
// config.json (with a fake api_key), audit logs, spawn-tree.json,
// state markers, claim files, and a merges.log. Returns the repo
// root so the caller can drive loadCtxAt against it.
func initDebugRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@e")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	mustWrite(t, filepath.Join(repo, "f"), "x\n")
	runGit("add", "f")
	runGit("commit", "-q", "-m", "init")

	bosunDir := filepath.Join(repo, ".bosun")
	if err := os.MkdirAll(filepath.Join(bosunDir, "audit"), 0o755); err != nil {
		t.Fatalf("mkdir audit: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bosunDir, "state"), 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bosunDir, "claims"), 0o755); err != nil {
		t.Fatalf("mkdir claims: %v", err)
	}

	// config.json carries a secret the redaction pass must catch.
	mustWrite(t, filepath.Join(bosunDir, "config.json"), `{
  "base_branch": "main",
  "api_key": "sk-FAKE-SECRET-TOKEN-123"
}
`)

	// One spawn audit entry + one subtask audit entry.
	mustWrite(t, filepath.Join(bosunDir, "audit", "spawn.log"),
		`{"event":"spawn","ts":"2026-05-18T15:00:00Z","parent":"session-1","child":"session-2"}`+"\n")
	mustWrite(t, filepath.Join(bosunDir, "audit", "subtask.log"),
		`{"event":"subtask","ts":"2026-05-18T15:00:01Z","parent":"session-1","subtask":"lint"}`+"\n")

	// Spawn tree.
	mustWrite(t, filepath.Join(bosunDir, "spawn-tree.json"),
		`{"version":"v1","sessions":{"session-1":{"depth":0,"spawned_at":"2026-05-18T14:55:00Z"}}}`+"\n")

	// State + claims (file presence is the signal — body is hint).
	mustWrite(t, filepath.Join(bosunDir, "state", "session-1.done"), "wrapped up\n")
	mustWrite(t, filepath.Join(bosunDir, "claims", "session-1.json"),
		`{"session":"session-1","paths":["cmd/bosun/cmd_debug.go"]}`+"\n")

	// merges.log with enough rows to verify the tail logic.
	var merges bytes.Buffer
	for i := 0; i < 5; i++ {
		merges.WriteString(`{"ts":"2026-05-18T15:00:00Z","session":"session-1","sha":"deadbeef"}` + "\n")
	}
	mustWrite(t, filepath.Join(bosunDir, "merges.log"), merges.String())

	return repo
}

// TestDebugBundle_StructureAndSections is the structural lock-in: every
// brief-listed section appears in the bundle, the redaction default
// fires on the fixture's `api_key`, and the trailing checklist is the
// LAST block.
func TestDebugBundle_StructureAndSections(t *testing.T) {
	repo := initDebugRepo(t)
	rc, err := loadCtxAt(repo)
	if err != nil {
		t.Fatalf("loadCtx: %v", err)
	}

	var buf bytes.Buffer
	writeDebugBundle(&buf, rc, debugOptions{redact: true}, time.Date(2026, 5, 18, 15, 0, 0, 0, time.UTC))
	out := buf.String()

	wantSections := []string{
		"BOSUN DEBUG REPORT — 2026-05-18 15:00:00 UTC",
		"bosun --version",
		"bosun doctor",
		"git status",
		"git worktree list --porcelain",
		".bosun/config.json",
		"audit logs (.bosun/audit/)",
		".bosun/spawn-tree.json",
		"state (.bosun/state/)",
		"claims (.bosun/claims/)",
		"merges (.bosun/merges.log, last 50)",
		"environment",
		"BEFORE SHARING THIS FILE",
	}
	for _, want := range wantSections {
		if !strings.Contains(out, want) {
			t.Errorf("bundle missing expected section title %q", want)
		}
	}

	// Redaction must catch the fixture's api_key value.
	if strings.Contains(out, "sk-FAKE-SECRET-TOKEN-123") {
		t.Errorf("bundle leaked secret value — redaction did not fire on api_key")
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("bundle has no <redacted> token despite a known secret in config.json")
	}

	// State/claims default mode should be one-line-per-file (NOT full
	// contents). Look for the mtime token; bail on the body text.
	if !strings.Contains(out, "session-1.done") {
		t.Errorf("state section missing session-1.done summary line")
	}
	if strings.Contains(out, "wrapped up") {
		t.Errorf("state section leaked full file body — should be summary only by default")
	}
	if !strings.Contains(out, "session-1.json") {
		t.Errorf("claims section missing session-1.json summary line")
	}

	// merges.log section should include at least one of the lines.
	if !strings.Contains(out, `"sha":"deadbeef"`) {
		t.Errorf("merges section missing merges.log content")
	}

	// Checklist must be the last block.
	idxChecklist := strings.Index(out, "BEFORE SHARING THIS FILE")
	if idxChecklist < 0 {
		t.Fatalf("checklist section absent")
	}
	tail := out[idxChecklist:]
	for _, sec := range wantSections[:len(wantSections)-1] {
		if strings.Contains(tail, sec) {
			t.Errorf("section %q appears AFTER the checklist; checklist must be last", sec)
		}
	}
}

// TestDebugBundle_NoRedactFlag confirms --no-redact lets the secret
// through verbatim — the operator opted out, so the bundle must
// reflect that.
func TestDebugBundle_NoRedactFlag(t *testing.T) {
	repo := initDebugRepo(t)
	rc, err := loadCtxAt(repo)
	if err != nil {
		t.Fatalf("loadCtx: %v", err)
	}

	var buf bytes.Buffer
	writeDebugBundle(&buf, rc, debugOptions{redact: false}, time.Date(2026, 5, 18, 15, 0, 0, 0, time.UTC))
	out := buf.String()

	if !strings.Contains(out, "sk-FAKE-SECRET-TOKEN-123") {
		t.Errorf("--no-redact should preserve secret values verbatim; not found in bundle")
	}
}

// TestDebugBundle_IncludeAuditExpandsLogs verifies that --include audit
// pulls the full audit-log contents into the bundle instead of the
// last-10-entries summary.
func TestDebugBundle_IncludeAuditExpandsLogs(t *testing.T) {
	repo := initDebugRepo(t)
	// Append an extra audit row so we can distinguish "all rows" from
	// "summary subset" — the summary path would still show every row
	// because the fixture has < 10 entries. Push past 10.
	auditPath := filepath.Join(repo, ".bosun", "audit", "spawn.log")
	f, err := os.OpenFile(auditPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := f.WriteString(`{"event":"spawn","i":` + debugItoa(i) + `}` + "\n"); err != nil {
			t.Fatalf("write audit: %v", err)
		}
	}
	f.Close()

	rc, err := loadCtxAt(repo)
	if err != nil {
		t.Fatalf("loadCtx: %v", err)
	}

	// With default (summary) mode, the first synthetic entry should be
	// trimmed (we have 13 total rows in spawn.log; last 10 keeps 10).
	var defBuf bytes.Buffer
	writeDebugBundle(&defBuf, rc, debugOptions{redact: true}, time.Now().UTC())
	defOut := defBuf.String()
	if strings.Contains(defOut, `"i":0`) {
		t.Errorf("default audit summary should drop earliest row; row 0 still present")
	}
	if !strings.Contains(defOut, `"i":11`) {
		t.Errorf("default audit summary should keep latest row; row 11 missing")
	}

	// With --include audit, the full file is in the bundle.
	var allBuf bytes.Buffer
	writeDebugBundle(&allBuf, rc, debugOptions{redact: true, includes: map[string]bool{"audit": true}}, time.Now().UTC())
	allOut := allBuf.String()
	if !strings.Contains(allOut, `"i":0`) {
		t.Errorf("--include audit should keep row 0; not present")
	}
	if !strings.Contains(allOut, `"i":11`) {
		t.Errorf("--include audit should keep row 11; not present")
	}
}

// TestDebugBundle_IncludeAllExpandsStateAndClaims verifies that
// --include all surfaces the full body of state/claims files (which
// the default mode summarizes to filename + size + mtime).
func TestDebugBundle_IncludeAllExpandsStateAndClaims(t *testing.T) {
	repo := initDebugRepo(t)
	rc, err := loadCtxAt(repo)
	if err != nil {
		t.Fatalf("loadCtx: %v", err)
	}

	var buf bytes.Buffer
	writeDebugBundle(&buf, rc, debugOptions{redact: true, includes: map[string]bool{"all": true}}, time.Now().UTC())
	out := buf.String()

	if !strings.Contains(out, "wrapped up") {
		t.Errorf("--include all should expand state file body; 'wrapped up' marker missing")
	}
	if !strings.Contains(out, `"session":"session-1"`) {
		t.Errorf("--include all should expand claims body; session marker missing")
	}
}

// debugItoa is a sprintf-free helper for the audit fixture rows. Keeps
// the fixture loop's payload terse so the test stays readable.
func debugItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [4]byte
	n := 0
	for i > 0 {
		b[n] = byte('0' + i%10)
		i /= 10
		n++
	}
	out := make([]byte, n)
	for j := 0; j < n; j++ {
		out[j] = b[n-1-j]
	}
	return string(out)
}
