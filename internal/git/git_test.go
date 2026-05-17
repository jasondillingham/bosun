package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRunner returns canned output for matching arg sequences.
type fakeRunner struct {
	t        *testing.T
	matchers []runMatcher
	calls    []runCall
}

type runMatcher struct {
	args   []string
	stdout string
	stderr string
	err    error
}

type runCall struct {
	dir  string
	args []string
}

func (f *fakeRunner) Run(_ context.Context, dir string, args ...string) (string, string, error) {
	f.calls = append(f.calls, runCall{dir: dir, args: append([]string(nil), args...)})
	for _, m := range f.matchers {
		if reflect.DeepEqual(m.args, args) {
			return m.stdout, m.stderr, m.err
		}
	}
	f.t.Fatalf("unexpected git call: %v (dir=%q)", args, dir)
	return "", "", nil
}

func newFakeClient(t *testing.T, matchers ...runMatcher) (*Client, *fakeRunner) {
	r := &fakeRunner{t: t, matchers: matchers}
	return &Client{Runner: r}, r
}

func TestParseWorktreeList(t *testing.T) {
	in := "worktree /repo/main\nHEAD abc123\nbranch refs/heads/main\n\nworktree /repo-bosun-1\nHEAD def456\nbranch refs/heads/bosun/session-1\n\nworktree /repo-bare\nbare\n\nworktree /repo-detached\nHEAD aaa111\ndetached\n"
	got := parseWorktreeList(in)
	want := []Worktree{
		{Path: "/repo/main", HEAD: "abc123", Branch: "refs/heads/main"},
		{Path: "/repo-bosun-1", HEAD: "def456", Branch: "refs/heads/bosun/session-1"},
		{Path: "/repo-bare", Bare: true},
		{Path: "/repo-detached", HEAD: "aaa111", Branch: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseWorktreeList mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestParsePorcelainStatus(t *testing.T) {
	in := " M internal/foo.go\nA  internal/bar.go\n?? build/out.txt\nR  old.go -> new.go\n"
	got := parsePorcelainStatus(in)
	want := []PorcelainStatusLine{
		{XY: " M", Path: "internal/foo.go"},
		{XY: "A ", Path: "internal/bar.go"},
		{XY: "??", Path: "build/out.txt"},
		{XY: "R ", Path: "new.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePorcelainStatus mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestCurrentBranch_Detached(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"rev-parse", "--abbrev-ref", "HEAD"},
		stdout: "HEAD\n",
	})
	got, err := c.CurrentBranch(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("CurrentBranch detached = %q, want empty", got)
	}
}

func TestCurrentBranch_Normal(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"rev-parse", "--abbrev-ref", "HEAD"},
		stdout: "main\n",
	})
	got, err := c.CurrentBranch(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("CurrentBranch = %q, want main", got)
	}
}

func TestRevListCount(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"rev-list", "--count", "main..HEAD"},
		stdout: "3\n",
	})
	n, err := c.RevListCount(context.Background(), "/wt", "main")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("RevListCount = %d, want 3", n)
	}
}

func TestDirtyCount_FiltersUntracked(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"status", "--porcelain"},
		stdout: " M a.go\n?? b.go\nA  c.go\n",
	})
	n, err := c.DirtyCount(context.Background(), "/wt")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("DirtyCount = %d, want 2 (untracked excluded)", n)
	}
}

func TestLastCommit_NoneAhead(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"rev-list", "--count", "main..HEAD"},
		stdout: "0\n",
	})
	got, err := c.LastCommit(context.Background(), "/wt", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("LastCommit with 0 ahead = %+v, want nil", got)
	}
}

