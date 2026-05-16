package mcp

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAnnounce_RoundTripPushesToBuffer(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "events.log")
	SetEventsLog(logPath)

	srv, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_announce",
		Arguments: map[string]any{
			"session": "session-2",
			"message": "starting on storage layer",
			"kind":    "progress",
		},
	})
	if err != nil {
		t.Fatalf("call bosun_announce: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_announce IsError: %+v", result)
	}

	var out AnnounceResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &out)
	}
	if !out.Recorded {
		t.Errorf("AnnounceResult.Recorded = false, want true")
	}

	// In-memory buffer.
	recent := Recent(5)
	if len(recent) != 1 {
		t.Fatalf("Recent(5) = %d, want 1", len(recent))
	}
	if recent[0].Session != "session-2" || recent[0].Kind != "progress" || recent[0].Message != "starting on storage layer" {
		t.Errorf("buffered event = %+v", recent[0])
	}

	// Persistence file got one line too.
	tailed, err := TailEvents(logPath, 5)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(tailed) != 1 || tailed[0].Message != "starting on storage layer" {
		t.Errorf("tail = %+v", tailed)
	}

	_ = srv // silence unused
}

func TestAnnounce_DefaultsKindToInfo(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	if _, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_announce",
		Arguments: map[string]any{
			"session": "session-1",
			"message": "no kind supplied",
		},
	}); err != nil {
		t.Fatalf("call: %v", err)
	}

	recent := Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d", len(recent))
	}
	if recent[0].Kind != "info" {
		t.Errorf("default kind = %q, want \"info\"", recent[0].Kind)
	}
}

func TestAnnounce_RejectsEmptyMessage(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_announce",
		Arguments: map[string]any{
			"session": "session-1",
			"message": "   ",
		},
	})
	// The SDK surfaces handler errors as IsError on the result rather than
	// returning err from CallTool — match the SDK's behavior whichever way
	// it lands.
	if err == nil && !result.IsError {
		t.Fatalf("empty message should be rejected, got %+v", result)
	}
	if len(Recent(5)) != 0 {
		t.Errorf("buffer should remain empty when message is blank")
	}
}

func TestAnnounce_RejectsOversizedMessage(t *testing.T) {
	// A runaway agent dumping a diff into the events log would blow past
	// 4KB easily and choke `bosun status`'s recent-events tail. Reject
	// loudly so the agent learns to summarize.
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	huge := make([]byte, maxAnnounceMessageBytes+1)
	for i := range huge {
		huge[i] = 'a'
	}
	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "bosun_announce",
		Arguments: map[string]any{
			"session": "session-1",
			"message": string(huge),
		},
	})
	if err == nil && !result.IsError {
		t.Fatalf("oversized message should be rejected, got %+v", result)
	}
	if len(Recent(5)) != 0 {
		t.Errorf("buffer should remain empty when message is rejected")
	}
}

func TestAnnounce_AdvertisesTool(t *testing.T) {
	ResetEventsForTest()
	t.Cleanup(ResetEventsForTest)

	_, sess, cancel := newPipedSession(t)
	defer cancel()

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := false
	for _, tl := range tools.Tools {
		if tl.Name == "bosun_announce" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("bosun_announce missing from tool list: %+v", tools.Tools)
	}
}

// newPipedSession builds an in-process server+client connected via io.Pipe
// pairs. Mirrors the harness in server_test.go but factored so multiple
// tests can share it.
func newPipedSession(t *testing.T) (*Server, *mcpsdk.ClientSession, func()) {
	t.Helper()
	tmp := t.TempDir()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, nil)

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

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
		cancel()
		t.Fatalf("client connect: %v", err)
	}

	teardown := func() {
		_ = session.Close()
		cancel()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
		}
	}
	return srv, session, teardown
}
