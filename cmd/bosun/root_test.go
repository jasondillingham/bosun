package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionDefault pins that an unldflagged `go build ./...` produces a
// binary reporting `dev`. Closes #16: a CI sandbox that just runs
// `go build ./...` must not emit an empty version string.
func TestVersionDefault(t *testing.T) {
	if version != "dev" {
		t.Errorf("package var version: want %q, got %q", "dev", version)
	}
}

// TestRootCmdVersionWired pins that the cobra command picks up the
// package-level version. Without this, a future refactor could leave
// `Version` unset on the root command and silently regress `--version`
// back to `Error: unknown flag: --version`.
func TestRootCmdVersionWired(t *testing.T) {
	root := newRootCmd()
	if root.Version == "" {
		t.Fatal("root.Version is empty — --version flag would not be registered")
	}
	if root.Version != version {
		t.Errorf("root.Version: want %q, got %q", version, root.Version)
	}
}

// TestRootCmdVersionFlag exercises the --version flag end-to-end so we
// catch a regression even if Version is wired but cobra's auto-flag
// behavior changes (e.g. someone disables it via DisableFlagParsing).
func TestRootCmdVersionFlag(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--version returned error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, version) {
		t.Errorf("--version output %q does not contain version %q", got, version)
	}
}