func TestLastCommit_Parses(t *testing.T) {
	c, _ := newFakeClient(t,
		runMatcher{
			args:   []string{"rev-list", "--count", "main..HEAD"},
			stdout: "2\n",
		},
		runMatcher{
			args:   []string{"log", "-1", "--format=%h|%ct|%ar|%s"},
			stdout: "abc1234|1700000000|2 hours ago|wire up handler\n",
		},
	)
	got, err := c.LastCommit(context.Background(), "/wt", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if got.ShortSHA != "abc1234" || got.Relative != "2 hours ago" || got.Subject != "wire up handler" || got.Unix != 1700000000 {
		t.Fatalf("LastCommit unexpected: %+v", got)
	}
}

func TestBranchExists_Found(t *testing.T) {
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"show-ref", "--verify", "--quiet", "refs/heads/main"},
		stdout: "",
		err:    nil,
	})
	ok, err := c.BranchExists(context.Background(), "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("BranchExists = false, want true")
	}
}

func TestRun_WrapsError(t *testing.T) {
	wantErr := errors.New("boom")
	c, _ := newFakeClient(t, runMatcher{
		args:   []string{"status", "--porcelain"},
		stderr: "fatal: not a git repository",
		err:    wantErr,
	})
	_, err := c.Status(context.Background(), "/nope")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error chain does not wrap original: %v", err)
	}
}

// blockingRunner sleeps until the caller's ctx is cancelled, then surfaces
// ctx.Err(). It mimics the silent-hang shape we care about: a `git`
// subprocess that produces no output and refuses to exit until the kernel
// reaps it. The fake doesn't actually fork — it just demonstrates that
// Client.run honors the timeout it installs on the child context.
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ ...string) (string, string, error) {
	<-ctx.Done()
	return "", "", ctx.Err()
}

func TestRun_TimeoutReturnsStructuredError(t *testing.T) {
	c := &Client{Runner: blockingRunner{}, Timeout: 50 * time.Millisecond}
	start := time.Now()
	_, err := c.Status(context.Background(), "/repo")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Must surface as *TimeoutError, not a bare context.DeadlineExceeded
	// or a generic "git status: ..." string. Operator-facing error.
	var to *TimeoutError
	if !errors.As(err, &to) {
		t.Fatalf("error is not *TimeoutError: %v (type %T)", err, err)
	}
	if to.Timeout != 50*time.Millisecond {
		t.Errorf("TimeoutError.Timeout = %v, want 50ms", to.Timeout)
	}
	if !strings.HasPrefix(to.Op, "status") {
		t.Errorf("TimeoutError.Op = %q, want it to start with the git subcommand", to.Op)
	}
	// Sanity: the timeout actually fired within a reasonable window. A
	// 500ms ceiling is generous enough to survive a slow CI runner but
	// tight enough to fail loudly if the timeout doesn't fire at all.
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, expected timeout near 50ms", elapsed)
	}
	// Error string should be human-readable, naming both op and duration.
	msg := err.Error()
	if !strings.Contains(msg, "timed out") || !strings.Contains(msg, "50ms") {
		t.Errorf("error message %q missing 'timed out' / duration", msg)
	}
}

func TestRun_ZeroTimeoutDisablesDeadline(t *testing.T) {
	// Timeout = 0 means "no per-op timeout" — caller passes their own ctx.
	// Verify by passing a ctx that has its own short deadline: the parent
	// ctx, not our Client.Timeout, should drive cancellation. The error
	// must NOT be a *TimeoutError because the deadline isn't ours.
	c := &Client{Runner: blockingRunner{}, Timeout: 0}
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Status(parent, "/repo")
	if err == nil {
		t.Fatal("expected error from parent ctx deadline")
	}
	var to *TimeoutError
	if errors.As(err, &to) {
		t.Fatalf("unexpected *TimeoutError when parent ctx caused cancellation: %v", err)
	}
}

