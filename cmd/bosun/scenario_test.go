package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// bosunBin is the path to the compiled bosun binary, populated by TestMain
// and shared across every test in this package. Building once avoids
// paying the cost on each scenario.
var bosunBin string

// fakeAgentBin is the path to a tiny `claude` binary compiled in
// TestMain. Used by scenario tests that need proc.Running to detect a
// live agent in a worktree. Empty when the build failed (or on
// Windows) — those tests t.Skip rather than hard-fail.
var fakeAgentBin string

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

	// Build the fake-agent binary once per test process so individual
	// scenario tests don't each pay the `go build` cost. macOS code-
	// signing kills relocated binaries, so we can't just copy /bin/sleep;
	// a freshly-built Go binary with argv[0]="claude" is the only way
	// gopsutil's Name() will report "claude" for the subprocess.
	fakeAgentSrc := filepath.Join(tmp, "fake-agent.go")
	if err := os.WriteFile(fakeAgentSrc, []byte("package main\nimport \"time\"\nfunc main() { time.Sleep(60 * time.Second) }\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: write fake-agent source: %v\n", err)
		os.Exit(1)
	}
	fakeAgentBin = filepath.Join(tmp, "claude")
	cmd = exec.Command("go", "build", "-o", fakeAgentBin, fakeAgentSrc)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Don't fail TestMain — scenarios that need the fake agent
		// will call t.Skip() when fakeAgentBin is "".
		fmt.Fprintf(os.Stderr, "TestMain: fake-agent build failed: %v\n%s", err, out)
		fakeAgentBin = ""
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
	// Any test that runs `bosun init --launch` will auto-spawn an MCP
	// daemon detached from the test process. Kill it on cleanup so we
	// don't leak processes (or sockets under /tmp for long repo paths).
	t.Cleanup(func() { killSpawnedMcpDaemon(repo) })
	return s
}

// killSpawnedMcpDaemon reads .bosun/mcp.pid in repoRoot, sends SIGTERM
// to whatever pid it names, and removes the socket file (which may live
// under /tmp for fallback paths). Safe to call when no daemon was
// spawned — missing files and dead pids are silently ignored.
func killSpawnedMcpDaemon(repoRoot string) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".bosun", "mcp.pid"))
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 1 {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err == nil && pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	if len(lines) >= 2 {
		_ = os.Remove(strings.TrimSpace(lines[1]))
	}
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

// WorktreePath returns the worktree path for session N. Real `bosun init`
// stamps each round's dirs with a UTC timestamp (scheme C in
// docs/uid-worktree-design.md), so the on-disk name is
// `<repo>-bosun-<YYYYMMDD-HHMMSS>-<N>`. The helper discovers that name by
// scanning the parent dir, falling back to the legacy `-bosun-<N>` form
// when nothing matches — that fallback is what `AssertWorktreeMissing`
// and pre-init seed steps depend on.
func (s *scenario) WorktreePath(n int) string {
	return s.WorktreePathLabel(fmt.Sprintf("session-%d", n))
}

// WorktreePathLabel is the named-session analogue of WorktreePath. The
// "session-N" prefix is stripped from a numeric label so callers can
// share the discovery logic for both forms.
func (s *scenario) WorktreePathLabel(label string) string {
	sub := label
	if rest, ok := strings.CutPrefix(label, "session-"); ok {
		sub = rest
	}
	prefix := s.name + "-bosun-"
	legacy := filepath.Join(s.parent, prefix+sub)

	entries, err := os.ReadDir(s.parent)
	if err != nil {
		// Parent missing / unreadable — fall back to the legacy form so
		// AssertWorktreeMissing and pre-init seeding can still spell the
		// expected path. The test will fail elsewhere if the dir was
		// supposed to exist.
		return legacy
	}
	wantSuffix := "-" + sub
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		tail := strings.TrimPrefix(name, prefix)
		// Legacy form: `<prefix><sub>` — exact tail match.
		if tail == sub {
			matches = append(matches, name)
			continue
		}
		// Scheme-C form: `<prefix><YYYYMMDD-HHMMSS>-<sub>`. The tail must
		// end with the desired suffix and the part before it must look
		// like the canonical UTC timestamp the format string in cmd_init
		// produces. Stricter than `strings.HasSuffix` so a sibling lane
		// like `session-12` never matches a query for `session-2`.
		if strings.HasSuffix(tail, wantSuffix) {
			head := strings.TrimSuffix(tail, wantSuffix)
			if looksLikeRoundTimestamp(head) {
				matches = append(matches, name)
			}
		}
	}
	switch len(matches) {
	case 0:
		return legacy
	case 1:
		return filepath.Join(s.parent, matches[0])
	default:
		s.t.Fatalf("scenario.WorktreePathLabel(%q): multiple dirs match: %v", label, matches)
		return ""
	}
}

// RoundTimestamp returns the `YYYYMMDD-HHMMSS` token bosun baked into
// the worktree dir names for this round, or "" if no such dir exists
// yet (e.g. before `bosun init`). Resume tests that hand-seed
// `.bosun/init.state` need this so the seeded `round_timestamp` matches
// the actual worktrees on disk; otherwise resume looks for legacy paths
// and refuses on missing completed-session worktrees.
func (s *scenario) RoundTimestamp() string {
	entries, err := os.ReadDir(s.parent)
	if err != nil {
		return ""
	}
	prefix := s.name + "-bosun-"
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		tail := strings.TrimPrefix(name, prefix)
		// Expect "<TS>-<sub>" where TS is exactly 15 chars
		// (YYYYMMDD-HHMMSS). Any sibling matching this prefix shape
		// is the freshest round; tests run in fresh tmpdirs so a single
		// match is the rule.
		if len(tail) >= 16 && tail[8] == '-' && tail[15] == '-' && looksLikeRoundTimestamp(tail[:15]) {
			return tail[:15]
		}
	}
	return ""
}

// looksLikeRoundTimestamp reports whether s has the shape produced by
// cmd_init's `initRoundTimestampFmt` (UTC `YYYYMMDD-HHMMSS`). Keeps the
// path-glob in WorktreePathLabel from confusing a sibling label whose
// own value happens to end in `-<sub>`.
func looksLikeRoundTimestamp(s string) bool {
	if len(s) != 15 || s[8] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 {
			continue
		}
		ch := s[i]
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
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
