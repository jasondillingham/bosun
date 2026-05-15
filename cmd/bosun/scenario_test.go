package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bosunBin is the path to the compiled bosun binary, populated by TestMain
// and shared across every test in this package. Building once avoids
// paying the cost on each scenario.
var bosunBin string

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// Scenarios use POSIX shell habits (forward slashes, no .exe);
		// they're skipped on Windows in the existing integration test too.
		// We still want unit-style tests to run, so don't bail out here —
		// just don't build a binary.
		os.Exit(m.Run())
	}

	tmp, err := os.MkdirTemp("", "bosun-test-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: mkdir temp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	bosunBin = filepath.Join(tmp, "bosun")
	cmd := exec.Command("go", "build", "-o", bosunBin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: build failed: %v\n%s", err, out)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// scenario is a single test scenario operating on its own temp git repo.
// Each scenario gets a fresh main worktree at <tempDir>/myproj; sibling
// worktrees go to <tempDir>/myproj-bosun-N as bosun creates them.
type scenario struct {
	t      *testing.T
	repo   string // main worktree path (absolute)
	parent string // parent dir (where sibling worktrees live)
	name   string // basename of repo dir (always "myproj" here)
}

// newScenario builds a fresh git repo for the test and returns a scenario
// handle. The repo is created under t.TempDir() so it's auto-cleaned.
func newScenario(t *testing.T) *scenario {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("scenario tests use POSIX shell helpers")
	}
	if bosunBin == "" {
		t.Skip("bosun binary not built (TestMain skipped build)")
	}

	parent := t.TempDir()
	repo := filepath.Join(parent, "myproj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	s := &scenario{t: t, repo: repo, parent: parent, name: "myproj"}
	s.gitInit()
	return s
}

// gitInit sets up an empty repo on `main` with a single commit.
func (s *scenario) gitInit() {
	s.t.Helper()
	s.GitIn(s.repo, "init", "-q", "-b", "main")
	s.GitIn(s.repo, "config", "user.email", "test@example.com")
	s.GitIn(s.repo, "config", "user.name", "Test User")
	s.GitIn(s.repo, "config", "commit.gpgsign", "false")
	s.WriteFile("README.md", "# test\n")
	s.GitIn(s.repo, "add", "README.md")
	s.GitIn(s.repo, "commit", "-q", "-m", "initial")
}

// --- file + git helpers ---

// WriteFile writes content to repo/rel (creating parent dirs as needed).
func (s *scenario) WriteFile(rel, content string) {
	s.t.Helper()
	s.WriteFileIn(s.repo, rel, content)
}

// WriteFileIn writes to dir/rel.
func (s *scenario) WriteFileIn(dir, rel, content string) {
	s.t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatalf("mkdir for %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		s.t.Fatalf("write %s: %v", full, err)
	}
}

// GitIn runs `git <args>` in dir; fails the test on non-zero exit.
func (s *scenario) GitIn(dir string, args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, buf.String())
	}
	return buf.String()
}

// CommitIn stages everything in dir and commits with msg.
func (s *scenario) CommitIn(dir, msg string) {
	s.t.Helper()
	s.GitIn(dir, "add", ".")
	s.GitIn(dir, "commit", "-q", "-m", msg)
}

// --- bosun helpers ---

// Bosun runs bosun with the given args in the main repo, failing the test
// on non-zero exit. Returns combined stdout+stderr.
func (s *scenario) Bosun(args ...string) string {
	s.t.Helper()
	out, err := s.bosunRaw(s.repo, args...)
	if err != nil {
		s.t.Fatalf("bosun %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// BosunErr runs bosun in the main repo and returns (output, error). Use
// when you want to inspect the error explicitly (e.g. asserting it fails).
func (s *scenario) BosunErr(args ...string) (string, error) {
	s.t.Helper()
	return s.bosunRaw(s.repo, args...)
}

// BosunIn runs bosun in an arbitrary directory (useful for testing that
// commands work from inside a linked worktree).
func (s *scenario) BosunIn(dir string, args ...string) string {
	s.t.Helper()
	out, err := s.bosunRaw(dir, args...)
	if err != nil {
		s.t.Fatalf("bosun %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func (s *scenario) bosunRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command(bosunBin, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// --- path helpers ---

// WorktreePath returns the canonical worktree path for session N.
func (s *scenario) WorktreePath(n int) string {
	return filepath.Join(s.parent, fmt.Sprintf("%s-bosun-%d", s.name, n))
}

// --- assertions ---

func (s *scenario) AssertContains(output, want string) {
	s.t.Helper()
	if !strings.Contains(output, want) {
		s.t.Fatalf("output missing %q:\n%s", want, output)
	}
}

func (s *scenario) AssertContainsAll(output string, wants ...string) {
	s.t.Helper()
	for _, w := range wants {
		if !strings.Contains(output, w) {
			s.t.Errorf("output missing %q", w)
		}
	}
	if s.t.Failed() {
		s.t.Fatalf("full output:\n%s", output)
	}
}

func (s *scenario) AssertNotContains(output, unwant string) {
	s.t.Helper()
	if strings.Contains(output, unwant) {
		s.t.Fatalf("output unexpectedly contains %q:\n%s", unwant, output)
	}
}

func (s *scenario) AssertWorktreeExists(n int) {
	s.t.Helper()
	p := s.WorktreePath(n)
	if _, err := os.Stat(p); err != nil {
		s.t.Fatalf("worktree %s missing: %v", p, err)
	}
}

func (s *scenario) AssertWorktreeMissing(n int) {
	s.t.Helper()
	p := s.WorktreePath(n)
	if _, err := os.Stat(p); err == nil {
		s.t.Fatalf("worktree %s still exists, expected gone", p)
	}
}

func (s *scenario) AssertBranchExists(name string) {
	s.t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Dir = s.repo
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("branch %s missing", name)
	}
}

func (s *scenario) AssertBranchMissing(name string) {
	s.t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Dir = s.repo
	if err := cmd.Run(); err == nil {
		s.t.Fatalf("branch %s still exists, expected gone", name)
	}
}

// AssertFileOnMain verifies path exists in the main worktree.
func (s *scenario) AssertFileOnMain(path string) {
	s.t.Helper()
	if _, err := os.Stat(filepath.Join(s.repo, path)); err != nil {
		s.t.Fatalf("expected %s on main: %v", path, err)
	}
}

// --- structured status access ---

type sessionStatus struct {
	Name    string `json:"name"`
	Number  int    `json:"number"`
	Branch  string `json:"branch"`
	State   string `json:"state"`
	Ahead   int    `json:"ahead"`
	Dirty   int    `json:"dirty"`
	Claimed int    `json:"claimed"`
}

type overlapStatus struct {
	Path     string   `json:"path"`
	Sessions []string `json:"sessions"`
}

type statusPayload struct {
	Sessions []sessionStatus `json:"sessions"`
	Overlaps []overlapStatus `json:"overlaps"`
}

// StatusJSON returns the parsed `bosun status --json --with-overlaps` payload.
func (s *scenario) StatusJSON() statusPayload {
	s.t.Helper()
	out := s.Bosun("status", "--json", "--with-overlaps")
	var p statusPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		s.t.Fatalf("status --json parse: %v\n%s", err, out)
	}
	return p
}

// SessionByNumber returns the session with this number, or nil if missing.
func (p statusPayload) SessionByNumber(n int) *sessionStatus {
	for i := range p.Sessions {
		if p.Sessions[i].Number == n {
			return &p.Sessions[i]
		}
	}
	return nil
}

// --- tiny helpers, kept in this file so scenarios_test.go reads cleanly ---

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}
