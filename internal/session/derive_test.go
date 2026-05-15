package session

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
)

// fakeRunner implements git.Runner with simple arg-matching.
type fakeRunner struct {
	t        *testing.T
	worktree string // stdout for worktree list --porcelain
	revCount map[string]string
	status   map[string]string
	log      map[string]string
}

func (f *fakeRunner) Run(_ context.Context, dir string, args ...string) (string, string, error) {
	joined := strings.Join(args, " ")
	switch {
	case joined == "worktree list --porcelain":
		return f.worktree, "", nil
	case strings.HasPrefix(joined, "rev-list --count "):
		if v, ok := f.revCount[dir]; ok {
			return v, "", nil
		}
		return "0\n", "", nil
	case joined == "status --porcelain":
		if v, ok := f.status[dir]; ok {
			return v, "", nil
		}
		return "", "", nil
	case joined == "log -1 --format=%h|%ct|%ar|%s":
		if v, ok := f.log[dir]; ok {
			return v, "", nil
		}
		return "", "", nil
	}
	f.t.Fatalf("unexpected git call: %v (dir=%q)", args, dir)
	return "", "", nil
}

type fakeState struct{ states map[string]State }

func (f *fakeState) Read(_, name string) (State, string, error) {
	if s, ok := f.states[name]; ok {
		return s, "", nil
	}
	return StateWorking, "", nil
}

type fakeClaims struct{ counts map[string]int }

func (f *fakeClaims) CountFor(_ string, name string) (int, error) {
	return f.counts[name], nil
}

func TestDerive_SortsAndFilters(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-2",
			"HEAD bbb",
			"branch refs/heads/bosun/session-2",
			"",
			"worktree /repo-bosun-1",
			"HEAD ccc",
			"branch refs/heads/bosun/session-1",
			"",
			"worktree /someones-other-worktree",
			"HEAD ddd",
			"branch refs/heads/feature/x",
			"",
		}, "\n"),
		revCount: map[string]string{
			"/repo-bosun-1": "2\n",
			"/repo-bosun-2": "0\n",
		},
		status: map[string]string{
			"/repo-bosun-1": " M a.go\n?? b.txt\n",
			"/repo-bosun-2": "",
		},
		log: map[string]string{
			"/repo-bosun-1": "abc1234|1700000000|2 hours ago|wire auth\n",
		},
	}
	c := &git.Client{Runner: r}
	cfg := config.Defaults()
	state := &fakeState{states: map[string]State{"session-1": StateDone}}
	claims := &fakeClaims{counts: map[string]int{"session-1": 3, "session-2": 0}}

	got, err := Derive(context.Background(), c, cfg, "/repo", state, claims)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (feature/x branch should be filtered out)", len(got))
	}
	// Sort order: 1 before 2.
	if got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("session order wrong: %+v", got)
	}
	// session-1 has 2 ahead, 1 dirty (untracked filtered), 3 claimed, DONE state, last commit subject "wire auth".
	s1 := got[0]
	if s1.Ahead != 2 {
		t.Errorf("session-1 Ahead = %d, want 2", s1.Ahead)
	}
	if s1.Dirty != 1 {
		t.Errorf("session-1 Dirty = %d, want 1", s1.Dirty)
	}
	if s1.Claimed != 3 {
		t.Errorf("session-1 Claimed = %d, want 3", s1.Claimed)
	}
	if s1.State != StateDone {
		t.Errorf("session-1 State = %s, want DONE", s1.State)
	}
	if s1.Last == nil || s1.Last.Subject != "wire auth" {
		t.Errorf("session-1 Last = %+v", s1.Last)
	}
	if s1.Path != "/repo-bosun-1" {
		t.Errorf("session-1 Path = %q", s1.Path)
	}
	// session-2 has 0 ahead, no last commit, WORKING state.
	s2 := got[1]
	if s2.Ahead != 0 || s2.Dirty != 0 || s2.Claimed != 0 {
		t.Errorf("session-2 unexpected: %+v", s2)
	}
	if s2.Last != nil {
		t.Errorf("session-2 Last should be nil, got %+v", s2.Last)
	}
	if s2.State != StateWorking {
		t.Errorf("session-2 State = %s, want WORKING", s2.State)
	}
}

func TestDerive_EmptyWhenNoBosunBranches(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: "worktree /repo\nHEAD aaa\nbranch refs/heads/main\n\n",
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{}, &fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Session(nil)) && len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}
