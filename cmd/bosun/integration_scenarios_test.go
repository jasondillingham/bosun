package main

// Cross-cutting integration scenarios.
//
// Each test in this file exercises a multi-feature flow that no single
// per-feature scenario can — e.g. dependency-aware merge + named sessions
// + dry-run in one run, or hooks firing across both init and done in one
// end-to-end pass. Single-feature regressions live in scenarios_test.go;
// this file is the place for "two systems must agree" coverage.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/jasondillingham/bosun/internal/tui/control"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestIntegrationScenario_NamedDepsDryRunRespectsTopoOrder combines three
// round-2 features that only intersect at the merge planner: named
// sessions, `## auth (depends: storage)` syntax, and `merge --dry-run`.
// The dry-run must preview the same plan a real merge would execute —
// storage before auth, with HEAD untouched.
func TestIntegrationScenario_NamedDepsDryRunRespectsTopoOrder(t *testing.T) {
	s := newScenario(t)

	plan := `# Plan

## storage
ground floor

## auth (depends: storage)
upper floor

## http
top floor
`
	s.WriteFile("plan.md", plan)
	s.Bosun("init", "storage", "auth", "http", "--brief", "plan.md")

	for _, label := range []string{"storage", "auth", "http"} {
		wt := s.WorktreePathLabel(label)
		s.WriteFileIn(wt, label+".txt", label+" work\n")
		s.CommitIn(wt, label+" work")
		s.Bosun("done", label)
	}

	headBefore := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))

	out := s.Bosun("merge", "--dry-run")
	s.AssertContainsAll(out,
		"storage: would merge",
		"auth: would merge",
		"http: would merge",
	)

	idxStorage := strings.Index(out, "storage: would merge")
	idxAuth := strings.Index(out, "auth: would merge")
	if idxStorage < 0 || idxAuth < 0 || idxStorage > idxAuth {
		t.Fatalf("dry-run plan must list storage before auth (idxStorage=%d, idxAuth=%d):\n%s",
			idxStorage, idxAuth, out)
	}

	headAfter := strings.TrimSpace(s.GitIn(s.repo, "rev-parse", "HEAD"))
	if headBefore != headAfter {
		t.Fatalf("dry-run shouldn't move HEAD: before=%s after=%s", headBefore, headAfter)
	}
	for _, label := range []string{"storage", "auth", "http"} {
		if _, err := os.Stat(filepath.Join(s.repo, label+".txt")); err == nil {
			t.Fatalf("%s.txt unexpectedly landed on main after dry-run", label)
		}
	}
}

// TestIntegrationScenario_TUIMergeOneClearsClaims drives a merge through
// the bubbletea Model's `m` keybind to confirm the wiring that the TUI
// uses in production hands off to the same merge path the CLI does — and
// that the post-merge claims cleanup runs regardless of which surface
// triggered the merge. Uses the exported test helper ApplyRefresh to
// seed the model so we don't need a real tea.Program loop.
func TestIntegrationScenario_TUIMergeOneClearsClaims(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "1")

	wt := s.WorktreePath(1)
	s.WriteFileIn(wt, "feature.txt", "x\n")
	s.CommitIn(wt, "feature work")
	s.Bosun("claim", "session-1", "feature.txt", "internal/foo.go")
	s.Bosun("done", "session-1")

	pre := s.StatusJSON()
	if sess := pre.SessionByNumber(1); sess == nil || sess.Claimed != 2 {
		t.Fatalf("pre-merge state: want Claimed=2, got %+v", sess)
	}

	var mergeOneCalled bool
	services := control.Services{
		Refresh: func() ([]session.Session, []status.Event, error) {
			return nil, nil, nil
		},
		MergeOne: func(sess session.Session) (string, string, error) {
			mergeOneCalled = true
			out, err := s.bosunRaw(s.repo, "merge", sess.Name)
			if err != nil {
				return "conflict", out, err
			}
			return "merged", out, nil
		},
	}

	m := control.New(services, true)
	seeded := []session.Session{{
		Number: 1,
		Name:   "session-1",
		Label:  "session-1",
		Branch: "bosun/session-1",
		Path:   wt,
		State:  session.StateDone,
		Ahead:  1,
	}}
	m.ApplyRefresh(seeded, nil, nil)

	_, cmd := m.Update(tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{'m'}}))
	if cmd == nil {
		t.Fatalf("pressing 'm' on a DONE session should return an action cmd")
	}
	msg := cmd()
	if msg == nil {
		t.Fatalf("merge cmd should produce an action message")
	}
	m.Update(msg)

	if !mergeOneCalled {
		t.Fatalf("MergeOne callback never fired; statusLine=%q", m.StatusLine())
	}
	if !strings.Contains(m.StatusLine(), "merged") {
		t.Fatalf("status line should mention merged outcome, got %q", m.StatusLine())
	}

	post := s.StatusJSON()
	if sess := post.SessionByNumber(1); sess == nil || sess.Claimed != 0 {
		t.Fatalf("post-merge claims should be cleared, got %+v", sess)
	}
	s.AssertFileOnMain("feature.txt")
}

