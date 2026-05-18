package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestScenario_HelpGroupsInOrder asserts `bosun --help` renders the four
// workflow-phase groups in the documented order, each followed by at least
// one expected command. The group titles and command memberships are the
// load-bearing surface a new user reads first; alphabetical drift here
// would defeat the grouping.
func TestScenario_HelpGroupsInOrder(t *testing.T) {
	if bosunBin == "" {
		t.Skip("bosun binary not built (TestMain skipped build)")
	}

	out, err := exec.Command(bosunBin, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("bosun --help: %v\n%s", err, out)
	}
	help := string(out)

	groups := []struct {
		header  string
		members []string
	}{
		{"Setup:", []string{"init", "suggest", "doctor"}},
		{"During a round:", []string{"status", "show", "list", "claim", "done"}},
		{"Finishing a round:", []string{"merge", "cleanup", "adopt", "rescue", "remove"}},
		{"Wiring + advanced:", []string{"config", "mcp", "tui", "serve", "launch", "predict"}},
	}

	prev := -1
	for _, g := range groups {
		idx := strings.Index(help, g.header)
		if idx < 0 {
			t.Fatalf("missing group header %q in --help output:\n%s", g.header, help)
		}
		if idx <= prev {
			t.Fatalf("group header %q appears out of order (idx=%d, prev=%d):\n%s", g.header, idx, prev, help)
		}
		prev = idx
	}

	// Verify each command appears in its group's section: between this
	// group's header and the next group's (or end-of-output for the last).
	for i, g := range groups {
		start := strings.Index(help, g.header)
		end := len(help)
		if i+1 < len(groups) {
			end = strings.Index(help, groups[i+1].header)
		}
		section := help[start:end]
		for _, cmd := range g.members {
			// Cobra renders each command as a left-padded "  <name>"
			// followed by its short description. Match the padded form
			// so a stray mention of the word in another section doesn't
			// satisfy the check.
			needle := "  " + cmd + " "
			if !strings.Contains(section, needle) {
				t.Errorf("group %q section is missing command %q:\n%s", g.header, cmd, section)
			}
		}
	}
}
