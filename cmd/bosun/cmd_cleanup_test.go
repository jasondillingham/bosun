package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/session"
)

// stubChecks lets each test rig the (isSquashed, wouldDiscardCommits)
// callbacks planCleanup expects, by branch. branches not in the maps
// behave as "not squashed, not discarding" — the zero value matches the
// most common in-flight WORKING shape.
type stubChecks struct {
	squashed map[string]bool
	discard  map[string]bool
	err      error
}

func (sc stubChecks) isSquashed(branch string) (bool, error) {
	if sc.err != nil {
		return false, sc.err
	}
	return sc.squashed[branch], nil
}

func (sc stubChecks) wouldDiscard(branch string) (bool, error) {
	if sc.err != nil {
		return false, sc.err
	}
	return sc.discard[branch], nil
}

// TestPlanCleanup_PurgeGate is the table-driven coverage of the v0.5
// safety gate: --force is no longer enough when removal would discard
// committed work, the operator must pass --purge to opt in. Each row
// names the input session shape, the flags in play, and what we expect
// the planner to decide.
func TestPlanCleanup_PurgeGate(t *testing.T) {
	type want struct {
		action cleanupAction
		// reasonContains is a substring assertion on the plan's reason
		// string. Lets the test pin the human-readable message without
		// brittling on exact wording.
		reasonContains string
	}
	type tc struct {
		name string
		sess session.Session
		opts cleanupOpts
		stub stubChecks
		want want
	}

	doneUnmerged := session.Session{
		Name: "session-1", Branch: "bosun/session-1",
		State: session.StateDone, Ahead: 1, Dirty: 0,
	}
	workingAhead := session.Session{
		Name: "session-2", Branch: "bosun/session-2",
		State: session.StateWorking, Ahead: 1, Dirty: 0,
	}
	workingDirty := session.Session{
		Name: "session-3", Branch: "bosun/session-3",
		State: session.StateWorking, Ahead: 0, Dirty: 1,
	}
	emptySession := session.Session{
		Name: "session-4", Branch: "bosun/session-4",
		State: session.StateWorking, Ahead: 0, Dirty: 0,
	}
	donePostMerge := session.Session{
		// State cleared by `bosun merge` already, content squashed onto base.
		Name: "session-5", Branch: "bosun/session-5",
		State: session.StateWorking, Ahead: 1, Dirty: 0,
	}

	cases := []tc{
		{
			name: "DONE-but-unmerged refuses default cleanup",
			sess: doneUnmerged,
			opts: cleanupOpts{},
			stub: stubChecks{discard: map[string]bool{"bosun/session-1": true}},
			want: want{action: cleanupSkip, reasonContains: "would discard"},
		},
		{
			name: "DONE-but-unmerged refuses --force",
			sess: doneUnmerged,
			opts: cleanupOpts{force: true},
			stub: stubChecks{discard: map[string]bool{"bosun/session-1": true}},
			want: want{action: cleanupSkip, reasonContains: "--purge"},
		},
		{
			name: "DONE-but-unmerged proceeds under --purge",
			sess: doneUnmerged,
			opts: cleanupOpts{purge: true},
			stub: stubChecks{discard: map[string]bool{"bosun/session-1": true}},
			want: want{action: cleanupRemove, reasonContains: "--purge discards"},
		},
		{
			name: "WORKING + ahead, squashed → removable without flags",
			sess: donePostMerge,
			opts: cleanupOpts{},
			stub: stubChecks{squashed: map[string]bool{"bosun/session-5": true}},
			want: want{action: cleanupRemove, reasonContains: "squash-merged"},
		},
		{
			name: "WORKING + ahead, not squashed, tree-divergent → SKIP default",
			sess: workingAhead,
			opts: cleanupOpts{},
			stub: stubChecks{discard: map[string]bool{"bosun/session-2": true}},
			want: want{action: cleanupSkip, reasonContains: "would discard"},
		},
		{
			name: "WORKING + ahead, not squashed, tree-divergent → SKIP under --force",
			sess: workingAhead,
			opts: cleanupOpts{force: true},
			stub: stubChecks{discard: map[string]bool{"bosun/session-2": true}},
			want: want{action: cleanupSkip, reasonContains: "--purge"},
		},
		{
			name: "WORKING + ahead, not squashed, tree-divergent → REMOVE under --purge",
			sess: workingAhead,
			opts: cleanupOpts{purge: true},
			stub: stubChecks{discard: map[string]bool{"bosun/session-2": true}},
			want: want{action: cleanupRemove, reasonContains: "--purge discards"},
		},
		{
			name: "WORKING + dirty only → SKIP default",
			sess: workingDirty,
			opts: cleanupOpts{},
			stub: stubChecks{},
			want: want{action: cleanupSkip, reasonContains: "uncommitted"},
		},
		{
			name: "WORKING + dirty only → REMOVE under --force (no committed work to lose)",
			sess: workingDirty,
			opts: cleanupOpts{force: true},
			stub: stubChecks{},
			want: want{action: cleanupRemove, reasonContains: "force-remove"},
		},
		{
			name: "empty session → removable without flags",
			sess: emptySession,
			opts: cleanupOpts{},
			stub: stubChecks{},
			want: want{action: cleanupRemove, reasonContains: "empty"},
		},
		{
			name: "WORKING + ahead, not squashed, tree-equal → REMOVE (already on base)",
			sess: workingAhead,
			opts: cleanupOpts{},
			stub: stubChecks{
				// discard=false (tree-equal even though unmerged > 0)
			},
			want: want{action: cleanupRemove, reasonContains: "already on base"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plans, err := planCleanup([]session.Session{c.sess}, c.opts, c.stub.isSquashed, c.stub.wouldDiscard)
			if err != nil {
				t.Fatalf("planCleanup err = %v", err)
			}
			if len(plans) != 1 {
				t.Fatalf("got %d plans, want 1", len(plans))
			}
			p := plans[0]
			if p.action != c.want.action {
				t.Fatalf("action = %v, want %v (reason: %q)", p.action, c.want.action, p.reason)
			}
			if c.want.reasonContains != "" && !strings.Contains(p.reason, c.want.reasonContains) {
				t.Fatalf("reason %q does not contain %q", p.reason, c.want.reasonContains)
			}
		})
	}
}