// TestAddWorktree_TimeoutFiresWithRecoveryHint reproduces the v0.6 trial
// finding: `git worktree add` hung indefinitely with no per-op cap.
// It shims a fake `git` binary onto PATH that sleeps 60s on `worktree add`,
// then asserts that AddWorktree returns within a few seconds with a
// *TimeoutError whose message points the operator at `bosun init --resume`.
//
// Against pre-fix code the timeout would fire but the error message would
// lack the recovery hint — operator hits the timeout, sees "timed out
// after Xs", and has no signal about what to do next.
func TestAddWorktree_TimeoutFiresWithRecoveryHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Shim uses POSIX `sh`; the timeout-wrapping logic itself is the
		// same on Windows, but this particular regression harness assumes
		// a Unix shell on PATH. The in-memory blockingRunner tests above
		// already cover the timeout-wrapping path cross-platform.
		t.Skip("shim uses POSIX sh; Windows skipped")
	}

	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "git")
	// Sleep forever on `worktree add`; succeed quickly on anything else
	// so the shim doesn't accidentally hang unrelated calls.
	shim := "#!/bin/sh\n" +
		"if [ \"$1\" = \"worktree\" ] && [ \"$2\" = \"add\" ]; then\n" +
		"    sleep 60\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Construct Client directly (skipping New()) so we don't pick up the
	// production worktree-add-specific default — we want this single
	// 2s Timeout to bound AddWorktree end-to-end.
	c := &Client{Runner: ExecRunner{}, Timeout: 2 * time.Second}

	start := time.Now()
	err := c.AddWorktree(context.Background(), "", filepath.Join(t.TempDir(), "fake-wt"), "main")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error from sleep-60s shim, got nil")
	}
	// 5s ceiling: 2s timeout + headroom for SIGKILL delivery on a slow runner.
	// A regression that drops the timeout would push this to ~60s.
	if elapsed > 5*time.Second {
		t.Fatalf("AddWorktree returned in %v; expected ~2s — worktree-add timeout did not fire", elapsed)
	}
	var to *TimeoutError
	if !errors.As(err, &to) {
		t.Fatalf("error is not *TimeoutError: %v (type %T)", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "timed out") {
		t.Errorf("error message %q missing 'timed out'", msg)
	}
	if !strings.Contains(msg, "bosun init --resume") {
		t.Errorf("error message must point at the recovery path; got:\n  %s", msg)
	}
}

// TestNew_WorktreeAddTimeoutDefault locks in the bumped default: worktree-add
// gets a longer cap than other git ops because creating a worktree under
// fsync pressure (APFS, Spotlight reindex) is legitimately slow. 30s fires
// spuriously; 120s is the floor per the v0.6 trial findings.
func TestNew_WorktreeAddTimeoutDefault(t *testing.T) {
	c := New()
	if c.WorktreeAddTimeout != DefaultWorktreeAddTimeout {
		t.Fatalf("New() WorktreeAddTimeout = %v, want %v", c.WorktreeAddTimeout, DefaultWorktreeAddTimeout)
	}
	if DefaultWorktreeAddTimeout <= DefaultOpTimeout {
		t.Fatalf("DefaultWorktreeAddTimeout (%v) must exceed DefaultOpTimeout (%v) — worktree-add is the legitimately-slow case", DefaultWorktreeAddTimeout, DefaultOpTimeout)
	}
}

// TestWorktreeAddTimeout_OperatorOverrideWins documents that an operator
// who raises Timeout above the WorktreeAddTimeout floor still gets the
// longer cap they asked for — the floor doesn't clamp them down.
func TestWorktreeAddTimeout_OperatorOverrideWins(t *testing.T) {
	c := New()
	c.Timeout = 300 * time.Second
	if got := c.worktreeAddTimeout(); got != 300*time.Second {
		t.Fatalf("worktreeAddTimeout with Timeout=300s = %v, want 300s (operator override should win)", got)
	}
}

