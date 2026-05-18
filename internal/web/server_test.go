package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/state"
)

// TestServer_RoutesEndToEnd boots a real Server on a random port and
// exercises every advertised route: /, /api/status, /api/events. The
// goal is contract coverage — proving that registerHandlers wires each
// path to the expected response — not a deep test of any single handler.
func TestServer_RoutesEndToEnd(t *testing.T) {
	repo := newTestRepo(t)

	// Seed the events log so the SSE backfill has something to emit.
	// Without this the test would have to wait for the poll interval to
	// observe anything, which is slow and flaky.
	logPath := filepath.Join(repo, bosunmcp.EventLogRelative)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	seedEvents(t, logPath,
		bosunmcp.Event{Session: "session-1", Kind: "progress", Message: "first", At: time.Unix(1700000000, 0).UTC()},
		bosunmcp.Event{Session: "session-1", Kind: "progress", Message: "second", At: time.Unix(1700000001, 0).UTC()},
	)

	srv := startTestServer(t, repo)

	// 1. GET / returns the embedded HTML.
	body := getOK(t, "http://"+srv.Addr()+"/")
	if !strings.Contains(body, "<title>bosun</title>") {
		t.Errorf("index page missing expected title:\n%s", body)
	}

	// 2. GET /api/status returns JSON with a "sessions" array. With an
	// empty repo (no bosun worktrees) sessions will be empty — that's
	// still a valid payload and proves the handler reached RenderJSON.
	statusBody := getOK(t, "http://"+srv.Addr()+"/api/status")
	var payload struct {
		Sessions []any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(statusBody), &payload); err != nil {
		t.Fatalf("decode /api/status: %v\nbody=%s", err, statusBody)
	}
	if payload.Sessions == nil {
		t.Errorf("/api/status response missing 'sessions' field: %s", statusBody)
	}

	// 3. GET /api/events streams SSE. Read enough lines to capture both
	// seeded events from the backfill, then break out so the test
	// doesn't block on the long-running stream.
	gotMessages := readSSEMessages(t, "http://"+srv.Addr()+"/api/events", 2, 5*time.Second)
	if len(gotMessages) < 2 {
		t.Fatalf("expected at least 2 SSE backfill events, got %d: %#v", len(gotMessages), gotMessages)
	}
	if !strings.Contains(gotMessages[0], "first") || !strings.Contains(gotMessages[1], "second") {
		t.Errorf("SSE backfill out of order or content mismatch: %#v", gotMessages)
	}
}