// TestPlanCleanup_NoAheadSkipsGitChecks asserts the planner doesn't make
// unnecessary git calls for sessions that can't possibly lose work (Ahead
// == 0). The stub fails on any call; if planCleanup invoked it for an
// empty session, we'd surface the error.
func TestPlanCleanup_NoAheadSkipsGitChecks(t *testing.T) {
	boom := errors.New("planner should not have called git for an empty session")
	stub := stubChecks{err: boom}
	sess := session.Session{
		Name: "session-1", Branch: "bosun/session-1",
		State: session.StateWorking, Ahead: 0, Dirty: 0,
	}
	plans, err := planCleanup([]session.Session{sess}, cleanupOpts{}, stub.isSquashed, stub.wouldDiscard)
	if err != nil {
		t.Fatalf("planCleanup err = %v", err)
	}
	if plans[0].action != cleanupRemove {
		t.Fatalf("expected remove for empty session, got %v (reason=%q)", plans[0].action, plans[0].reason)
	}
}

// TestCleanupReason maps each invocation shape to the canonical reason
// string the pre-cleanup hook env will carry. Operators wire on this to
// filter manual sweeps from automated orphan trims, so a typo here would
// silently break their routing.
func TestCleanupReason(t *testing.T) {
	cases := []struct {
		name string
		opts cleanupOpts
		want string
	}{
		{"plain", cleanupOpts{}, "manual"},
		{"force only", cleanupOpts{force: true}, "manual"},
		{"purge only", cleanupOpts{purge: true}, "manual"},
		{"orphans wins over orphan-dirs", cleanupOpts{orphansMode: true, orphanDirs: true}, "orphans-mode"},
		{"orphan-dirs only", cleanupOpts{orphanDirs: true}, "orphan-dirs-mode"},
		{"orphans only", cleanupOpts{orphansMode: true}, "orphans-mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanupReason(c.opts); got != c.want {
				t.Fatalf("cleanupReason(%+v) = %q, want %q", c.opts, got, c.want)
			}
		})
	}
}
