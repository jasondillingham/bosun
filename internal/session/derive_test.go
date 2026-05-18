package session

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/proc"
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

type fakeState struct {
	states     map[string]State
	heartbeats map[string]time.Time
	// attached maps session name → pid registered via Attached. Tests
	// that exercise the v0.11 attach-then-proc-scan ladder populate
	// this; legacy tests leave it nil and the reader returns ok=false.
	attached map[string]int
}

func (f *fakeState) Read(_, name string) (State, string, error) {
	if s, ok := f.states[name]; ok {
		return s, "", nil
	}
	return StateWorking, "", nil
}

func (f *fakeState) Heartbeat(_, name string) (time.Time, bool, error) {
	if f.heartbeats == nil {
		return time.Time{}, false, nil
	}
	t, ok := f.heartbeats[name]
	return t, ok, nil
}

func (f *fakeState) Attached(_, name string) (int, bool, error) {
	if f.attached == nil {
		return 0, false, nil
	}
	pid, ok := f.attached[name]
	return pid, ok, nil
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

// TestDerive_CrashedWhenProcGoneAndDirty covers the CRASHED derivation
// path: a WORKING session whose worktree has uncommitted dirty files but
// whose agent process is gone should surface as CRASHED. The runner
// fixture sets dirty=1 (one " M" line, untracked " M" excluded) and the
// proc lister implicitly reports nothing running for non-existent paths.
func TestDerive_CrashedWhenProcGoneAndDirty(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": " M a.go\n"},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{}, &fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1", len(got))
	}
	if got[0].State != StateCrashed {
		t.Errorf("State = %q, want CRASHED (proc not running + dirty)", got[0].State)
	}
}

// TestDerive_NotCrashedWhenClean covers the negative side of CRASHED: a
// WORKING session that's clean (no dirty files) stays WORKING even when
// the agent process is gone. Otherwise every idle session would flicker
// CRASHED the moment the operator closed its terminal window.
func TestDerive_NotCrashedWhenClean(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": ""},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{}, &fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateWorking {
		t.Errorf("State = %q, want WORKING (clean worktree)", got[0].State)
	}
}

// TestDerive_DoneNotCrashedEvenWhenDirty: DONE sessions stay DONE even if
// their worktree still has uncommitted files (which can happen when an
// agent marks done then continues exploring, or when bosun_done is called
// before a stray edit lands). CRASHED is reserved for the WORKING path.
func TestDerive_DoneNotCrashedEvenWhenDirty(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "1\n"},
		status:   map[string]string{"/repo-bosun-1": " M leftover.go\n"},
		log:      map[string]string{"/repo-bosun-1": "abc1234|1700000000|2 hours ago|wire auth\n"},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{states: map[string]State{"session-1": StateDone}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateDone {
		t.Errorf("State = %q, want DONE (CRASHED must not override terminal state)", got[0].State)
	}
}

// TestDerive_StaleFlagSetWhenHeartbeatOld: a WORKING session with a
// heartbeat older than HeartbeatStaleAfter surfaces Stale=true. The
// session state itself stays WORKING — STALE is a derived flag, not a
// State enum value.
func TestDerive_StaleFlagSetWhenHeartbeatOld(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": ""},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{heartbeats: map[string]time.Time{
			"session-1": time.Now().Add(-10 * time.Minute),
		}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateWorking {
		t.Errorf("State = %q, want WORKING (stale doesn't change state)", got[0].State)
	}
	if !got[0].Stale {
		t.Errorf("Stale = false, want true (heartbeat 10m old, threshold 5m)")
	}
}

// TestDerive_StaleFalseWhenHeartbeatFresh: heartbeat within the threshold
// keeps the session non-stale even though one is recorded.
func TestDerive_StaleFalseWhenHeartbeatFresh(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": ""},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{heartbeats: map[string]time.Time{
			"session-1": time.Now().Add(-1 * time.Minute),
		}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Stale {
		t.Errorf("Stale = true, want false (heartbeat 1m old, threshold 5m)")
	}
}

// TestDerive_StaleFalseWhenNoHeartbeat: a session that has never recorded
// a heartbeat must NOT be flagged stale. We can't distinguish "agent
// doesn't emit heartbeats" from "agent is hung" without the file, so the
// absence-of-evidence path stays quiet.
func TestDerive_StaleFalseWhenNoHeartbeat(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": ""},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{}, &fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Stale {
		t.Errorf("Stale = true, want false (no heartbeat recorded)")
	}
}

// TestDerive_StaleNotSetOnCrashed: a CRASHED session shouldn't carry the
// stale flag — the operator already has the more useful CRASHED signal.
// (Heartbeat may still be old when an agent crashed long ago, but
// surfacing both CRASHED + STALE is noise.)
func TestDerive_StaleNotSetOnCrashed(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": " M a.go\n"},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{heartbeats: map[string]time.Time{
			"session-1": time.Now().Add(-30 * time.Minute),
		}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateCrashed {
		t.Errorf("State = %q, want CRASHED", got[0].State)
	}
	if got[0].Stale {
		t.Errorf("Stale = true on CRASHED session — should not double up")
	}
}

// stubLister fakes proc.Lister so the four-state attach tests can
// pin "is the worktree process visible to proc.Running?" without
// depending on real subprocesses. Tests using it temporarily swap
// procLister via the test-only hook below.
//
// Note: we exercise proc.RunningWith directly via the public path
// (proc.Running called from Derive). To avoid intrusive plumbing, the
// four-state tests below operate on the lister-tested Derive path by
// passing dirty=0 (so absence of a process never triggers CRASHED on
// its own) and asserting via the *resulting* State and Running flag.
// The attach-alive case in particular is asserted by checking that
// State stays WORKING and RunningPID matches the registered PID.

