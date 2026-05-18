package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupStatusRepo builds a minimal git repo + initial commit and returns
// a runCtx rooted at it. The watch tests need a real runCtx because
// renderStatusOnce reaches into git for worktree info; mocking that
// surface would mean duplicating loadCtx's wiring just for tests.
func setupStatusRepo(t *testing.T) *runCtx {
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
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("add", "f")
	runGit("commit", "-q", "-m", "init")

	rc, err := loadCtxAt(repo)
	if err != nil {
		t.Fatalf("loadCtx: %v", err)
	}
	return rc
}

// TestWatchStatusLoop_RendersFrameAndExitsOnCancel exercises the loop body
// with a pre-cancelled context: one frame should render, then the select
// returns nil immediately rather than sleeping on the ticker. Verifies the
// per-frame ANSI escapes and footer line land in the writer.
func TestWatchStatusLoop_RendersFrameAndExitsOnCancel(t *testing.T) {
	rc := setupStatusRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if err := watchStatusLoop(ctx, &buf, rc, statusOpts{}, 2*time.Second); err != nil {
		t.Fatalf("watchStatusLoop returned error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, ansiCursorHome) {
		t.Errorf("expected cursor-home escape (%q) in frame:\n%q", ansiCursorHome, out)
	}
	if !strings.Contains(out, ansiClearDown) {
		t.Errorf("expected clear-down escape (%q) in frame:\n%q", ansiClearDown, out)
	}
	if !strings.Contains(out, "Press Ctrl-C to exit. Refreshes every 2s.") {
		t.Errorf("expected footer with interval in frame:\n%s", out)
	}
}

// TestRenderWatchFrame_FooterReflectsInterval locks the footer wording in,
// since the brief specifies the exact phrasing the operator sees.
func TestRenderWatchFrame_FooterReflectsInterval(t *testing.T) {
	rc := setupStatusRepo(t)

	cases := []struct {
		interval time.Duration
		want     string
	}{
		{1 * time.Second, "Press Ctrl-C to exit. Refreshes every 1s."},
		{2 * time.Second, "Press Ctrl-C to exit. Refreshes every 2s."},
		{10 * time.Second, "Press Ctrl-C to exit. Refreshes every 10s."},
		{60 * time.Second, "Press Ctrl-C to exit. Refreshes every 60s."},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := renderWatchFrame(&buf, rc, statusOpts{}, tc.interval); err != nil {
			t.Fatalf("renderWatchFrame(%v): %v", tc.interval, err)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("interval %v: want footer %q\nfull frame:\n%s", tc.interval, tc.want, buf.String())
		}
	}
}

// TestFormatRefreshInterval pins the seconds-only formatting that feeds
// the footer.
func TestFormatRefreshInterval(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{1 * time.Second, "1s"},
		{2 * time.Second, "2s"},
		{60 * time.Second, "60s"},
	}
	for _, tc := range cases {
		if got := formatRefreshInterval(tc.in); got != tc.want {
			t.Errorf("formatRefreshInterval(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStatusWatchInterval_OutOfRangeRejected covers the flag validation in
// the RunE: --interval below 1 or above 60 should produce a user error.
// Drives the cobra command directly so the integer-flag plumbing is
// exercised end-to-end without spawning the binary.
func TestStatusWatchInterval_OutOfRangeRejected(t *testing.T) {
	// Run inside a real bosun-like repo so loadCtx (which may run before
	// validation kicks in on some code paths) wouldn't blow up — but the
	// validation we're testing happens before loadCtx is reached.
	rc := setupStatusRepo(t)
	prev, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(prev) })
	if err := os.Chdir(rc.repoRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"zero", []string{"status", "--watch", "--interval", "0"}},
		{"negative", []string{"status", "--watch", "--interval", "-1"}},
		{"over-max", []string{"status", "--watch", "--interval", "61"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			root.SetArgs(tc.args)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			err := root.Execute()
			if err == nil {
				t.Fatalf("expected error for args %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), "between 1 and 60") {
				t.Errorf("expected range hint in error, got %v", err)
			}
		})
	}
}
