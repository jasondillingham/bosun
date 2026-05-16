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
	if err := WriteToWorktree(wt, b, ""); err != nil {
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
	b := Brief{Session: 3, Label: "session-3", Body: "implement X"}
	if err := WriteToWorktree(wt, b, ""); err != nil {
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
		// Round-1 MCP discovery contract: agents should know the env
		// var is set and prefer MCP tools over the CLI.
		"BOSUN_MCP_SOCK",
		"bosun_claim",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("brief missing %q\n--- full content ---\n%s", want, content)
		}
	}
}

func TestWriteToWorktree_NamedLabelInPreamble(t *testing.T) {
	wt := t.TempDir()
	b := Brief{Label: "auth", Body: "wire login"}
	if err := WriteToWorktree(wt, b, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(wt, "BOSUN_BRIEF.md"))
	content := string(data)
	for _, want := range []string{
		"# Bosun brief — auth",
		"bosun claim auth",
		"bosun done auth",
		"wire login",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("brief missing %q\n--- full content ---\n%s", want, content)
		}
	}
}

func TestWriteToWorktree_SubstitutesCustomVerifyCmd(t *testing.T) {
	// Round-2 assay dogfood finding: hardcoded `make check` confused agents
	// on projects that use a different verification target. WriteToWorktree
	// now substitutes the {verifyCmd} placeholder so projects can configure
	// their own — e.g. `make test` or `go test ./...`.
	wt := t.TempDir()
	b := Brief{Session: 1, Body: "do something"}
	if err := WriteToWorktree(wt, b, "make test"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(wt, "BOSUN_BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "Run `make test`") {
		t.Errorf("expected `make test` in preamble:\n%s", content)
	}
	if strings.Contains(content, "make check") {
		t.Errorf("`make check` should NOT appear when verifyCmd=make test:\n%s", content)
	}
	if strings.Contains(content, "{verifyCmd}") {
		t.Errorf("placeholder `{verifyCmd}` was not substituted:\n%s", content)
	}
}

func TestWriteToWorktree_DefaultsToMakeCheck(t *testing.T) {
	wt := t.TempDir()
	if err := WriteToWorktree(wt, Brief{Session: 1, Body: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(wt, "BOSUN_BRIEF.md"))
	if !strings.Contains(string(data), "Run `make check`") {
		t.Fatalf("empty verifyCmd should default to make check:\n%s", string(data))
	}
}

func TestWriteSessionPointer(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSessionPointer(wt, "session-4"); err != nil {
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

func TestWriteSessionPointer_NamedLabel(t *testing.T) {
	wt := t.TempDir()
	if err := WriteSessionPointer(wt, "storage"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(wt, ".claude", "CLAUDE.md"))
	content := string(data)
	if !strings.Contains(content, "(storage)") {
		t.Errorf("pointer missing named label:\n%s", content)
	}
	if strings.Contains(content, "session-") {
		t.Errorf("pointer should not mention session-N for named session:\n%s", content)
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
	briefs := []Brief{{Session: 1, Label: "session-1", Body: "a"}, {Session: 3, Label: "session-3", Body: "c"}}
	if got := LookupBrief(briefs, 3); got == nil || got.Body != "c" {
		t.Fatalf("LookupBrief(3) = %+v", got)
	}
	if got := LookupBrief(briefs, 2); got != nil {
		t.Fatalf("LookupBrief(2) = %+v, want nil", got)
	}
}

func TestLookupBriefByLabel(t *testing.T) {
	briefs := []Brief{
		{Session: 1, Label: "session-1", Body: "a"},
		{Label: "auth", Body: "wire login"},
	}
	if got := LookupBriefByLabel(briefs, "auth"); got == nil || got.Body != "wire login" {
		t.Fatalf("LookupBriefByLabel(auth) = %+v", got)
	}
	if got := LookupBriefByLabel(briefs, "session-1"); got == nil || got.Body != "a" {
		t.Fatalf("LookupBriefByLabel(session-1) = %+v", got)
	}
	if got := LookupBriefByLabel(briefs, "missing"); got != nil {
		t.Fatalf("LookupBriefByLabel(missing) = %+v, want nil", got)
	}
}

func TestParse_DependsClause(t *testing.T) {
	cases := []struct {
		name     string
		heading  string
		wantDeps []string
	}{
		{"no clause", "## session-2", nil},
		{"single dep", "## session-2 (depends: session-1)", []string{"session-1"}},
		{"multiple deps", "## session-3 (depends: session-1, session-2)", []string{"session-1", "session-2"}},
		{"bare numeric form", "## session-3 (depends: 1, 2)", []string{"session-1", "session-2"}},
		{"extra whitespace", "## session-4 (depends:  session-1 , session-3 )", []string{"session-1", "session-3"}},
		{"unparseable entries skipped", "## session-2 (depends: session-1, GARBAGE!)", []string{"session-1"}},
		{"named dep", "## auth (depends: storage, http)", []string{"storage", "http"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := tc.heading + "\nbody\n"
			briefs := parseContent(plan)
			if len(briefs) != 1 {
				t.Fatalf("expected 1 brief, got %d", len(briefs))
			}
			got := briefs[0].Depends
			if len(got) != len(tc.wantDeps) {
				t.Fatalf("Depends = %v, want %v", got, tc.wantDeps)
			}
			for i, d := range tc.wantDeps {
				if got[i] != d {
					t.Errorf("Depends[%d] = %q, want %q", i, got[i], d)
				}
			}
		})
	}
}

func TestWriteToWorktree_IncludesDependsBlock(t *testing.T) {
	wt := t.TempDir()
	b := Brief{Session: 3, Label: "session-3", Body: "the assignment", Depends: []string{"session-1", "session-2"}}
	if err := WriteToWorktree(wt, b, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(wt, "BOSUN_BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "## Depends on") {
		t.Errorf("brief missing 'Depends on' header:\n%s", body)
	}
	if !strings.Contains(body, "session-1, session-2") {
		t.Errorf("brief missing dependency list:\n%s", body)
	}
	// The depends block sits between the preamble and the assignment.
	depsIdx := strings.Index(body, "## Depends on")
	assignIdx := strings.Index(body, "## Your assignment")
	if depsIdx < 0 || assignIdx < 0 || depsIdx > assignIdx {
		t.Errorf("'Depends on' should precede 'Your assignment' (deps=%d, assign=%d)", depsIdx, assignIdx)
	}
}

func TestWriteToWorktree_NoDependsBlockWhenEmpty(t *testing.T) {
	wt := t.TempDir()
	b := Brief{Session: 1, Body: "no deps"}
	if err := WriteToWorktree(wt, b, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(wt, "BOSUN_BRIEF.md"))
	if strings.Contains(string(data), "## Depends on") {
		t.Errorf("brief unexpectedly contains 'Depends on' for no-dep session:\n%s", data)
	}
}

func TestLoadArchivedDeps(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".bosun", "briefs"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := `## session-1
foundation

## session-2 (depends: session-1)
wraps session-1

## session-3 (depends: session-1, session-2)
last
`
	if err := os.WriteFile(filepath.Join(repo, archivedPlanRelative), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadArchivedDeps(repo)
	if err != nil {
		t.Fatalf("LoadArchivedDeps: %v", err)
	}
	if d := got["session-2"]; len(d) != 1 || d[0] != "session-1" {
		t.Errorf("deps[session-2] = %v, want [session-1]", d)
	}
	if d := got["session-3"]; len(d) != 2 || d[0] != "session-1" || d[1] != "session-2" {
		t.Errorf("deps[session-3] = %v, want [session-1 session-2]", d)
	}
	if _, has := got["session-1"]; has {
		t.Errorf("deps[session-1] should be absent (no deps declared), got %v", got["session-1"])
	}
}

func TestLoadArchivedDeps_MissingPlanReturnsEmpty(t *testing.T) {
	repo := t.TempDir()
	got, err := LoadArchivedDeps(repo)
	if err != nil {
		t.Fatalf("LoadArchivedDeps: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for missing plan, got %v", got)
	}
}
