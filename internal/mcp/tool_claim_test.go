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

// TestServer_ClaimAndRelease exercises bosun_claim and bosun_release end
// to end over the in-process pipe transport. The on-disk claims store is
// the source of truth, so each step verifies the file matches what the
// tool reported.
func TestServer_ClaimAndRelease(t *testing.T) {
	tmp := t.TempDir()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, nil)

	session, cancel, serverDone := startTestSession(t, srv)
	defer cancel()
	defer session.Close()

	// Sanity: this round's tools are advertised. Loose contains-check so
	// adding a tool in a sibling file doesn't force this test to update
	// its expected count.
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, want := range []string{"bosun_check", "bosun_claim", "bosun_release"} {
		if !hasTool(tools.Tools, want) {
			t.Fatalf("%s not advertised, got: %v", want, toolNames(tools.Tools))
		}
	}

	ctx := context.Background()

	// --- Case 1: first claim seeds the file with both paths, dedupe trims duplicates. ---
	got := callClaim(t, ctx, session, "session-1", []string{"internal/auth/handler.go", "internal/auth/handler.go", "internal/auth/middleware.go"})
	if got.Claimed != 2 {
		t.Fatalf("first claim Claimed = %d, want 2", got.Claimed)
	}
	assertOnDisk(t, cstore, "session-1", []string{"internal/auth/handler.go", "internal/auth/middleware.go"})

	// --- Case 2: second claim merges, idempotent on an already-claimed path. ---
	got = callClaim(t, ctx, session, "session-1", []string{"internal/auth/handler.go", "internal/storage/db.go"})
	if got.Claimed != 3 {
		t.Fatalf("merged claim Claimed = %d, want 3", got.Claimed)
	}
	assertOnDisk(t, cstore, "session-1", []string{"internal/auth/handler.go", "internal/auth/middleware.go", "internal/storage/db.go"})

	// --- Case 3: release a subset, file is rewritten without those paths. ---
	rel := callRelease(t, ctx, session, "session-1", []string{"internal/auth/handler.go", "not-claimed.go"})
	if rel.Released != 1 {
		t.Fatalf("partial release Released = %d, want 1", rel.Released)
	}
	assertOnDisk(t, cstore, "session-1", []string{"internal/auth/middleware.go", "internal/storage/db.go"})

	// --- Case 4: release everything (empty paths) wipes the claim file. ---
	rel = callRelease(t, ctx, session, "session-1", nil)
	if rel.Released != 2 {
		t.Fatalf("release-all Released = %d, want 2", rel.Released)
	}
	c, err := cstore.Read("session-1")
	if err != nil {
		t.Fatalf("read after release-all: %v", err)
	}
	if c != nil {
		t.Fatalf("claim file should be gone after release-all, got %+v", c)
	}

	// --- Case 5: release on a session with no claim returns 0 cleanly. ---
	rel = callRelease(t, ctx, session, "session-9", nil)
	if rel.Released != 0 {
		t.Fatalf("release on empty session = %d, want 0", rel.Released)
	}

	// --- Case 6: missing session field is rejected as a user error. ---
	_, isErr := callClaimRaw(t, ctx, session, "", []string{"x.go"})
	if !isErr {
		t.Fatalf("bosun_claim with empty session should report IsError")
	}
	_, isErr = callReleaseRaw(t, ctx, session, "", nil)
	if !isErr {
		t.Fatalf("bosun_release with empty session should report IsError")
	}

	// --- Case 7: empty paths array is rejected — Add() would have silently
	// no-op'd, which surfaced no signal that the agent had misformed the
	// call (saw this round-1 with agents calling claim with paths: []). ---
	_, isErr = callClaimRaw(t, ctx, session, "session-1", []string{})
	if !isErr {
		t.Fatalf("bosun_claim with empty paths array should report IsError")
	}
	// All-blank entries collapse to "no paths" too.
	_, isErr = callClaimRaw(t, ctx, session, "session-1", []string{"", "   "})
	if !isErr {
		t.Fatalf("bosun_claim with all-blank paths should report IsError")
	}

	// --- Case 8: oversized paths array is rejected before we touch disk. ---
	huge := make([]string, maxClaimPaths+1)
	for i := range huge {
		huge[i] = "f.go"
	}
	_, isErr = callClaimRaw(t, ctx, session, "session-1", huge)
	if !isErr {
		t.Fatalf("bosun_claim with %d paths should report IsError (cap=%d)", len(huge), maxClaimPaths)
	}

	// Clean shutdown so the server goroutine doesn't leak past the test.
	session.Close()
	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit after cancel")
	}
}

// startTestSession wires the server to a pair of in-process pipes and
// returns a connected client session plus a cancel func that tears
// everything down cleanly. The boilerplate is identical to TestServer_CheckTool;
// pulled out so the claim/release test reads top-down without it.
func startTestSession(t *testing.T, srv *Server) (*mcpsdk.ClientSession, context.CancelFunc, <-chan error) {
	t.Helper()

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

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "bosun-claim-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &pipeTransport{
		r:      clientReader,
		w:      clientWriter,
		closer: pipeCloser{clientReader, clientWriter},
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("client connect: %v", err)
	}
	return session, cancel, serverDone
}

func callClaim(t *testing.T, ctx context.Context, session *mcpsdk.ClientSession, name string, paths []string) ClaimResult {
	t.Helper()
	out, isErr := callClaimRaw(t, ctx, session, name, paths)
	if isErr {
		t.Fatalf("bosun_claim %q %v unexpectedly returned IsError", name, paths)
	}
	return out
}

func callClaimRaw(t *testing.T, ctx context.Context, session *mcpsdk.ClientSession, name string, paths []string) (ClaimResult, bool) {
	t.Helper()
	args := map[string]any{"session": name, "paths": paths}
	if paths == nil {
		args["paths"] = []string{}
	}
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "bosun_claim",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool bosun_claim %q %v: %v", name, paths, err)
	}
	var out ClaimResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &out)
	}
	return out, result.IsError
}

func callRelease(t *testing.T, ctx context.Context, session *mcpsdk.ClientSession, name string, paths []string) ReleaseResult {
	t.Helper()
	out, isErr := callReleaseRaw(t, ctx, session, name, paths)
	if isErr {
		t.Fatalf("bosun_release %q %v unexpectedly returned IsError", name, paths)
	}
	return out
}

func callReleaseRaw(t *testing.T, ctx context.Context, session *mcpsdk.ClientSession, name string, paths []string) (ReleaseResult, bool) {
	t.Helper()
	args := map[string]any{"session": name}
	if paths != nil {
		args["paths"] = paths
	}
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "bosun_release",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool bosun_release %q %v: %v", name, paths, err)
	}
	var out ReleaseResult
	if result.StructuredContent != nil {
		data, _ := json.Marshal(result.StructuredContent)
		_ = json.Unmarshal(data, &out)
	}
	return out, result.IsError
}

func assertOnDisk(t *testing.T, store *claims.Store, session string, want []string) {
	t.Helper()
	c, err := store.Read(session)
	if err != nil {
		t.Fatalf("read %s: %v", session, err)
	}
	if c == nil {
		t.Fatalf("claim file for %s missing, want %v", session, want)
	}
	got := append([]string(nil), c.Paths...)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if !equalStrings(got, sortedWant) {
		t.Fatalf("on-disk claims for %s = %v, want %v", session, got, sortedWant)
	}
}

func toolNames(tools []*mcpsdk.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