// TestIntegrationScenario_ServeAnnounceFlowsToSSE wires `bosun serve`
// (SSE consumer) and `bosun mcp` (announce producer) through the shared
// .bosun/events.log file. A real browser dashboard works by exactly this
// path: a separate MCP agent pushes an event, the web process polls the
// log, and the SSE stream delivers it. Catches regressions in any of the
// three (announce → file → poll → SSE frame).
func TestIntegrationScenario_ServeAnnounceFlowsToSSE(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets aren't supported on Windows runners")
	}

	s := newScenario(t)
	s.Bosun("init", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Start the web server.
	port := pickFreePort(t)
	serveCmd := exec.CommandContext(ctx, bosunBin, "serve",
		"--port", itoa(port), "--bind", "127.0.0.1", "--interval", "1")
	serveCmd.Dir = s.repo
	var serveOut subprocessTail
	serveCmd.Stdout = &serveOut
	serveCmd.Stderr = &serveOut
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start bosun serve: %v", err)
	}
	t.Cleanup(func() {
		_ = serveCmd.Process.Signal(os.Interrupt)
		_ = serveCmd.Wait()
	})
	// Wait until the HTTP listener is accepting connections by probing
	// /api/status (cheap and already covered by another scenario).
	statusURL := fmt.Sprintf("http://127.0.0.1:%d/api/status", port)
	_ = waitForHTTP200(t, statusURL, 5*time.Second, &serveOut)

	// Start the MCP server on a unique Unix socket in /tmp.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("bosun-serve-announce-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	mcpCmd := exec.CommandContext(ctx, bosunBin, "mcp", "--socket", socketPath)
	mcpCmd.Dir = s.repo
	var mcpOut subprocessTail
	mcpCmd.Stdout = &mcpOut
	mcpCmd.Stderr = &mcpOut
	if err := mcpCmd.Start(); err != nil {
		t.Fatalf("start bosun mcp: %v", err)
	}
	t.Cleanup(func() {
		_ = mcpCmd.Process.Kill()
		_ = mcpCmd.Wait()
	})
	if err := waitForSocket(socketPath, 3*time.Second); err != nil {
		t.Fatalf("mcp socket never appeared: %v\nsubprocess output:\n%s", err, mcpOut.String())
	}

	// Open SSE before posting so we exercise the live-poll path rather
	// than backfill. Read in a goroutine so the announce can fire while
	// we're blocked on the stream.
	sseURL := fmt.Sprintf("http://127.0.0.1:%d/api/events", port)
	type sseResult struct {
		got string
		err error
	}
	resultCh := make(chan sseResult, 1)
	sseCtx, sseCancel := context.WithTimeout(ctx, 12*time.Second)
	defer sseCancel()
	go func() {
		req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, sseURL, nil)
		req.Header.Set("Accept", "text/event-stream")
		client := &http.Client{Timeout: 0}
		resp, err := client.Do(req)
		if err != nil {
			resultCh <- sseResult{err: fmt.Errorf("GET %s: %w", sseURL, err)}
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if strings.Contains(payload, "cross-cutting heartbeat") {
				resultCh <- sseResult{got: payload}
				return
			}
		}
		resultCh <- sseResult{err: fmt.Errorf("scanner exited without match: %v", sc.Err())}
	}()

	// Give the SSE handler a beat to install its file offset before we
	// append. Without this the announce may race the seek-to-EOF in
	// handleEvents and never reach the live-poll branch.
	time.Sleep(250 * time.Millisecond)

	// Post the announce via MCP.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial mcp socket: %v", err)
	}
	defer conn.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name: "bosun-serve-announce-client", Version: "test",
	}, nil)
	mcpSession, err := client.Connect(ctx, &netConnTransport{conn: conn}, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	defer mcpSession.Close()
	res, err := mcpSession.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_announce",
		Arguments: map[string]any{
			"session": "session-1",
			"message": "cross-cutting heartbeat",
			"kind":    "progress",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_announce: %v", err)
	}
	if res.IsError {
		t.Fatalf("bosun_announce IsError: %+v", res)
	}

	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("SSE read error: %v\nserve output:\n%s", r.err, serveOut.String())
		}
		var ev struct {
			Session, Kind, Message string
		}
		if err := json.Unmarshal([]byte(r.got), &ev); err != nil {
			t.Fatalf("decode SSE payload %q: %v", r.got, err)
		}
		if ev.Session != "session-1" || ev.Kind != "progress" || ev.Message != "cross-cutting heartbeat" {
			t.Fatalf("SSE payload mismatch: %+v", ev)
		}
	case <-sseCtx.Done():
		t.Fatalf("never saw the announcement on SSE within %s\nserve output:\n%s",
			"12s", serveOut.String())
	}
}

