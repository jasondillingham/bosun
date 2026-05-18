package main

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// sandboxLine extracts the sandbox path from the tour's "Sandbox: <path>"
// header line. The path is later asserted gone (default) or preserved
// (--keep-sandbox).
var sandboxLine = regexp.MustCompile(`(?m)^Sandbox:\s+(\S+)`)

// runTourBin invokes bosun tour against the shared test binary with
// BOSUN_TOUR_AUTO=1 set. Extra args are appended (e.g. --keep-sandbox).
// The tour shells out to its own bosun binary via os.Executable(), so the
// recursive sub-calls all hit the same code under test.
func runTourBin(t *testing.T, extraArgs ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tour shells out to bosun + git with POSIX habits")
	}
	if bosunBin == "" {
		t.Skip("bosun binary not built (TestMain skipped build)")
	}
	args := append([]string{"tour"}, extraArgs...)
	cmd := exec.Command(bosunBin, args...)
	// Use a writable tempdir as the tour's cwd so it doesn't accidentally
	// pick up a parent git repo's state — the tour is supposed to be
	// repo-independent.
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "BOSUN_TOUR_AUTO=1")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// TestTour_AutoMode_EndToEnd drives the full 5-step tour in
// non-interactive mode and asserts (1) every step header is printed,
// (2) the sandbox is announced and then removed, and (3) the per-step
// narrative beats the brief promises actually appear.
func TestTour_AutoMode_EndToEnd(t *testing.T) {
	out, err := runTourBin(t)
	if err != nil {
		t.Fatalf("bosun tour: %v\n%s", err, out)
	}

	// All five step headers, in order.
	steps := []string{
		"── Step 1/5 ",
		"── Step 2/5 ",
		"── Step 3/5 ",
		"── Step 4/5 ",
		"── Step 5/5 ",
	}
	prev := -1
	for _, s := range steps {
		idx := strings.Index(out, s)
		if idx < 0 {
			t.Fatalf("missing step header %q in tour output:\n%s", s, out)
		}
		if idx <= prev {
			t.Fatalf("step header %q out of order (idx=%d, prev=%d)", s, idx, prev)
		}
		prev = idx
	}

	// Step-specific output that proves the underlying bosun sub-commands
	// actually ran (status row labels, predict-report scaffold, merge
	// success, cleanup tally).
	for _, want := range []string{
		"Sandbox:",
		"session-1",
		"session-2",
		"Two worktrees created.",
		"bosun status",
		"No overlap. Safe to proceed.",
		"Predicted conflict report",
		"Both lanes landed on main.",
		"Next steps:",
		"README quickstart",
		"docs/brief-recipe-template.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tour output missing %q\n--- full output:\n%s", want, out)
		}
	}

	// Sandbox path must be (a) emitted in the header line and (b) gone
	// from disk by the time the tour returns. Both halves of the
	// cleanup contract — the directory itself and the sibling worktree
	// dirs bosun init creates — are checked.
	m := sandboxLine.FindStringSubmatch(out)
	if len(m) < 2 {
		t.Fatalf("tour output didn't print a Sandbox: line:\n%s", out)
	}
	sandbox := m[1]
	if !strings.Contains(out, "Sandbox at "+sandbox+" removed.") {
		t.Errorf("tour output didn't confirm sandbox removal for %s:\n%s", sandbox, out)
	}
	if _, err := os.Stat(sandbox); !os.IsNotExist(err) {
		t.Errorf("sandbox %s should be gone after tour, stat err=%v", sandbox, err)
	}
	// Sibling worktree dirs are the v0.3-corruption-shape leak — check
	// the cleanup path swept them too.
	if _, err := os.Stat(sandbox + "-bosun-1"); !os.IsNotExist(err) {
		t.Errorf("sibling worktree %s-bosun-1 should be gone, stat err=%v", sandbox, err)
	}
	if _, err := os.Stat(sandbox + "-bosun-2"); !os.IsNotExist(err) {
		t.Errorf("sibling worktree %s-bosun-2 should be gone, stat err=%v", sandbox, err)
	}
}

// TestTour_KeepSandbox runs the tour with --keep-sandbox and asserts
// the sandbox dir is left on disk for inspection. The test takes
// responsibility for removing it afterwards.
func TestTour_KeepSandbox(t *testing.T) {
	out, err := runTourBin(t, "--keep-sandbox")
	if err != nil {
		t.Fatalf("bosun tour --keep-sandbox: %v\n%s", err, out)
	}
	m := sandboxLine.FindStringSubmatch(out)
	if len(m) < 2 {
		t.Fatalf("tour output didn't print a Sandbox: line:\n%s", out)
	}
	sandbox := m[1]
	t.Cleanup(func() {
		// Belt-and-braces: tour leaves siblings too, since `bosun
		// cleanup` runs *inside* the tour and may have already
		// reaped them — but if not, sweep.
		_ = os.RemoveAll(sandbox + "-bosun-1")
		_ = os.RemoveAll(sandbox + "-bosun-2")
		_ = os.RemoveAll(sandbox)
	})

	if !strings.Contains(out, "Sandbox preserved at "+sandbox) {
		t.Errorf("--keep-sandbox should announce preservation:\n%s", out)
	}
	if _, err := os.Stat(sandbox); err != nil {
		t.Fatalf("--keep-sandbox: sandbox %s should still exist, stat err=%v", sandbox, err)
	}
}

// TestTour_HelpText pins the user-visible help surface so a rename or
// accidental wording change shows up in tests.
func TestTour_HelpText(t *testing.T) {
	if bosunBin == "" {
		t.Skip("bosun binary not built (TestMain skipped build)")
	}
	out, err := exec.Command(bosunBin, "tour", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("bosun tour --help: %v\n%s", err, out)
	}
	help := string(out)
	for _, want := range []string{
		"interactive 5-minute walkthrough",
		"--keep-sandbox",
		"BOSUN_TOUR_AUTO",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("tour --help missing %q:\n%s", want, help)
		}
	}
}
