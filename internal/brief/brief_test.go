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
