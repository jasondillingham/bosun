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
	"github.com/jasondillingham/bosun/internal/usage"
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
	// agentCommands maps session name → persisted agent command.
	// Tests exercising the Phase 1 per-session override populate this;
	// legacy tests leave it nil and the reader returns ok=false.
	agentCommands map[string]string
	// dockerHosts maps session name → persisted Docker host endpoint
	// (Phase 3 lane 4). Tests covering the remote-docker plumbing
	// populate this; legacy tests leave it nil and the reader returns
	// ok=false (so Session.DockerHost stays "" — the local-docker
	// signal).
	dockerHosts map[string]string
	// usageTotals maps session name → cumulative usage. Phase 4 cost-
	// tracking surface; legacy tests leave nil and Session.Usage stays
	// zeroed (which renders as "—" in the COST column).
	usageTotals map[string]usage.Totals
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

func (f *fakeState) ReadAgentCommand(_, name string) (string, bool, error) {
	if f.agentCommands == nil {
		return "", false, nil
	}
	cmd, ok := f.agentCommands[name]
	return cmd, ok, nil
}

func (f *fakeState) ReadDockerHost(_, name string) (string, bool, error) {
	if f.dockerHosts == nil {
		return "", false, nil
	}
	host, ok := f.dockerHosts[name]
	return host, ok, nil
}

func (f *fakeState) ReadUsageTotals(_, name string) (usage.Totals, error) {
	if f.usageTotals == nil {
		return usage.Totals{}, nil
	}
	return f.usageTotals[name], nil
}

type fakeClaims struct{ counts map[string]int }

func (f *fakeClaims) CountFor(_ string, name string) (int, error) {
	return f.counts[name], nil
}

// TestDerive_SubSessionsWithDots pins the 2026-05 follow-up #99
// fix: a sub-session created by bosun_spawn lands at
// `<parent>.<suffix>` (e.g. `session-1.frontend`). Pre-fix, two
// layers conspired to hide them from Derive:
//   1. The branch regex `[a-z][a-z0-9-]*` rejected the dot.
//   2. The numeric-parse `strconv.Atoi("1.frontend")` errored and
//      the loop hit `continue` — hiding even the labels that did
//      match a looser regex.
// Result was sub-sessions silently absent from bosun status / list
// despite their branches + worktrees existing on disk.
func TestDerive_SubSessionsWithDots(t *testing.T) {
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
			"worktree /repo-bosun-1.frontend",
			"HEAD ccc",
			"branch refs/heads/bosun/session-1.frontend",
			"",
			"worktree /repo-bosun-1.backend",
			"HEAD ddd",
			"branch refs/heads/bosun/session-1.backend",
			"",
		}, "\n"),
		revCount: map[string]string{
			"/repo-bosun-1":          "0\n",
			"/repo-bosun-1.frontend": "0\n",
			"/repo-bosun-1.backend":  "0\n",
		},
		status: map[string]string{
			"/repo-bosun-1":          "",
			"/repo-bosun-1.frontend": "",
			"/repo-bosun-1.backend":  "",
		},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo", &fakeState{}, &fakeClaims{})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d sessions, want 3 (parent + 2 sub-sessions): %+v", len(got), got)
	}
	wantLabels := map[string]bool{
		"session-1":          false,
		"session-1.frontend": false,
		"session-1.backend":  false,
	}
	for _, s := range got {
		if _, ok := wantLabels[s.Label]; !ok {
			t.Errorf("unexpected label %q in derived sessions", s.Label)
		}
		wantLabels[s.Label] = true
	}
	for lbl, seen := range wantLabels {
		if !seen {
			t.Errorf("expected label %q missing from derived sessions", lbl)
		}
	}
	// Parent keeps its Number; sub-sessions land as named (Number==0).
	for _, s := range got {
		switch s.Label {
		case "session-1":
			if s.Number != 1 {
				t.Errorf("parent Number = %d, want 1", s.Number)
			}
		case "session-1.frontend", "session-1.backend":
			if s.Number != 0 {
				t.Errorf("sub-session %q Number = %d, want 0 (named-session convention)", s.Label, s.Number)
			}
		}
	}
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

