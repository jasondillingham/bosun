package brief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_BasicSections(t *testing.T) {
	plan := `# Refactor plan

## session-1
Refactor internal/auth.

## session-2
Update HTTP routing.

## session-3
Migrate storage to pgx v5.
`
	briefs := parseContent(plan)
	if len(briefs) != 3 {
		t.Fatalf("len(briefs) = %d, want 3", len(briefs))
	}
	if briefs[0].Session != 1 || !strings.Contains(briefs[0].Body, "Refactor internal/auth") {
		t.Errorf("session-1 body wrong: %+v", briefs[0])
	}
	if briefs[2].Session != 3 || !strings.Contains(briefs[2].Body, "pgx v5") {
		t.Errorf("session-3 body wrong: %+v", briefs[2])
	}
}

func TestParse_HandlesCRLF(t *testing.T) {
	plan := "## session-1\r\nbody one\r\n\r\n## session-2\r\nbody two\r\n"
	briefs := parseContent(plan)
	if len(briefs) != 2 {
		t.Fatalf("len(briefs) = %d, want 2", len(briefs))
	}
}

func TestParse_NoHeadings(t *testing.T) {
	briefs := parseContent("just some prose\nnothing structured")
	if len(briefs) != 0 {
		t.Fatalf("len(briefs) = %d, want 0", len(briefs))
	}
}

func TestParse_GapsArePreserved(t *testing.T) {
	plan := `## session-1
body 1

## session-5
body 5
`
	briefs := parseContent(plan)
	if len(briefs) != 2 {
		t.Fatalf("len(briefs) = %d, want 2", len(briefs))
	}
	if briefs[1].Session != 5 {
		t.Fatalf("second brief session = %d, want 5", briefs[1].Session)
	}
}

func TestWriteToWorktree(t *testing.T) {
	wt := t.TempDir()
	b := Brief{Session: 2, Body: "do the thing"}
	if err := WriteToWorktree(wt, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(wt, "BOSUN_BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "session-2") || !strings.Contains(string(data), "do the thing") {
		t.Fatalf("BOSUN_BRIEF.md content unexpected: %s", string(data))
	}
}

func TestWriteToWorktree_IncludesWorkflowPreamble(t *testing.T) {
	// Regression test for the v0.1 dogfood finding: agents finished
	// implementation but skipped commit + claim + done. The preamble
	// makes the lifecycle explicit in the brief.
	wt := t.TempDir()
	b := Brief{Session: 3, Body: "implement X"}
	if err := WriteToWorktree(wt, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(wt, "BOSUN_BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Preamble must mention every lifecycle step.
	for _, want := range []string{
		"How to work this session",
		"make check",
		"git add . && git commit",
		"bosun claim session-3",
		"bosun done session-3",
		"## Your assignment",
		"implement X",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("brief missing %q\n--- full content ---\n%s", want, content)
		}
	}
}

func TestWriteSessionPointer(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSessionPointer(wt, 4); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(wt, ".claude", "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read .claude/CLAUDE.md: %v", err)
	}
	content := string(data)
	for _, want := range []string{"session-4", "BOSUN_BRIEF.md", "bosun-managed"} {
		if !strings.Contains(content, want) {
			t.Errorf("pointer missing %q\n--- full content ---\n%s", want, content)
		}
	}
}

func TestArchivePlan(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(plan, []byte("## session-1\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ArchivePlan(dir, plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".bosun/briefs/plan.last.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "session-1") {
		t.Fatalf("archived plan content unexpected: %s", string(data))
	}
}

func TestLookupBrief(t *testing.T) {
	briefs := []Brief{{Session: 1, Body: "a"}, {Session: 3, Body: "c"}}
	if got := LookupBrief(briefs, 3); got == nil || got.Body != "c" {
		t.Fatalf("LookupBrief(3) = %+v", got)
	}
	if got := LookupBrief(briefs, 2); got != nil {
		t.Fatalf("LookupBrief(2) = %+v, want nil", got)
	}
}
