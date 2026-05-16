package mcp

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServer_ConcurrentClaimAcrossConnections is the race-detector workout
// the brief calls for: two simulated sessions hit bosun_claim against the
// same Server over independent Unix-socket connections, both adding
// distinct paths to the *same* bosun session label. With a correctly
// locked claims.Store, every path lands on disk; without the lock, the
// read-modify-write cycle inside Add loses updates and the assertion
// below trips. Run with `go test -race`.
func TestServer_ConcurrentClaimAcrossConnections(t *testing.T) {
	tmp := t.TempDir()
	srv, sockPath, cleanup := startSocketServer(t, tmp)
	defer cleanup()

	const clients = 2
	const callsPerClient = 25

	var wg sync.WaitGroup
	wg.Add(clients)
	errCh := make(chan error, clients*callsPerClient)

	for c := 0; c < clients; c++ {
		go func(c int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			session := dialSocket(t, ctx, sockPath, fmt.Sprintf("client-%d", c))
			defer session.Close()

			for i := 0; i < callsPerClient; i++ {
				p := fmt.Sprintf("c%d/path%02d.go", c, i)
				_, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
					Name: "bosun_claim",
					Arguments: map[string]any{
						"session": "session-1",
						"paths":   []string{p},
					},
				})
				if err != nil {
					errCh <- fmt.Errorf("c%d call %d: %w", c, i, err)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent claim: %v", err)
	}

	// Read straight off disk — every claim must have landed.
	cstore := claims.NewStore(tmp)
	got, err := cstore.Read("session-1")
	if err != nil {
		t.Fatalf("read claims after stress: %v", err)
	}
	want := clients * callsPerClient
	if got == nil || len(got.Paths) != want {
		n := 0
		if got != nil {
			n = len(got.Paths)
		}
		t.Fatalf("paths after concurrent CallTool = %d, want %d (lost-update across connections)", n, want)
	}

	_ = srv // touched to keep the linter happy if it complains about an unused capture below
}

// startSocketServer brings up a real Unix-socket-bound bosun MCP server
// in tmp/.bosun/mcp.sock. Returns the server, the socket path, and a
// cleanup func that stops the listener and waits for in-flight conns.
func startSocketServer(t *testing.T, tmp string) (*Server, string, func()) {
	t.Helper()
	cstore := claims.NewStore(tmp)
	sstore := state.NewStore(tmp)
	srv := NewServer(cstore, sstore, nil)

	// macOS Unix-domain socket paths cap at 104 bytes, and t.TempDir() can
	// blow past that. Use DefaultSocketPath, which routes long paths to a
	// deterministic /tmp/bosun-<hash>.sock — same trick production uses.
	sockPath := DefaultSocketPath(tmp)
	t.Cleanup(func() { _ = os.Remove(sockPath) })
	if err := srv.Listen(sockPath); err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(serveDone)
	}()

	cleanup := func() {
		cancel()
		_ = srv.Stop()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Log("warning: server did not exit promptly after Stop")
		}
	}
	return srv, sockPath, cleanup
}

// dialSocket connects an MCP client to a Unix-socket-bound bosun server.
// Each call returns a fresh logical session — that's the production model
// (one connection per agent process), so tests that exercise concurrency
// across sessions should call this once per simulated session.
func dialSocket(t *testing.T, ctx context.Context, sockPath, name string) *mcpsdk.ClientSession {
	t.Helper()
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("%s dial socket: %v", name, err)
	}
	transport := &connTransport{conn: conn}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: name, Version: "test"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("%s connect: %v", name, err)
	}
	return session
}
