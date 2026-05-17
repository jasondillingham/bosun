package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHeartbeat_AdvertisedAndWrites covers the happy path: the tool is in
// the advertised list, and one call lands a parseable RFC3339Nano
// timestamp into .bosun/state/<session>.heartbeat. The state store's
// Heartbeat method then reads it back to confirm the file round-trips.
func TestHeartbeat_AdvertisedAndWrites(t *testing.T) {
	tmp := t.TempDir()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, nil)

	session, cancel, _ := startTestSession(t, srv)
	defer cancel()
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !hasTool(tools.Tools, "bosun_heartbeat") {
		t.Fatalf("bosun_heartbeat missing from tool list: %v", toolNames(tools.Tools))
	}

	ctx := context.Background()
	before := time.Now().UTC()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "bosun_heartbeat",
		Arguments: map[string]any{"session": "session-1"},
	})
	if err != nil {
		t.Fatalf("CallTool bosun_heartbeat: %v", err)
	}
	if result.IsError {
		t.Fatalf("bosun_heartbeat returned IsError: %+v", result)
	}

	// File landed on disk at the canonical path.
	hbPath := filepath.Join(tmp, ".bosun", "state", "session-1.heartbeat")
	body, err := os.ReadFile(hbPath)
	if err != nil {
		t.Fatalf("read heartbeat file: %v", err)
	}
	at, _, err := sstore.Heartbeat(tmp, "session-1")
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	// Heartbeat timestamp falls inside the [before, after-call] window.
	if at.Before(before.Add(-time.Second)) || at.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("heartbeat time %v outside expected window [%v, now]", at, before)
	}
	if !strings.Contains(string(body), "T") || !strings.Contains(string(body), "Z") {
		t.Errorf("heartbeat body %q doesn't look like RFC3339", body)
	}

	// Structured result advertises the session and timestamp.
	if result.StructuredContent != nil {
		var out HeartbeatResult
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &out)
		if out.Session != "session-1" {
			t.Errorf("Result Session = %q, want session-1", out.Session)
		}
		if out.At == "" {
			t.Errorf("Result At empty; want a timestamp")
		}
	}
}

// TestHeartbeat_RejectsEmptySession protects against an agent that
// forgets to fill the session field — the call should surface as a tool
// error rather than silently writing to a junk path.
func TestHeartbeat_RejectsEmptySession(t *testing.T) {
	_, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_heartbeat",
		Arguments: map[string]any{"session": ""},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("empty session should report IsError, got %+v", result)
	}
}

// TestHeartbeat_RejectsInvalidLabel mirrors ParseLabel's rejection rules:
// uppercase, leading dashes, etc. shouldn't reach the disk.
func TestHeartbeat_RejectsInvalidLabel(t *testing.T) {
	_, sess, cancel := newPipedSession(t)
	defer cancel()

	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_heartbeat",
		Arguments: map[string]any{"session": "BadLabel"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("invalid label should report IsError, got %+v", result)
	}
}

// TestHeartbeat_ConcurrentWritesDoNotTear stresses the flock around
// .bosun/state/<session>.heartbeat. Two clients hammering the same
// session through independent socket connections must end with exactly
// one parseable file body — no partial writes mixing two RFC3339
// timestamps into one line.
func TestHeartbeat_ConcurrentWritesDoNotTear(t *testing.T) {
	tmp := t.TempDir()
	srv, sockPath, cleanup := startSocketServer(t, tmp)
	defer cleanup()
	_ = srv

	const clients = 2
	const callsPerClient = 30

	var wg sync.WaitGroup
	wg.Add(clients)
	errCh := make(chan error, clients*callsPerClient)

	for c := 0; c < clients; c++ {
		go func(c int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			s := dialSocket(t, ctx, sockPath, fmt.Sprintf("client-%d", c))
			defer s.Close()

			for i := 0; i < callsPerClient; i++ {
				if _, err := s.CallTool(ctx, &mcpsdk.CallToolParams{
					Name:      "bosun_heartbeat",
					Arguments: map[string]any{"session": "session-1"},
				}); err != nil {
					errCh <- fmt.Errorf("c%d call %d: %w", c, i, err)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent heartbeat: %v", err)
	}

	// After the stress run, the heartbeat file must contain exactly one
	// parseable timestamp — no torn writes mashing two RFC3339 strings.
	hbPath := filepath.Join(tmp, ".bosun", "state", "session-1.heartbeat")
	body, err := os.ReadFile(hbPath)
	if err != nil {
		t.Fatalf("read heartbeat after stress: %v", err)
	}
	line := strings.TrimSpace(string(body))
	if strings.Count(line, "T") != 1 {
		t.Fatalf("heartbeat file has %d 'T' chars — likely torn write:\n%q",
			strings.Count(line, "T"), line)
	}
	// Round-trips through time.Parse.
	if _, err := time.Parse(time.RFC3339Nano, line); err != nil {
		if _, err2 := time.Parse(time.RFC3339, line); err2 != nil {
			t.Fatalf("heartbeat body %q not parseable as RFC3339(Nano): %v / %v", line, err, err2)
		}
	}
}
