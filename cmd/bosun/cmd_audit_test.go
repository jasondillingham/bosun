package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedAuditLogs writes deterministic spawn.log and subtask.log
// fixtures into repo/.bosun/audit/. The lines are kept simple so
// assertion failures point cleanly at the filter behaviour rather
// than the fixture shape.
func seedAuditLogs(t *testing.T, repo string) {
	t.Helper()
	dir := filepath.Join(repo, ".bosun", "audit")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir audit dir: %v", err)
	}
	spawn := `{"time":"2026-05-20T10:00:00Z","parent":"session-1","requested_label":"session-1.auth","outcome":"granted"}
{"time":"2026-05-20T10:01:00Z","parent":"session-2","requested_label":"session-2.db","outcome":"refused","refusal_gate":"max-depth","refusal_message":"too deep"}
`
	subtask := `{"time":"2026-05-20T10:02:00Z","parent":"session-1","requested_label":"task-grep","outcome":"granted"}
{"time":"2026-05-20T10:03:00Z","parent":"session-3","requested_label":"task-lint","outcome":"refused","refusal_gate":"concurrent-quota","refusal_message":"over cap"}
`
	mustWrite(t, filepath.Join(dir, "spawn.log"), spawn)
	mustWrite(t, filepath.Join(dir, "subtask.log"), subtask)
}

// TestRunAudit_AllKindsHumanFormat confirms the default invocation
// reads both logs, tags each row with its source, and renders them
// in a grep-friendly columnar layout.
func TestRunAudit_AllKindsHumanFormat(t *testing.T) {
	repo := initBosunRepo(t)
	seedAuditLogs(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "all"}); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
	})

	for _, want := range []string{
		"session-1", "session-2", "session-3",
		"spawn", "subtask",
		"granted", "refused",
		"gate=max-depth", "gate=concurrent-quota",
		"too deep", "over cap",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestRunAudit_JSONRoundTrip confirms --json emits one JSON object
// per audit entry on its own line, with the kind tag attached and
// keys preserved.
func TestRunAudit_JSONRoundTrip(t *testing.T) {
	repo := initBosunRepo(t)
	seedAuditLogs(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "all", json: true}); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d JSON lines, want 4\n%s", len(lines), out)
	}
	kinds := map[string]int{}
	for _, line := range lines {
		var row auditRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		if row.Kind == "" {
			t.Errorf("kind missing on row %+v", row)
		}
		kinds[row.Kind]++
	}
	if kinds["spawn"] != 2 || kinds["subtask"] != 2 {
		t.Errorf("kinds = %v, want spawn:2 subtask:2", kinds)
	}
}

// TestRunAudit_SessionFilter scopes output to entries whose parent
// or requested_label matches the session arg.
func TestRunAudit_SessionFilter(t *testing.T) {
	repo := initBosunRepo(t)
	seedAuditLogs(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "all", session: "session-1"}); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
	})

	if !strings.Contains(out, "session-1") {
		t.Errorf("session-1 missing from output:\n%s", out)
	}
	if strings.Contains(out, "session-2") {
		t.Errorf("session-2 leaked into session-1 filter:\n%s", out)
	}
	if strings.Contains(out, "session-3") {
		t.Errorf("session-3 leaked into session-1 filter:\n%s", out)
	}
}

// TestRunAudit_OutcomeFilter scopes to refused/granted decisions —
// the canonical operator question is "show me what got blocked".
func TestRunAudit_OutcomeFilter(t *testing.T) {
	repo := initBosunRepo(t)
	seedAuditLogs(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "all", outcome: "refused"}); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
	})

	if !strings.Contains(out, "refused") {
		t.Errorf("refused entries missing:\n%s", out)
	}
	if strings.Contains(out, "granted") {
		t.Errorf("granted entries leaked into refused filter:\n%s", out)
	}
}

// TestRunAudit_TailN limits output to the most recent N rows.
func TestRunAudit_TailN(t *testing.T) {
	repo := initBosunRepo(t)
	seedAuditLogs(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "all", tail: 1, json: true}); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1\n%s", len(lines), out)
	}
}

// TestRunAudit_KindFilter limits to a single source file.
func TestRunAudit_KindFilter(t *testing.T) {
	repo := initBosunRepo(t)
	seedAuditLogs(t, repo)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "spawn", json: true}); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
	})
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, `"kind":"spawn"`) {
			t.Errorf("subtask row leaked into kind=spawn output: %s", line)
		}
	}
}

// TestRunAudit_MissingLogsAreNotErrors keeps an empty repo from
// erroring out — "no events yet" must render as a friendly message,
// not a stack trace.
func TestRunAudit_MissingLogsAreNotErrors(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "all"}); err != nil {
			t.Fatalf("runAudit on empty repo: %v", err)
		}
	})
	if !strings.Contains(out, "no audit entries") {
		t.Errorf("expected friendly empty-state message, got:\n%s", out)
	}
}

// TestRunAudit_RejectsUnknownKind keeps typos from silently reading
// nothing — the operator should see why their filter showed no rows.
func TestRunAudit_RejectsUnknownKind(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)

	err := runAudit(auditOpts{kind: "claims"})
	if err == nil {
		t.Fatal("expected error for unknown --kind, got nil")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error should mention --kind, got: %v", err)
	}
}

// TestRunAudit_MalformedLinesAreSkipped: a torn write or hand-edited
// log line shouldn't crash the read. The good lines must still come
// through.
func TestRunAudit_MalformedLinesAreSkipped(t *testing.T) {
	repo := initBosunRepo(t)
	dir := filepath.Join(repo, ".bosun", "audit")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir audit dir: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "spawn.log"),
		"{\"time\":\"2026-05-20T10:00:00Z\",\"parent\":\"session-1\",\"outcome\":\"granted\"}\n"+
			"{this is not valid json\n"+
			"{\"time\":\"2026-05-20T10:01:00Z\",\"parent\":\"session-2\",\"outcome\":\"refused\"}\n")
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runAudit(auditOpts{kind: "spawn", json: true}); err != nil {
			t.Fatalf("runAudit: %v", err)
		}
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (malformed line skipped)\n%s", len(lines), out)
	}
}
