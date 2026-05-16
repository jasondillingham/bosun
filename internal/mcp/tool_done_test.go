package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServer_StuckTool exercises bosun_stuck and asserts the state marker
// lands in .bosun/state/session-1.stuck.
func TestServer_StuckTool(t *testing.T) {
	tmp := t.TempDir()
	session := startInProcServer(t, tmp, nil)

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_stuck",
		Arguments: map[string]any{
			"session": "session-1",
			"message": "blocked on review",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_stuck: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_stuck IsError: %+v", result)
	}

	marker := filepath.Join(tmp, ".bosun", "state", "session-1.stuck")
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected stuck marker at %s: %v", marker, err)
	}
	if !strings.Contains(string(body), "blocked on review") {
		t.Errorf("stuck marker missing message: %q", body)
	}
	// And the done marker must NOT exist (mutually exclusive markers).
	if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-1.done")); err == nil {
		t.Errorf("done marker should not exist after MarkStuck")
	}
}

// TestServer_DoneToolForce verifies that bosun_done with force=true skips
// the dirty/ahead gate entirely and still writes the marker.
func TestServer_DoneToolForce(t *testing.T) {
	tmp := t.TempDir()
	// No git client wired — force=true must not require one because it skips
	// validation. If the handler tries to derive sessions anyway, this test
	// will fail with a nil-pointer dereference and flag the regression.
	session := startInProcServer(t, tmp, nil)

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_done",
		Arguments: map[string]any{
			"session": "1",
			"message": "shipped",
			"force":   true,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_done: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_done IsError: %+v", result)
	}

	var got DoneResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &got)
	}
	if got.State != "DONE" {
		t.Errorf("state = %q, want DONE", got.State)
	}

	marker := filepath.Join(tmp, ".bosun", "state", "session-1.done")
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected done marker at %s: %v", marker, err)
	}
	if !strings.Contains(string(body), "shipped") {
		t.Errorf("done marker missing message: %q", body)
	}
}

// TestServer_DoneToolBadSession surfaces parse errors as IsError instead of
// transport failures. Callers (agents) need a structured error to recover.
func TestServer_DoneToolBadSession(t *testing.T) {
	session := startInProcServer(t, t.TempDir(), nil)

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_done",
		Arguments: map[string]any{
			"session": "not-a-session",
			"force":   true,
		},
	})
	if err != nil {
		t.Fatalf("call bosun_done: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError for malformed session, got: %+v", result)
	}
}

// TestServer_DoneToolDirtyRefuses drives bosun_done (no force) against a
// fake git client that reports the worktree dirty. The MarkDone marker
// must NOT be written.
func TestServer_DoneToolDirtyRefuses(t *testing.T) {
	tmp := t.TempDir()
	// Fake git: one bosun worktree (session-1), dirty=1, ahead=1.
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "1\n",
		status:   " M README.md\n",
	}}
	session := startInProcServer(t, tmp, c)

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_done",
		Arguments: map[string]any{
			"session": "session-1",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_done: %v", err)
	}
	if !result.IsError {
		t.Fatalf("dirty worktree should refuse, got success: %+v", result)
	}
	// The marker must not exist — validation refused before MarkDone ran.
	if _, err := os.Stat(filepath.Join(tmp, ".bosun", "state", "session-1.done")); err == nil {
		t.Errorf("done marker should not exist when validation refused")
	}
}

// TestServer_DoneToolCleanSucceeds drives the happy path: clean worktree,
// commits ahead. bosun_done (no force) must succeed and the marker must
// land in .bosun/state/.
func TestServer_DoneToolCleanSucceeds(t *testing.T) {
	tmp := t.TempDir()
	c := &git.Client{Runner: &fakeDoneRunner{
		worktrees: "worktree " + tmp + "\nHEAD aaa\nbranch refs/heads/main\n\n" +
			"worktree " + tmp + "-bosun-1\nHEAD bbb\nbranch refs/heads/bosun/session-1\n\n",
		revCount: "2\n",
		status:   "",
		log:      "abc1234|1700000000|2 hours ago|wire auth\n",
	}}
	session := startInProcServer(t, tmp, c)

	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_done",
		Arguments: map[string]any{
			"session": "session-1",
			"message": "lgtm",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_done: %v", err)
	}
	if result.IsError {
		t.Fatalf("clean+ahead should succeed, got IsError: %+v", result)
	}
	marker := filepath.Join(tmp, ".bosun", "state", "session-1.done")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected done marker at %s: %v", marker, err)
	}
}

// startInProcServer wires a bosun MCP server to in-memory pipes and returns
// a connected client session ready to call tools against. Cleanup is
// registered on t.
func startInProcServer(t *testing.T, repoRoot string, gitClient *git.Client) *mcpsdk.ClientSession {
	t.Helper()
	cstore := claims.NewStore(repoRoot)
	sstore := state.NewStore(repoRoot)
	srv := NewServer(cstore, sstore, gitClient)

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Run(ctx, &pipeTransport{
			r:      serverReader,
			w:      serverWriter,
			closer: pipeCloser{serverReader, serverWriter},
		})
	}()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "bosun-test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, &pipeTransport{
		r:      clientReader,
		w:      clientWriter,
		closer: pipeCloser{clientReader, clientWriter},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		session.Close()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			// Server didn't exit — test cleanup will let the goroutine leak,
			// but t.Cleanup ran cancel() above so the leak is bounded.
		}
	})
	return session
}

// fakeDoneRunner returns canned `git` output for the calls Derive makes
// against one bosun worktree. It's intentionally narrower than the
// fakeRunner in internal/session/derive_test.go — we only need enough to
// drive ValidateDoneable inside the tool handler.
type fakeDoneRunner struct {
	worktrees string
	revCount  string
	status    string
	log       string
}

func (f *fakeDoneRunner) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	joined := strings.Join(args, " ")
	switch {
	case joined == "worktree list --porcelain":
		return f.worktrees, "", nil
	case strings.HasPrefix(joined, "rev-list --count "):
		return f.revCount, "", nil
	case joined == "status --porcelain":
		return f.status, "", nil
	case joined == "log -1 --format=%h|%ct|%ar|%s":
		return f.log, "", nil
	}
	return "", "", nil
}