// TestDerive_HeartbeatFresh_MarksRunning: a session that's only emitting
// `bosun_heartbeat` (no attached PID, no proc-scan match) should still
// register as RUNNING when the heartbeat is fresh. This is the Phase 5
// #63 in-container shim path — agents inside Docker containers have a
// different PID namespace than the host, so neither the attached-PID
// check nor proc.RunningForCommand can see them. The heartbeat is the
// portable liveness signal that crosses the boundary.
func TestDerive_HeartbeatFresh_MarksRunning(t *testing.T) {
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
		// Dirty worktree — without the heartbeat fallback this would
		// flip CRASHED because nothing else proved liveness.
		status: map[string]string{"/repo-bosun-1": " M a.go\n"},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{heartbeats: map[string]time.Time{
			"session-1": time.Now().Add(-30 * time.Second),
		}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateWorking {
		t.Errorf("State = %q, want WORKING (heartbeat is fresh)", got[0].State)
	}
	if !got[0].Running {
		t.Errorf("Running = false, want true (fresh heartbeat implies alive)")
	}
	if !got[0].RunningHeartbeat {
		t.Errorf("RunningHeartbeat = false, want true (liveness came from heartbeat path)")
	}
	if got[0].RunningPID != 0 {
		t.Errorf("RunningPID = %d, want 0 (heartbeat path has no PID)", got[0].RunningPID)
	}
	if got[0].Stale {
		t.Errorf("Stale = true, want false (heartbeat 30s old, threshold 5m)")
	}
}

// TestDerive_HeartbeatStale_DoesNotMarkRunning: an old heartbeat
// (past HeartbeatStaleAfter) is NOT treated as evidence of liveness.
// The session falls through to the normal STALE+CRASHED logic — the
// fallback exists only to close the in-container visibility gap, not
// to mask actually-dead sessions.
func TestDerive_HeartbeatStale_DoesNotMarkRunning(t *testing.T) {
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
	if got[0].Running {
		t.Errorf("Running = true, want false (heartbeat is stale)")
	}
	if got[0].RunningHeartbeat {
		t.Errorf("RunningHeartbeat = true, want false (stale heartbeat doesn't count)")
	}
}

// TestDerive_HeartbeatDoesNotMaskAttachedDead: when an attached-pid
// file exists but the PID is dead, the explicit "I crashed" wins
// over any heartbeat — even a fresh one. The agent registered, then
// disappeared; that's a real crash signal, not a missing-heartbeat one.
func TestDerive_HeartbeatDoesNotMaskAttachedDead(t *testing.T) {
	deadPID := 2147483640
	if procIsAlive(deadPID) {
		t.Skipf("PID %d happens to be live on this host", deadPID)
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
		status:   map[string]string{"/repo-bosun-1": ""},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{
			attached:   map[string]int{"session-1": deadPID},
			heartbeats: map[string]time.Time{"session-1": time.Now()},
		},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != StateCrashed {
		t.Errorf("State = %q, want CRASHED (attached-dead wins over fresh heartbeat)", got[0].State)
	}
	if got[0].RunningHeartbeat {
		t.Errorf("RunningHeartbeat = true, want false (attached-dead suppresses heartbeat liveness)")
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

// TestDerive_PopulatesDockerHost pins Phase 3 lane 4's contract:
// when the StateReader reports a persisted Docker host for a session,
// Derive copies it into Session.DockerHost so cleanup/remove/launch
// can consult it without re-reading the state file. Sessions without
// a persisted host get DockerHost="" — the local-docker signal.
func TestDerive_PopulatesDockerHost(t *testing.T) {
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
			"worktree /repo-bosun-2",
			"HEAD ccc",
			"branch refs/heads/bosun/session-2",
			"",
		}, "\n"),
		revCount: map[string]string{
			"/repo-bosun-1": "0\n",
			"/repo-bosun-2": "0\n",
		},
		status: map[string]string{
			"/repo-bosun-1": "",
			"/repo-bosun-2": "",
		},
	}
	c := &git.Client{Runner: r}
	got, err := Derive(context.Background(), c, config.Defaults(), "/repo",
		&fakeState{dockerHosts: map[string]string{
			"session-1": "ssh://thor",
			// session-2 deliberately absent: stays local.
		}},
		&fakeClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	if got[0].DockerHost != "ssh://thor" {
		t.Errorf("session-1 DockerHost = %q, want %q", got[0].DockerHost, "ssh://thor")
	}
	if got[1].DockerHost != "" {
		t.Errorf("session-2 DockerHost = %q, want \"\" (no persisted host)", got[1].DockerHost)
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