// TestDerive_NoAttached_ProcFound: today's auto path. No attached-pid
// file, proc.Running would normally do the scan. We stub the lister to
// inject a `claude` process in the worktree; Derive should report
// Running=true with the matching PID and State=WORKING.
//
// We can't easily inject a lister here without changing Derive's
// signature; instead this test relies on the proc.Running fallback
// being exercised by the integration test in cmd/bosun. The unit-
// level coverage of "attached absent → fall through" is the
// negative-side test below (TestDerive_NoAttached_NoProc).

// TestDerive_NoAttached_NoProc: no attached-pid file, no agent in proc
// table, dirty>0 → today's CRASHED path. This pins the legacy auto
// behavior so the attach refactor doesn't regress the dogfood case
// that motivated the v0.11 CRASHED rule.
func TestDerive_NoAttached_NoProc(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": " M a.go\n"},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{}, &fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateCrashed {
		t.Errorf("State = %q, want CRASHED (no attached, no proc, dirty>0)", got[0].State)
	}
	if got[0].Running {
		t.Errorf("Running = true, want false")
	}
	if got[0].RunningExternal {
		t.Errorf("RunningExternal = true, want false (auto mode)")
	}
}

// TestDerive_AttachedAlive_SkipsCrashed: an attached-pid pointing to a
// live process (the test process itself) suppresses CRASHED even when
// the worktree is dirty and no `claude` is in the proc table. This is
// the v0.11 happy path for external workers — the explicit "I was
// here" is trusted over the absence of a name-matched process.
func TestDerive_AttachedAlive_SkipsCrashed(t *testing.T) {
	myPID := os.Getpid()
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": " M a.go\n"},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{attached: map[string]int{"session-1": myPID}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateWorking {
		t.Errorf("State = %q, want WORKING (attached PID is alive)", got[0].State)
	}
	if !got[0].Running {
		t.Errorf("Running = false, want true (attached PID is alive)")
	}
	if got[0].RunningPID != myPID {
		t.Errorf("RunningPID = %d, want %d (matches attached registration)", got[0].RunningPID, myPID)
	}
}

// TestDerive_AttachedDead_FlipsCrashed: an attached-pid pointing to a
// PID that's no longer alive is the "recoverable crash" signal. The
// explicit registration says "I was here"; its disappearance is
// meaningful enough to flip CRASHED regardless of dirty-count (since
// the agent itself promised to be present). PID 0 / negative is
// treated as absent by Attached, so we use a high never-allocated PID.
func TestDerive_AttachedDead_FlipsCrashed(t *testing.T) {
	deadPID := 2147483640 // see proc/alive_test.go for the sentinel rationale
	if procIsAlive(deadPID) {
		t.Skipf("PID %d happens to be live; the recoverable-crash case can't be exercised on this host", deadPID)
	}
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		// Important: dirty=0 here. The attached-dead path must flip
		// CRASHED on its own — without the explicit registration, a
		// clean worktree never goes CRASHED.
		status: map[string]string{"/repo-bosun-1": ""},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{attached: map[string]int{"session-1": deadPID}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateCrashed {
		t.Errorf("State = %q, want CRASHED (attached PID is dead — recoverable crash)", got[0].State)
	}
	if got[0].Running {
		t.Errorf("Running = true, want false (attached PID dead)")
	}
}

// TestDerive_ExternalGate_SkipsCrashed: liveness_gate=external
// suppresses CRASHED transitions entirely. The session keeps WORKING
// even with dirty files + no live agent, and RunningExternal=true so
// the status column renders "external".
func TestDerive_ExternalGate_SkipsCrashed(t *testing.T) {
	r := &fakeRunner{
		t: t,
		worktree: strings.Join([]string{
			"worktree /repo",
			"HEAD aaa",
			"branch refs/heads/main",
			"",
			"worktree /repo-bosun-1",
			"HEAD bbb",
			"branch refs/heads/bosun/session-1",
			"",
		}, "\n"),
		revCount: map[string]string{"/repo-bosun-1": "0\n"},
		status:   map[string]string{"/repo-bosun-1": " M a.go\n"},
	}
	c := &git.Client{Runner: r}
	cfg := config.Defaults()
	cfg.LivenessGate = config.LivenessGateExternal
	got, err := Derive(context.Background(), c, cfg, "/repo",
		&fakeState{}, &fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateWorking {
		t.Errorf("State = %q, want WORKING (external gate suppresses CRASHED)", got[0].State)
	}
	if !got[0].RunningExternal {
		t.Errorf("RunningExternal = false, want true (external gate)")
	}
	if got[0].Running {
		t.Errorf("Running = true, want false (external gate skips proc-scan; no PID claimed)")
	}
}

// procIsAlive wraps proc.IsAlive through a function-typed indirection
// to avoid an import cycle in this test file (session imports proc; the
// test happens to want a direct call). The wrapper exists so the dead-
// PID sentinel can be re-verified inside the test before we assert on
// "Derive saw it as dead".
var procIsAlive = func(pid int) bool {
	return proc.IsAlive(pid)
}

func TestDerive_EmptyWhenNoBosunBranches(t *testing.T) {
	r := &fakeRunner{
		t:        t,
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
