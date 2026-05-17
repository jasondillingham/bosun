package main

// Scenarios for the standalone `bosun launch` initial-prompt resolution.
//
// `bosun init --launch --brief X` already defaults the initial prompt to
// "Read BOSUN_BRIEF.md ..." when the operator doesn't override it. Plain
// `bosun launch session-N` (run later, e.g. to reopen a closed window)
// must match: when BOSUN_BRIEF.md exists in the worktree and no
// --initial-prompt was given, the same default fires. These scenarios
// drive `bosun launch` end-to-end with `launcher=print` so the rendered
// shell command lands in captured output where we can grep it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScenario_LaunchDefaultsToBriefPromptWhenBriefExists(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n")
	s.Bosun("init", "1", "--brief", "plan.md")

	// Sanity check: init wrote a brief into the worktree.
	if _, err := os.Stat(filepath.Join(s.WorktreePath(1), "BOSUN_BRIEF.md")); err != nil {
		t.Fatalf("BOSUN_BRIEF.md not present in session-1 worktree: %v", err)
	}

	out := s.Bosun("launch", "session-1")
	s.AssertContainsAll(out, "Launched session-1", "Read BOSUN_BRIEF.md")
}

func TestScenario_LaunchLeavesPromptEmptyWhenNoBrief(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.Bosun("init", "1")

	// Sanity check: no brief was written, since --brief wasn't passed.
	if _, err := os.Stat(filepath.Join(s.WorktreePath(1), "BOSUN_BRIEF.md")); !os.IsNotExist(err) {
		t.Fatalf("BOSUN_BRIEF.md unexpectedly present (or stat err): %v", err)
	}

	out := s.Bosun("launch", "session-1")
	if strings.Contains(out, "Read BOSUN_BRIEF.md") {
		t.Fatalf("launch without a brief should not inject the default prompt:\n%s", out)
	}
	// The print launcher renders the prompt (when set) as a shell-quoted
	// argument right after `claude`. With no prompt, the line ends at
	// `claude\n`. Anything like `claude '...` would mean a default leaked.
	if strings.Contains(out, "claude '") {
		t.Fatalf("printed command should not include a quoted prompt arg:\n%s", out)
	}
}

func TestScenario_LaunchExplicitPromptBeatsBriefDefault(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n")
	s.Bosun("init", "1", "--brief", "plan.md")

	out := s.Bosun("launch", "session-1", "--initial-prompt", "custom kickoff")
	s.AssertContains(out, "'custom kickoff'")
	if strings.Contains(out, "Read BOSUN_BRIEF.md") {
		t.Fatalf("explicit --initial-prompt must override the default:\n%s", out)
	}
}
