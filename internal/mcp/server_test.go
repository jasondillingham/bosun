package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServer_CheckTool drives the server end-to-end via in-process pipes:
// server speaks JSON-RPC on one pipe pair, client speaks on the mirror
// pair, both go through the SDK's normal Run / CallTool path. No Unix
// socket needed — that's covered by the e2e test that runs against the
// real `bosun mcp` subcommand.
func TestServer_CheckTool(t *testing.T) {
	tmp := t.TempDir()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)

	// Pre-populate a conflicting claim so bosun_check has something to report.
	if err := cstore.Add("session-2", []string{"internal/auth/handler.go", "internal/storage/"}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	srv := NewServer(cstore, sstore, nil)

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Server side: read from clientWriter (via serverReader), write to clientReader (via serverWriter).
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Run(ctx, &pipeTransport{
			r:      serverReader,
			w:      serverWriter,
			closer: pipeCloser{serverReader, serverWriter},
		})
	}()

	// Client side: mirror pipes.
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "bosun-test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, &pipeTransport{
		r:      clientReader,
		w:      clientWriter,
		closer: pipeCloser{clientReader, clientWriter},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	// Sanity: bosun_check is advertised among the registered tools.
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !hasTool(tools.Tools, "bosun_check") {
		t.Fatalf("bosun_check not in tools list: %+v", tools.Tools)
	}

	// Case 1: querying a path session-2 has claimed → conflict reported.
	got := callCheck(t, ctx, session, []string{"internal/auth/handler.go"})
	if len(got.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %+v", got)
	}
	if got.Conflicts[0].Path != "internal/auth/handler.go" {
		t.Errorf("conflict path = %q", got.Conflicts[0].Path)
	}
	sort.Strings(got.Conflicts[0].Sessions)
	if len(got.Conflicts[0].Sessions) != 1 || got.Conflicts[0].Sessions[0] != "session-2" {
		t.Errorf("conflict sessions = %v", got.Conflicts[0].Sessions)
	}

	// Case 2: directory-containment overlap (claim was "internal/storage/",
	// query is a file inside it).
	got = callCheck(t, ctx, session, []string{"internal/storage/db.go"})
	if len(got.Conflicts) != 1 {
		t.Fatalf("expected 1 dir-containment conflict, got %+v", got)
	}

	// Case 3: a path nobody has claimed → no conflict.
	got = callCheck(t, ctx, session, []string{"internal/http/router.go"})
	if len(got.Conflicts) != 0 {
		t.Errorf("expected no conflicts, got %+v", got)
	}

	// Clean shutdown.
	session.Close()
	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit after cancel")
	}
}

// callCheck is a small wrapper that runs the bosun_check tool and decodes
// the structured response. The SDK serializes the typed return value into
// the CallToolResult.StructuredContent field.
func callCheck(t *testing.T, ctx context.Context, session *mcpsdk.ClientSession, paths []string) CheckResult {
	t.Helper()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "bosun_check",
		Arguments: map[string]any{
			"paths": paths,
		},
	})
	if err != nil {
		t.Fatalf("CallTool bosun_check %v: %v", paths, err)
	}
	if result.IsError {
		t.Fatalf("bosun_check returned IsError: %+v", result)
	}
	var out CheckResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &out)
	}
	return out
}

// hasTool reports whether tools contains one with the given name. Kept
// loose so adding a tool doesn't force every existing test to update its
// expected count.
func hasTool(tools []*mcpsdk.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// pipeTransport adapts an io.Reader/io.Writer pair into an mcp.Transport.
// Tests-only: production code uses connTransport on top of net.Conn.
type pipeTransport struct {
	r      io.Reader
	w      io.Writer
	closer io.Closer
}

func (t *pipeTransport) Connect(_ context.Context) (mcpsdk.Connection, error) {
	return newConnConn(t.r, t.w, t.closer), nil
}

// pipeCloser closes both halves of an io.Pipe pair (Reader and Writer have
// independent Close() methods).
type pipeCloser struct {
	r io.Closer
	w io.Closer
}

func (c pipeCloser) Close() error {
	_ = c.r.Close()
	return c.w.Close()
}
