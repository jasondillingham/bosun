package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/brief"
)

func TestRunNewBrief_PatternRequired(t *testing.T) {
	var buf bytes.Buffer
	err := runNewBrief(&buf, "", "", false)
	if err == nil {
		t.Fatal("expected error when --pattern omitted")
	}
	if !strings.Contains(err.Error(), "--pattern is required") {
		t.Errorf("error message should mention --pattern, got: %v", err)
	}
}

func TestRunNewBrief_UnknownPatternErrors(t *testing.T) {
	var buf bytes.Buffer
	err := runNewBrief(&buf, "nonesuch", "", false)
	if err == nil {
		t.Fatal("expected error for unknown pattern")
	}
	if !strings.Contains(err.Error(), "unknown pattern") {
		t.Errorf("error should mention 'unknown pattern', got: %v", err)
	}
}

func TestRunNewBrief_WritesToStdout(t *testing.T) {
	var buf bytes.Buffer
	if err := runNewBrief(&buf, "recipe", "", false); err != nil {
		t.Fatalf("runNewBrief: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "## session-1") {
		t.Errorf("stdout missing ## session-1 heading; got:\n%s", out)
	}
	if !strings.Contains(out, "{{") {
		t.Error("stdout missing {{placeholder}} markers")
	}
}

func TestRunNewBrief_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "my-plan.md")

	var buf bytes.Buffer
	if err := runNewBrief(&buf, "audit", target, false); err != nil {
		t.Fatalf("runNewBrief: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if !strings.Contains(string(data), "## session-1") {
		t.Errorf("file missing ## session-1 heading; got:\n%s", string(data))
	}

	// Stdout should carry a confirmation, not the body.
	stdout := buf.String()
	if !strings.Contains(stdout, "Wrote audit pattern") {
		t.Errorf("stdout missing success line; got: %s", stdout)
	}
	if !strings.Contains(stdout, "bosun init --brief") {
		t.Errorf("stdout missing next-step hint; got: %s", stdout)
	}
}

func TestRunNewBrief_ListPatterns(t *testing.T) {
	var buf bytes.Buffer
	if err := runNewBrief(&buf, "", "", true); err != nil {
		t.Fatalf("runNewBrief: %v", err)
	}
	out := buf.String()
	for _, name := range []string{"recipe", "review", "audit", "cleanup"} {
		if !strings.Contains(out, name+":") {
			t.Errorf("list-patterns output missing %q row; got:\n%s", name, out)
		}
	}
	// Each line should look like "name: description"
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, ":") {
			t.Errorf("malformed list-patterns row (no colon): %q", line)
		}
	}
}

func TestRunNewBrief_ListPatternsIgnoresPattern(t *testing.T) {
	// --list-patterns takes precedence and doesn't require --pattern.
	var buf bytes.Buffer
	if err := runNewBrief(&buf, "", "", true); err != nil {
		t.Fatalf("runNewBrief with only --list-patterns: %v", err)
	}
	if !strings.Contains(buf.String(), "recipe:") {
		t.Errorf("--list-patterns alone should still list; got: %s", buf.String())
	}
}

// TestRunNewBrief_OutputFeedsBosunInit is the end-to-end contract test:
// every pattern's output must parse through brief.ParseString without
// error, so the documented workflow
//
//	bosun new-brief --pattern X > plan.md && bosun init --brief plan.md
//
// actually works rather than dying at init time on a heading the parser
// can't handle.
func TestRunNewBrief_OutputFeedsBosunInit(t *testing.T) {
	for _, name := range []string{"recipe", "review", "audit", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, name+"-plan.md")
			var buf bytes.Buffer
			if err := runNewBrief(&buf, name, target, false); err != nil {
				t.Fatalf("runNewBrief: %v", err)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			briefs, err := brief.ParseString(string(data))
			if err != nil {
				t.Fatalf("%s pattern fails brief.ParseString: %v", name, err)
			}
			if len(briefs) == 0 {
				t.Fatalf("%s pattern parses but contains no ## session-N headings", name)
			}
		})
	}
}
