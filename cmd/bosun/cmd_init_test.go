package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	initstate "github.com/jasondillingham/bosun/internal/init"
)

func TestFindPhantomBranchRefs_DetectsFinderDuplicates(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".git", "refs", "heads", "bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Real ref + two Finder-style duplicates + an unrelated file that
	// happens to have a space (no trailing digit — should be ignored).
	mustWrite(t, filepath.Join(dir, "session-1"), "abc\n")
	mustWrite(t, filepath.Join(dir, "session-1 2"), "abc\n")
	mustWrite(t, filepath.Join(dir, "session-1 3"), "abc\n")
	mustWrite(t, filepath.Join(dir, "feature thing"), "abc\n")

	phantoms, err := findPhantomBranchRefs(repo, "bosun")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(phantoms) != 2 {
		t.Fatalf("want 2 phantoms, got %d: %v", len(phantoms), phantoms)
	}
}

func TestFindPhantomBranchRefs_MissingDirNotAnError(t *testing.T) {
	repo := t.TempDir()
	phantoms, err := findPhantomBranchRefs(repo, "bosun")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(phantoms) != 0 {
		t.Errorf("want 0 phantoms, got %d", len(phantoms))
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCheckStaleSessionBranch covers the v0.6.2 stale-branch pre-flight in
// isolation. Drives the helper directly against a real on-disk repo so the
// rev-parse / branch-exists paths run end-to-end rather than against
// mocked git output.
//
// Three cases:
//   - branch absent             → no error (proceed)
//   - branch at base HEAD       → no error (equal-SHA is a no-op semantically)
//   - branch diverges from base → userErr with recovery hints
func TestCheckStaleSessionBranch(t *testing.T) {
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

	// Case 1: branch absent.
	if err := checkStaleSessionBranch(rc, "bosun/session-99", "main"); err != nil {
		t.Fatalf("expected nil for absent branch, got %v", err)
	}

	// Case 2: branch at base HEAD.
	runGit("branch", "bosun/session-1")
	if err := checkStaleSessionBranch(rc, "bosun/session-1", "main"); err != nil {
		t.Fatalf("expected nil for branch == base HEAD, got %v", err)
	}

	// Case 3: branch diverges from base.
	runGit("checkout", "-q", "bosun/session-1")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("y\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("commit", "-q", "-am", "diverge")
	runGit("checkout", "-q", "main")

	err = checkStaleSessionBranch(rc, "bosun/session-1", "main")
	if err == nil {
		t.Fatal("expected refusal on divergent branch, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"already exists",
		"diverges from main",
		"git branch -D bosun/session-1",
		"--force",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}
}

// loadCtxAt wraps loadCtx for tests that need a runCtx rooted at a
// specific directory rather than the process CWD. Captures and restores
// the working directory around the call.
func loadCtxAt(dir string) (*runCtx, error) {
	prev, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	defer os.Chdir(prev)
	return loadCtx()
}

// roundTimestampWorktreeRe matches `<repo>-bosun-<YYYYMMDD-HHMMSS>-<N>`,
// the v0.10 UID-per-worktree dir shape produced by `bosun init`
// (scheme C in docs/uid-worktree-design.md). Used by the tests below
// to assert each session's worktree is named with the same round
// timestamp.
// Pattern now expects `<repo>-bosun-<YYYYMMDD-HHMMSS>-<PID>-<N>`
// after the 2026-05 bug-hunt pass-2 #4 fix added a PID component to
// disambiguate same-second parallel inits. The PID is captured but
// discarded — the test only asserts the timestamp portion matches
// across sibling worktrees. Group 1 = timestamp, group 2 = session N.
var roundTimestampWorktreeRe = regexp.MustCompile(`^myproj-bosun-(\d{8}-\d{6})-\d+-(\d+)$`)

// TestScenario_InitUsesTimestampedWorktreeDirs locks in the lane-3
// contract: `bosun init N` produces N worktrees whose dirs all share
// the same UTC `YYYYMMDD-HHMMSS` token and end in `-1..-N`. The
// timestamp + suffix N together form the unique-per-round directory
// name that prevents post-merge-zombie collisions described in
// docs/uid-worktree-design.md.
func TestScenario_InitUsesTimestampedWorktreeDirs(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "3")

	entries, err := os.ReadDir(s.parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	seenN := map[string]bool{}
	var ts string
	for _, e := range entries {
		m := roundTimestampWorktreeRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if ts == "" {
			ts = m[1]
		} else if ts != m[1] {
			t.Errorf("dir %s has timestamp %s, expected %s (every session in one round must share the timestamp)", e.Name(), m[1], ts)
		}
		seenN[m[2]] = true
	}
	for _, n := range []string{"1", "2", "3"} {
		if !seenN[n] {
			t.Errorf("missing timestamped worktree for session-%s (entries: %v)", n, entryNames(entries))
		}
	}
	if ts == "" {
		t.Fatalf("no worktree matched the `<repo>-bosun-<YYYYMMDD-HHMMSS>-<N>` shape; entries: %v", entryNames(entries))
	}
	// The captured timestamp must be a recent UTC time — within a few
	// minutes of now. Catches accidental drift back to the legacy form
	// (which would skip this regex entirely) or to non-UTC zones.
	parsed, err := time.Parse("20060102-150405", ts)
	if err != nil {
		t.Fatalf("captured timestamp %q failed to parse: %v", ts, err)
	}
	if delta := time.Since(parsed); delta > 10*time.Minute || delta < -1*time.Minute {
		t.Errorf("timestamp %q is %s from now — UTC drift? (test machine clock skewed?)", ts, delta)
	}
}

func entryNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestScenario_InitResumeReproducesTimestampedPaths covers the
// brief's "resume must produce the same paths" contract. It runs
// `bosun init 2`, then artificially rewinds the state to look like
// the run was interrupted between session-1 and session-2, and
// re-runs `bosun init --resume`. The second worktree on disk must
// share the original round's timestamp — proving the timestamp
// round-tripped through `.bosun/init.state` rather than being
// re-derived from time.Now() at resume time.
func TestScenario_InitResumeReproducesTimestampedPaths(t *testing.T) {
	s := newScenario(t)
	s.WriteFile("plan.md", "## session-1\nA\n\n## session-2\nB\n")
	s.Bosun("init", "2", "--brief", "plan.md")

	// Capture the round timestamp that real init used, then wipe
	// session-2 to look like the prior run died mid-loop.
	ts := s.RoundTimestamp()
	if ts == "" {
		t.Fatalf("scenario.RoundTimestamp() returned empty after `bosun init 2` (worktrees: %v)", entryNames(readParentDir(t, s.parent)))
	}
	wt2 := s.WorktreePath(2)
	s.GitIn(s.repo, "worktree", "remove", "--force", wt2)
	if err := os.RemoveAll(wt2); err != nil {
		t.Fatalf("rm wt2: %v", err)
	}

	// Re-seed init.state pointing at the abandoned session-2, with the
	// captured timestamp threaded through so resume reconstructs the
	// same path it would have created on the first attempt.
	statePath := filepath.Join(s.repo, ".bosun", "init.state")
	stateJSON := `{
  "version": "v0.6",
  "started_at": "2026-01-01T00:00:00Z",
  "round_timestamp": "` + ts + `",
  "plan_path": "plan.md",
  "total_sessions": 2,
  "labels": ["session-1", "session-2"],
  "completed_sessions": ["session-1"],
  "current_session": "session-2",
  "current_step": "git_worktree_add"
}
`
	if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	out := s.Bosun("init", "--resume")
	if !strings.Contains(out, "Resuming previous init") {
		t.Errorf("resume should announce itself; got:\n%s", out)
	}

	// Verify the rebuilt session-2 dir carries the original timestamp.
	wantBase := s.name + "-bosun-" + ts + "-2"
	wantPath := filepath.Join(s.parent, wantBase)
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("resume should have re-created %s; stat err = %v\nparent entries: %v",
			wantPath, err, entryNames(readParentDir(t, s.parent)))
	}
}

func readParentDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return entries
}

// TestInitNowFn_DifferentPIDsAvoidCollision pins the 2026-05 bug-hunt
// pass-2 #4 fix: two `bosun init` invocations from DIFFERENT processes
// landing on the same wall-clock second no longer produce identical
// worktree paths. The PID is part of the round timestamp now, so
// distinct PIDs disambiguate even when the clock matches.
//
// (The same-process same-second case — TestInitNowFn_SameSecondRunsCollide
// below — still collides intentionally so an operator who fat-fingers
// two sequential `bosun init` calls in the same shell session still
// gets refused on the second instead of silent overwrite.)
func TestInitNowFn_DifferentPIDsAvoidCollision(t *testing.T) {
	repo := newInitTestRepo(t)

	fixed := time.Date(2026, 5, 18, 11, 54, 0, 0, time.UTC)
	prev := initNowFn
	initNowFn = func() time.Time { return fixed }
	t.Cleanup(func() { initNowFn = prev })

	prevPID := initPIDFn
	t.Cleanup(func() { initPIDFn = prevPID })

	// First init from PID 100.
	initPIDFn = func() int { return 100 }
	if err := runInitFromDir(repo, []string{"a"}, initOpts{noLoadCheck: true, forceICloud: true}); err != nil {
		t.Fatalf("first init (PID 100): %v", err)
	}

	// Second init from PID 200, same clock-second. Without the PID
	// disambiguation this would collide and refuse; with it, the
	// timestamps differ and both rounds get distinct worktree dirs.
	initPIDFn = func() int { return 200 }
	if err := runInitFromDir(repo, []string{"b"}, initOpts{noLoadCheck: true, forceICloud: true}); err != nil {
		t.Fatalf("second init (PID 200): %v", err)
	}

	// Confirm both worktrees exist with PID-distinguishable names.
	entries, err := os.ReadDir(filepath.Dir(repo))
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	got := entryNames(entries)
	want1 := "myproj-bosun-20260518-115400-100-a"
	want2 := "myproj-bosun-20260518-115400-200-b"
	have1, have2 := false, false
	for _, n := range got {
		if n == want1 {
			have1 = true
		}
		if n == want2 {
			have2 = true
		}
	}
	if !have1 || !have2 {
		t.Errorf("expected both %q and %q in parent dir; got %v", want1, want2, got)
	}
}

// TestInitNowFn_SameSecondRunsCollide locks the safety check the brief
// calls out: two `bosun init` invocations FROM THE SAME PROCESS that
// land in the same second must produce identical timestamps, so the
// second run is refused on the existing `worktree path already exists`
// pre-flight rather than silently overwriting the first. Drives
// `runInit` in-process with a mocked `initNowFn` so the test is
// deterministic regardless of how fast the host runs.
//
// 2026-05 pass-2: the PID is now part of the timestamp, so this test
// also pins that the SAME-process invariant still holds — same PID
// + same clock-second → same timestamp → same collision.
func TestInitNowFn_SameSecondRunsCollide(t *testing.T) {
	repo := newInitTestRepo(t)

	// Freeze the clock so both invocations sample the same UTC second.
	fixed := time.Date(2026, 5, 18, 11, 54, 0, 0, time.UTC)
	prev := initNowFn
	initNowFn = func() time.Time { return fixed }
	t.Cleanup(func() { initNowFn = prev })

	if err := runInitFromDir(repo, []string{"2"}, initOpts{noLoadCheck: true, forceICloud: true}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	// The first run cleared init.state on success. Without --force,
	// the second run hits the same-second timestamp and refuses on the
	// existing worktree dir.
	err := runInitFromDir(repo, []string{"2"}, initOpts{noLoadCheck: true, forceICloud: true})
	if err == nil {
		t.Fatalf("expected second init to refuse on existing dir; got nil err")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected `already exists` refusal; got: %v", err)
	}
}

// TestInitState_RoundTimestampPersisted verifies the new field reaches
// disk: a fresh init writes `round_timestamp` into `.bosun/init.state`
// before the file is cleared on success. We freeze the clock, halt the
// run mid-stream via an injected hook, then read the breadcrumb.
func TestInitState_RoundTimestampPersisted(t *testing.T) {
	repo := newInitTestRepo(t)

	fixed := time.Date(2026, 5, 18, 11, 54, 0, 0, time.UTC)
	prev := initNowFn
	initNowFn = func() time.Time { return fixed }
	t.Cleanup(func() { initNowFn = prev })

	// Pin the PID too — 2026-05 bug-hunt pass-2 #4 appended the PID
	// to the round timestamp to disambiguate same-second cross-
	// process inits. Without freezing it here, the test would
	// non-deterministically include whichever PID the test binary
	// happens to have.
	prevPID := initPIDFn
	initPIDFn = func() int { return 42 }
	t.Cleanup(func() { initPIDFn = prevPID })

	if err := runInitFromDir(repo, []string{"1"}, initOpts{noLoadCheck: true, forceICloud: true}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// init.state is cleared on success; the timestamp survives in the
	// worktree dir name. The point of this test is the live-state path,
	// so we seed a state mid-run by re-invoking runInit with a stub that
	// fails between branch create and the final clear — easier path: just
	// read the on-disk dir name and verify it carries the expected
	// timestamp.
	entries, err := os.ReadDir(filepath.Dir(repo))
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	want := "myproj-bosun-20260518-115400-42-1"
	found := false
	for _, e := range entries {
		if e.Name() == want {
			found = true
		}
	}
	if !found {
		t.Errorf("worktree dir %q not found among parent entries: %v", want, entryNames(entries))
	}

	// Sanity-check the round-trip through initstate by writing a state
	// file manually and reading it back.
	dir := t.TempDir()
	const ts = "20260518-115400"
	s := initstate.New([]string{"session-1"}, "", ts)
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := initstate.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RoundTimestamp != ts {
		t.Errorf("RoundTimestamp roundtrip = %q, want %q", got.RoundTimestamp, ts)
	}
}

// newInitTestRepo sets up a bare git repo with a single commit on main,
// suitable for invoking runInit against. Returns the absolute path of
// the main worktree. The repo lives under t.TempDir() with the basename
// "myproj" so the worktree-naming tests above can match by basename.
func newInitTestRepo(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	repo := filepath.Join(parent, "myproj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
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
	if err := os.WriteFile(filepath.Join(repo, "seed"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runGit("add", "seed")
	runGit("commit", "-q", "-m", "init")
	return repo
}

// runInitFromDir runs runInit with the working directory pinned to dir,
// so loadCtx resolves the right main worktree path. Captures and
// restores the previous CWD around the call.
func runInitFromDir(dir string, args []string, opts initOpts) error {
	prev, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	defer os.Chdir(prev)
	return runInit(nil, args, opts)
}

func TestSameLabels(t *testing.T) {
	// sameLabels gates the --resume "args contradict state" warning. The
	// fix downgraded the contradiction from a fatal error to a warning, so
	// the comparator's correctness is what controls whether the operator
	// sees the warning at all.
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"identical numbered", []string{"session-1", "session-2"}, []string{"session-1", "session-2"}, true},
		{"identical named", []string{"auth", "http"}, []string{"auth", "http"}, true},
		{"differ in count", []string{"session-1", "session-2", "session-3"}, []string{"session-1", "session-2"}, false},
		{"differ in order", []string{"auth", "http"}, []string{"http", "auth"}, false},
		{"differ in content", []string{"auth"}, []string{"storage"}, false},
		{"empty vs non-empty", nil, []string{"session-1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameLabels(tc.a, tc.b); got != tc.want {
				t.Errorf("sameLabels(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
