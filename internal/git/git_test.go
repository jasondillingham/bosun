package git

import (
	"context"
	"errors"
	"reflect"
	"testing"
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