// TestIntegrationScenario_CleanupRemovesSquashMergedSessions exercises
// the multi-session squash-equivalence path: merge two sessions, then a
// bare `bosun cleanup` (no --force) must remove both via patch-id
// detection while leaving an unfinished third session intact. Catches
// regressions where cleanup's "is this already on main?" check stops
// working for the second-and-later session in a multi-merge run.
func TestIntegrationScenario_CleanupRemovesSquashMergedSessions(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "3")

	for _, n := range []int{1, 2} {
		wt := s.WorktreePath(n)
		s.WriteFileIn(wt, fmt.Sprintf("merged-%d.txt", n), "content\n")
		s.CommitIn(wt, fmt.Sprintf("session-%d work", n))
		s.Bosun("done", fmt.Sprintf("session-%d", n))
	}
	wt3 := s.WorktreePath(3)
	s.WriteFileIn(wt3, "wip.txt", "still cooking\n")
	s.CommitIn(wt3, "session-3 wip (no done)")

	mergeOut := s.Bosun("merge")
	s.AssertContainsAll(mergeOut, "session-1: merged", "session-2: merged")

	cleanupOut := s.Bosun("cleanup")
	s.AssertContainsAll(cleanupOut,
		"session-1: removed",
		"session-2: removed",
		"squash-merged",
	)
	if strings.Contains(cleanupOut, "session-3: removed") {
		t.Fatalf("session-3 (working, no done) should not be removed by bare cleanup:\n%s", cleanupOut)
	}

	s.AssertWorktreeMissing(1)
	s.AssertBranchMissing("bosun/session-1")
	s.AssertWorktreeMissing(2)
	s.AssertBranchMissing("bosun/session-2")
	s.AssertWorktreeExists(3)
	s.AssertBranchExists("bosun/session-3")
}

