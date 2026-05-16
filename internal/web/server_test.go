package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
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
