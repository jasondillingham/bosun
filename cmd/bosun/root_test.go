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
	if !strings.Contains(got, resolvedVersion()) {
		t.Errorf("--version output %q does not contain resolved version %q", got, resolvedVersion())
	}
}

// TestResolvedVersion_LdflagsWins pins the priority order: when the
// ldflags-injected `version` var is set to something other than "dev",
// resolvedVersion returns it directly without consulting BuildInfo.
// Closes #25: production binaries built by GoReleaser or `make build`
// must report the injected version, not the module fallback.
func TestResolvedVersion_LdflagsWins(t *testing.T) {
	orig := version
	defer func() { version = orig }()

	version = "v9.9.9-test"
	if got := resolvedVersion(); got != "v9.9.9-test" {
		t.Errorf("resolvedVersion() = %q, want %q (ldflags should win)", got, "v9.9.9-test")
	}
}

// TestResolvedVersion_FallsBackToDev pins the test-binary case:
// runtime/debug.BuildInfo returns Main.Version="(devel)" for a binary
// built without a module-tag pin, and resolvedVersion must filter
// that out rather than reporting "(devel)" to the user. The test runs
// against the actual go test binary, so this is the same context every
// CI invocation hits.
func TestResolvedVersion_FallsBackToDev(t *testing.T) {
	// version is "dev" in the test binary (no ldflags). resolvedVersion
	// should either land at "dev" (BuildInfo returned "(devel)") or
	// emit a real module version if the test was somehow tagged. The
	// failure shape we're guarding against is "(devel)" leaking through.
	got := resolvedVersion()
	if got == "(devel)" {
		t.Errorf("resolvedVersion() leaked %q — BuildInfo's (devel) sentinel should be filtered to %q", got, "dev")
	}
}