// TestIntegrationScenario_ListJSONNamedSessions probes the `list --json`
// + named-session intersection: every label must appear in the JSON
// stream with its real branch (bosun/<label>, not bosun/session-N), so
// dashboards and scripts can address named sessions by their label.
func TestIntegrationScenario_ListJSONNamedSessions(t *testing.T) {
	s := newScenario(t)
	s.Bosun("init", "auth", "http", "storage")

	out := s.Bosun("list", "--json")

	var payload struct {
		Version  string `json:"version"`
		Sessions []struct {
			Name   string `json:"name"`
			Branch string `json:"branch"`
			State  string `json:"state"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("list --json parse: %v\n%s", err, out)
	}
	if payload.Version == "" {
		t.Errorf("list --json missing version key:\n%s", out)
	}
	if len(payload.Sessions) != 3 {
		t.Fatalf("sessions = %d, want 3 (one per named label):\n%s", len(payload.Sessions), out)
	}

	want := map[string]bool{"auth": false, "http": false, "storage": false}
	for _, sess := range payload.Sessions {
		if _, ok := want[sess.Name]; !ok {
			t.Errorf("unexpected session name %q (want one of auth/http/storage)", sess.Name)
			continue
		}
		want[sess.Name] = true
		if sess.Branch != "bosun/"+sess.Name {
			t.Errorf("named session %s: branch = %q, want bosun/%s", sess.Name, sess.Branch, sess.Name)
		}
		if sess.State == "" {
			t.Errorf("named session %s: state is empty", sess.Name)
		}
	}
	for label, seen := range want {
		if !seen {
			t.Errorf("named session %q missing from list --json output:\n%s", label, out)
		}
	}
}

// TestIntegrationScenario_HooksFireInSequence wires all three v0.4 hook
// events (pre-init, post-init, post-done) to distinct shell snippets and
// runs a full init → commit → done flow. The test asserts both ordering
// (pre-init runs before worktrees exist; post-init after; post-done last)
// and the env-var contract each hook should see — operators wire on
// these names, so any drift is a breaking change.
func TestIntegrationScenario_HooksFireInSequence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook commands run via sh -c")
	}

	s := newScenario(t)

	preFile := filepath.Join(s.parent, "pre-init.env")
	postFile := filepath.Join(s.parent, "post-init.env")
	doneFile := filepath.Join(s.parent, "post-done.env")
	worktreeProbe := filepath.Join(s.parent, "pre-init-saw-worktree.txt")

	// Probe whether any bosun worktree exists alongside the repo. Since
	// v0.10's UID-per-worktree naming the on-disk dir carries a timestamp
	// suffix that's not predictable before init runs, so we glob the parent
	// rather than spell the path.
	preCmd := fmt.Sprintf(
		`env | grep -E '^BOSUN_(REPO_ROOT|SESSION_COUNT|BASE_BRANCH)=' | sort > %s; `+
			`if ls -d %s/myproj-bosun-* >/dev/null 2>&1; then echo yes > %s; else echo no > %s; fi`,
		shQuote(preFile), shQuote(s.parent), shQuote(worktreeProbe), shQuote(worktreeProbe),
	)
	postCmd := fmt.Sprintf(
		`env | grep -E '^BOSUN_(REPO_ROOT|SESSION_COUNT|BASE_BRANCH)=' | sort > %s`,
		shQuote(postFile),
	)
	doneCmd := fmt.Sprintf(
		`env | grep -E '^BOSUN_(REPO_ROOT|SESSION_LABEL|DONE_STATUS|AHEAD_COUNT|DONE_MESSAGE)=' | sort > %s`,
		shQuote(doneFile),
	)

	cfgBytes, err := json.MarshalIndent(map[string]any{
		"hooks": []map[string]any{
			{"event": "pre-init", "command": preCmd},
			{"event": "post-init", "command": postCmd},
			{"event": "post-done", "command": doneCmd},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	s.WriteFile(".bosun/config.json", string(cfgBytes))

	s.Bosun("init", "1")

	// pre-init must have fired before any worktree existed.
	probe, err := os.ReadFile(worktreeProbe)
	if err != nil {
		t.Fatalf("pre-init never ran (probe missing): %v", err)
	}
	if strings.TrimSpace(string(probe)) != "no" {
		t.Errorf("pre-init saw the worktree on disk; should have run before init created it: %q", probe)
	}

	preEnv := readFile(t, preFile)
	if !strings.Contains(preEnv, "BOSUN_SESSION_COUNT=1\n") {
		t.Errorf("pre-init env missing BOSUN_SESSION_COUNT=1:\n%s", preEnv)
	}
	if !strings.Contains(preEnv, "BOSUN_BASE_BRANCH=main\n") {
		t.Errorf("pre-init env missing BOSUN_BASE_BRANCH=main:\n%s", preEnv)
	}

	postEnv := readFile(t, postFile)
	if !strings.Contains(postEnv, "BOSUN_SESSION_COUNT=1\n") {
		t.Errorf("post-init env missing BOSUN_SESSION_COUNT=1:\n%s", postEnv)
	}
	wantRoot, _ := filepath.EvalSymlinks(s.repo)
	if !strings.Contains(postEnv, "BOSUN_REPO_ROOT="+wantRoot+"\n") {
		t.Errorf("post-init env missing BOSUN_REPO_ROOT=%s:\n%s", wantRoot, postEnv)
	}

	// Drive the done flow so post-done can fire. Look up the worktree
	// path now (post-init) so we get the actual timestamped dir bosun
	// created rather than the legacy fallback the helper would have
	// returned pre-init.
	wt1 := s.WorktreePath(1)
	s.WriteFileIn(wt1, "thing.txt", "x\n")
	s.CommitIn(wt1, "session-1 work")
	s.Bosun("done", "session-1", "--message", "ready for review")

	doneEnv := readFile(t, doneFile)
	if !strings.Contains(doneEnv, "BOSUN_SESSION_LABEL=session-1\n") {
		t.Errorf("post-done env missing BOSUN_SESSION_LABEL=session-1:\n%s", doneEnv)
	}
	if !strings.Contains(doneEnv, "BOSUN_DONE_STATUS=done\n") {
		t.Errorf("post-done env missing BOSUN_DONE_STATUS=done:\n%s", doneEnv)
	}
	if !strings.Contains(doneEnv, "BOSUN_AHEAD_COUNT=1\n") {
		t.Errorf("post-done env missing BOSUN_AHEAD_COUNT=1:\n%s", doneEnv)
	}
	if !strings.Contains(doneEnv, "BOSUN_DONE_MESSAGE=ready for review\n") {
		t.Errorf("post-done env missing BOSUN_DONE_MESSAGE=ready for review:\n%s", doneEnv)
	}
}