// TestClient_AllMethodsHonorTimeout is the regression-proof against future
// "I'll just call exec.CommandContext here" backsliding. Every public
// method on *Client that shells out to git must surface a *TimeoutError
// within Timeout+1s when git hangs — otherwise a new method silently
// reintroduces the v0.6.1 fsck-bypass shape.
//
// We shim a single fake `git` onto PATH that sleeps 60s on any
// invocation, then exercise each method with a tight 1s Timeout. The
// shimDir path lets every t.Run share one shim; PATH and shim payload
// stay constant for the whole test.
func TestClient_AllMethodsHonorTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim uses POSIX sh; Windows skipped")
	}

	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "git")
	shim := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Cwd must exist on disk — otherwise exec fails with `chdir: no such
	// file` *before* invoking the shim, and the test never gets near the
	// sleep that would exercise the timeout. A bare TempDir is enough;
	// the shim ignores its own working directory.
	workDir := t.TempDir()
	// Some methods (RemoveWorktree.chmodWritableTree) stat the target
	// path before invoking git; create it so the pre-pass doesn't shortcut.
	wtDir := filepath.Join(workDir, "wt")
	if err := os.Mkdir(wtDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree placeholder: %v", err)
	}

	// Budget: timeout + execPipeDrainTimeout (the WaitDelay we install on
	// every child) + a slack window for OS scheduling. With timeout=500ms
	// and pipe-drain=1s the wall-clock cost per row is ~1.5s; the 2.5s
	// ceiling tolerates +1s of jitter on slow CI runners while still
	// failing loudly if a method returns in ~60s (the un-bounded case).
	const timeout = 500 * time.Millisecond
	const ceiling = 2500 * time.Millisecond

	// Each row exercises one Client method. ListWorktrees swallows parse
	// errors via parseWorktreeList — but we only care that the method
	// returns within the budget with a TimeoutError, which it must
	// because c.run wraps every call. Same logic for RevListCount
	// (Atoi-parses post-timeout output we never reach).
	cases := []struct {
		name string
		fn   func(c *Client) error
	}{
		{"CreateBranch", func(c *Client) error {
			return c.CreateBranch(context.Background(), workDir, "topic", "main")
		}},
		{"DeleteBranch", func(c *Client) error {
			return c.DeleteBranch(context.Background(), workDir, "topic", false)
		}},
		{"AddWorktree", func(c *Client) error {
			// AddWorktree uses its own (longer) timeout via worktreeAddTimeout();
			// setting WorktreeAddTimeout=timeout here ensures the test exits in
			// the same budget as the other methods.
			return c.AddWorktree(context.Background(), workDir, wtDir, "main")
		}},
		{"RemoveWorktree", func(c *Client) error {
			return c.RemoveWorktree(context.Background(), workDir, wtDir, false)
		}},
		{"ListWorktrees", func(c *Client) error {
			_, err := c.ListWorktrees(context.Background(), workDir)
			return err
		}},
		{"RevListCount", func(c *Client) error {
			_, err := c.RevListCount(context.Background(), workDir, "main")
			return err
		}},
		{"FsckWorktree", func(c *Client) error {
			return c.FsckWorktree(context.Background(), workDir)
		}},
		{"RevParseHEAD", func(c *Client) error {
			_, err := c.RevParseHEAD(context.Background(), workDir)
			return err
		}},
		{"ResetHard", func(c *Client) error {
			return c.ResetHard(context.Background(), workDir, "deadbeef")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Construct a fresh client per row so AddWorktree's worktreeAddTimeout
			// floor can be lowered to the shared budget for this test.
			c := &Client{Runner: ExecRunner{}, Timeout: timeout, WorktreeAddTimeout: timeout}
			start := time.Now()
			err := tc.fn(c)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("%s: expected timeout error from sleep-60s shim, got nil", tc.name)
			}
			if elapsed > ceiling {
				t.Fatalf("%s: returned in %v; expected ~%v — timeout did not fire (bypass regression?)", tc.name, elapsed, timeout)
			}
			var to *TimeoutError
			if !errors.As(err, &to) {
				t.Fatalf("%s: error is not *TimeoutError: %v (type %T) — caller bypassed c.run", tc.name, err, err)
			}
		})
	}
}

func TestSetTimeout(t *testing.T) {
	c := New()
	if c.Timeout != DefaultOpTimeout {
		t.Fatalf("New() timeout = %v, want %v", c.Timeout, DefaultOpTimeout)
	}
	c.SetTimeout(2 * time.Minute)
	if c.Timeout != 2*time.Minute {
		t.Fatalf("after SetTimeout, c.Timeout = %v, want 2m", c.Timeout)
	}
	c.SetTimeout(0)
	if c.Timeout != 0 {
		t.Fatalf("after SetTimeout(0), c.Timeout = %v, want 0 (disabled)", c.Timeout)
	}
}