// TestServer_Show_Present boots a real Server against a repo with one
// initialized bosun session, claims a path, writes a BOSUN_BRIEF.md, and
// proves /api/show/<session> returns the expected JSON: identifying
// fields from session.Derive plus the brief body and claimed paths the
// dashboard's preview pane needs.
func TestServer_Show_Present(t *testing.T) {
	repo := newTestRepo(t)
	addBosunSession(t, repo, "session-1")

	// Claim a path so the show payload exercises the claims branch — not
	// just the empty fallback.
	store := claims.NewStore(repo)
	if err := store.Add("session-1", []string{"internal/web/handlers.go"}); err != nil {
		t.Fatalf("claims.Add: %v", err)
	}

	// Write a BOSUN_BRIEF.md into the worktree so the brief field has
	// content to round-trip.
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+cfg.WorktreeSuffixForLabel("session-1", ""))
	briefBody := "# Bosun brief — session-1\n\n## Your assignment\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(wt, "BOSUN_BRIEF.md"), []byte(briefBody), 0o644); err != nil {
		t.Fatalf("write brief: %v", err)
	}

	srv := startTestServer(t, repo)
	body := getOK(t, "http://"+srv.Addr()+"/api/show/session-1")

	var got struct {
		Name         string   `json:"name"`
		Branch       string   `json:"branch"`
		State        string   `json:"state"`
		Claimed      int      `json:"claimed"`
		ClaimedPaths []string `json:"claimed_paths"`
		Brief        string   `json:"brief"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode /api/show: %v\nbody=%s", err, body)
	}
	if got.Name != "session-1" {
		t.Errorf("name = %q, want session-1", got.Name)
	}
	if got.Branch != "bosun/session-1" {
		t.Errorf("branch = %q, want bosun/session-1", got.Branch)
	}
	if got.State != "WORKING" {
		t.Errorf("state = %q, want WORKING (default for a session with no marker)", got.State)
	}
	if got.Claimed != 1 || len(got.ClaimedPaths) != 1 || got.ClaimedPaths[0] != "internal/web/handlers.go" {
		t.Errorf("claimed_paths = %v (count=%d), want [internal/web/handlers.go]", got.ClaimedPaths, got.Claimed)
	}
	if got.Brief != briefBody {
		t.Errorf("brief mismatch:\n got: %q\nwant: %q", got.Brief, briefBody)
	}
}

// Schema-lock for the /api/show/<session> JSON payload. Documented in
// `docs/json-schema.md` (especially F1/F2/F4 — this surface diverges
// from `bosun show --json` on purpose; the lock keeps the divergence
// deliberate). If any key is added, renamed, retyped, or moved between
// omitempty/non-omitempty, this test fails — and the fix is to update
// the doc and the lock lists together.
var apiShowJSON_AllFieldsPopulatedKeys = []string{
	"name", "number", "branch", "path", "state",
	"state_message",
	"ahead", "dirty", "claimed", "running",
	"running_pid",
	"last_sha", "last_subject", "last_relative", "last_unix",
	"claimed_paths", "brief",
}

var apiShowJSON_MinimalKeys = []string{
	"name", "number", "branch", "path", "state",
	"ahead", "dirty", "claimed", "running",
	"claimed_paths", "brief",
}

func TestSchema_ShowAPIJSON_LockedKeys(t *testing.T) {
	repo := newTestRepo(t)
	addBosunSession(t, repo, "session-1")
	store := claims.NewStore(repo)
	if err := store.Add("session-1", []string{"internal/auth.go"}); err != nil {
		t.Fatalf("claims.Add: %v", err)
	}
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+cfg.WorktreeSuffixForLabel("session-1", ""))
	if err := os.WriteFile(filepath.Join(wt, "BOSUN_BRIEF.md"), []byte("# brief\n"), 0o644); err != nil {
		t.Fatalf("write brief: %v", err)
	}

	srv := startTestServer(t, repo)
	body := getOK(t, "http://"+srv.Addr()+"/api/show/session-1")

	var top map[string]any
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	// Minimal-keys fixture: an empty bosun repo session has no commits
	// yet (so all last_* are absent), no agent running (no running_pid),
	// and no .done/.stuck file (no state_message). This locks the
	// omitempty contract.
	assertExactWebKeys(t, "/api/show/<session> (minimal)", top, apiShowJSON_MinimalKeys)

	// Lock the deliberate divergence with `bosun show --json`. If these
	// keys move to match (because the surfaces converged), update F1/F2
	// in docs/json-schema.md and this lock list together.
	if _, ok := top["worktree"]; ok {
		t.Errorf("/api/show emits 'path', not 'worktree' (see docs/json-schema.md F1) — found unexpected 'worktree' key")
	}
	if _, ok := top["state_msg"]; ok {
		t.Errorf("/api/show emits 'state_message', not 'state_msg' (see docs/json-schema.md F2) — found unexpected 'state_msg' key")
	}
	if _, ok := top["recent_commits"]; ok {
		t.Errorf("/api/show does NOT emit recent_commits (see docs/json-schema.md F4) — found unexpected key")
	}
	if _, ok := top["version"]; ok {
		t.Errorf("/api/show does NOT emit a top-level version field today (see docs/json-schema.md F3). Adding it is purely additive — but update the doc + lock list together.")
	}

	// claimed_paths must always be an array (never null).
	if _, ok := top["claimed_paths"].([]any); !ok {
		t.Errorf("claimed_paths: want array, got %T", top["claimed_paths"])
	}
}

// TestSchema_ShowAPIJSON_AllFieldsPopulated exercises the
// "every omitempty field is set" half of the contract by manually
// marshaling the struct with a fully-populated fixture. This is the
// only practical way to assert the full key set without a flaky
// dependency on the agent process running inside a test worktree.
func TestSchema_ShowAPIJSON_AllFieldsPopulated(t *testing.T) {
	row := showJSON{
		Name:         "session-1",
		Number:       1,
		Branch:       "bosun/session-1",
		Path:         "/abs/myproj-bosun-1",
		State:        "WORKING",
		StateMsg:     "blocked",
		Ahead:        2,
		Dirty:        0,
		Claimed:      1,
		Running:      true,
		RunningPID:   12345,
		LastSHA:      "abc1234",
		LastSubject:  "wire up handler",
		LastRel:      "3m ago",
		LastUnix:     1700000000,
		ClaimedPaths: []string{"internal/auth.go"},
		Brief:        "# brief\n",
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	assertExactWebKeys(t, "/api/show/<session> (full)", top, apiShowJSON_AllFieldsPopulatedKeys)
}

// assertExactWebKeys is the same shape comparator used by
// status_json_test.go and cmd_list_test.go, copied here to keep the
// internal/web tests free of cross-package test imports.
func assertExactWebKeys(t *testing.T, label string, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	expected := append([]string(nil), want...)
	sort.Strings(expected)

	missing := webKeyDiff(expected, got)
	extra := webKeyDiff(got, expected)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	t.Errorf("%s key set mismatch — update docs/json-schema.md and this lock list when intentional.\n  want: %v\n  got:  %v\n  missing: %v\n  extra:   %v",
		label, expected, got, missing, extra)
}

func webKeyDiff(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, x := range b {
		set[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := set[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}

// TestServer_Show_Absent proves /api/show/<unknown> returns 404 — the
// dashboard renders an "unknown session" message rather than a stale
// preview when this happens, so the status code matters.
func TestServer_Show_Absent(t *testing.T) {
	repo := newTestRepo(t)
	srv := startTestServer(t, repo)

	resp, err := http.Get("http://" + srv.Addr() + "/api/show/session-9")
	if err != nil {
		t.Fatalf("GET /api/show/session-9: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/show/session-9 = %d, want 404", resp.StatusCode)
	}

	// A malformed label is a client-side error (400), not a 404 — distinct
	// status so a script can tell "your input was wrong" from "not here".
	resp2, err := http.Get("http://" + srv.Addr() + "/api/show/Not-A-Valid-Label")
	if err != nil {
		t.Fatalf("GET /api/show/Not-A-Valid-Label: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /api/show/Not-A-Valid-Label = %d, want 400", resp2.StatusCode)
	}

	// Trailing-segment paths shouldn't silently match the first segment.
	resp3, err := http.Get("http://" + srv.Addr() + "/api/show/session-1/extra")
	if err != nil {
		t.Fatalf("GET /api/show/session-1/extra: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/show/session-1/extra = %d, want 404", resp3.StatusCode)
	}
}

// addBosunSession creates a real bosun-style branch + worktree at
// the canonical `<repo>-bosun-<label>` path so session.Derive surfaces
// it. Mirrors the steps `bosun init` takes minus the brief writing —
// tests that need a brief write it explicitly.
func addBosunSession(t *testing.T, repo, label string) {
	t.Helper()
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	branch := cfg.BranchForLabel(label)
	cmd := exec.Command("git", "branch", branch, "main")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch %s: %v\n%s", branch, err, out)
	}
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+cfg.WorktreeSuffixForLabel(label, ""))
	cmd = exec.Command("git", "worktree", "add", worktreePath, branch)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
}

// TestServer_Status_IncludesSpawnTreeFields proves the /api/status
// payload surfaces Parent / Children / Depth for sessions that have
// been recorded in .bosun/spawn-tree.json. Top-level sessions with no
// children keep the minimal v0.8 shape (the three new keys are
// omitempty), and sub-sessions emit parent + depth. The dashboard
// indents rows based on these fields, so the JSON shape is the
// contract we lock here.
func TestServer_Status_IncludesSpawnTreeFields(t *testing.T) {
	repo := newTestRepo(t)
	// Both worktrees use the un-dotted form so session.Derive's branch
	// regex matches them. The spawn-tree linkage below is what flips
	// session-2 into a child of session-1 — Derive's job is to surface
	// the rows; spawntree.EnrichSessions paints the tree shape on top.
	addBosunSession(t, repo, "session-1")
	addBosunSession(t, repo, "session-2")

	tree := spawntree.NewStore(repo)
	if err := tree.AddTopLevel("session-1"); err != nil {
		t.Fatalf("AddTopLevel: %v", err)
	}
	if err := tree.AddChild("session-1", "session-2"); err != nil {
		t.Fatalf("AddChild: %v", err)
	}

	srv := startTestServer(t, repo)
	body := getOK(t, "http://"+srv.Addr()+"/api/status")

	var payload struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode /api/status: %v\nbody=%s", err, body)
	}

	byName := map[string]map[string]any{}
	for _, s := range payload.Sessions {
		if name, ok := s["name"].(string); ok {
			byName[name] = s
		}
	}

	parent, ok := byName["session-1"]
	if !ok {
		t.Fatalf("session-1 missing from /api/status: %s", body)
	}
	child, ok := byName["session-2"]
	if !ok {
		t.Fatalf("session-2 missing from /api/status: %s", body)
	}

	// Parent row: depth omitempty (=0 → absent), parent omitempty (=""
	// → absent), children populated as the JSON array of child labels.
	if _, hasParent := parent["parent"]; hasParent {
		t.Errorf("session-1 should not emit 'parent' key (top-level): %v", parent)
	}
	if _, hasDepth := parent["depth"]; hasDepth {
		t.Errorf("session-1 should omit 'depth' (=0 omitempty): %v", parent)
	}
	kids, ok := parent["children"].([]any)
	if !ok || len(kids) != 1 || kids[0] != "session-2" {
		t.Errorf("session-1 children = %v, want [session-2]", parent["children"])
	}

	// Child row: parent set, depth=1.
	if got := child["parent"]; got != "session-1" {
		t.Errorf("session-2 parent = %v, want session-1", got)
	}
	if got, ok := child["depth"].(float64); !ok || int(got) != 1 {
		t.Errorf("session-2 depth = %v (%T), want 1", child["depth"], child["depth"])
	}
}

// TestServer_StatusMethodNotAllowed proves the handler rejects non-GET
// methods rather than silently returning the cached body. Keeps the API
// boundary tight if someone later turns this into a public-facing service.
func TestServer_StatusMethodNotAllowed(t *testing.T) {
	repo := newTestRepo(t)
	srv := startTestServer(t, repo)

	req, _ := http.NewRequest(http.MethodPost, "http://"+srv.Addr()+"/api/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/status status = %d, want 405", resp.StatusCode)
	}
}

// startTestServer boots a Server on 127.0.0.1:0 and registers cleanup that
// cancels the context and waits for the goroutine. Returns once Addr()
// reports a real port — that's the contract Start makes via the bind step.
func startTestServer(t *testing.T, repo string) *Server {
	t.Helper()
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := New(Config{
		RepoRoot: repo,
		Git:      git.New(),
		Cfg:      cfg,
		Claims:   claims.NewStore(repo),
		State:    state.NewStore(repo),
		Bind:     "127.0.0.1:0",
		// Interval=0 disables status caching so each test sees fresh data.
		Interval: 0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	// Wait until Addr() is populated (set inside Start after net.Listen).
	deadline := time.Now().Add(3 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		t.Fatalf("server never bound a port within 3s")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("server did not shut down within 3s")
		}
	})
	return srv
}

// newTestRepo initializes a minimal git repo in t.TempDir() with one
// commit so session.Derive can run without erroring. It does NOT create
// any bosun sessions — tests that need them initialize them explicitly.
func newTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-q", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

// getOK GETs url with a short timeout and returns the body on 200, or
// fails the test on any other status.
func getOK(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

// readSSEMessages opens an SSE stream, reads until it has collected `want`
// data: lines or until timeout, and returns the unparsed data payloads.
// SSE framing: each event is `data: <body>\n\n`, optionally preceded by
// other lines (event:, id:, comments starting with `:`). We ignore those
// and pluck only data: lines.
func readSSEMessages(t *testing.T, url string, want int, timeout time.Duration) []string {
	t.Helper()
	client := &http.Client{Timeout: 0} // SSE stays open; rely on context
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if got, want := resp.Header.Get("Content-Type"), "text/event-stream"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && len(out) < want {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

// seedEvents writes a JSONL events log at path containing the given
// records. Mirrors the on-disk format the MCP server writes so the SSE
// backfill replays them as if they came from a real session.
func seedEvents(t *testing.T, path string, events ...bosunmcp.Event) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open events log: %v", err)
	}
	defer f.Close()
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Fatalf("write event: %v", err)
		}
	}
}
