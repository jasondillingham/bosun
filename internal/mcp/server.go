// Package mcp implements a Model Context Protocol server that exposes
// bosun's session-coordination primitives as tool calls. Sessions inside an
// MCP-capable agent (e.g. Claude Code) can call `bosun_claim`, `bosun_done`,
// `bosun_check`, etc. directly instead of shelling out to the bosun CLI.
//
// v0.2.0-alpha (round-0 foundation):
//   - Server skeleton bound to the official `modelcontextprotocol/go-sdk`
//   - One stub tool: `bosun_check` (read-only, queries claims for conflicts)
//   - Custom Unix-socket transport so one server can fan multiple sessions
//     onto a single shared backend (vs. one-server-per-stdio-subprocess)
//   - Filesystem fallback is preserved: bosun status / cleanup / merge still
//     read .bosun/claims/ and .bosun/state/ directly, so sessions without
//     MCP keep working
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is the MCP Implementation.Name advertised to clients.
const ServerName = "bosun"

// ServerVersion is the protocol-level version reported in the MCP handshake.
// Decoupled from bosun's binary version on purpose — the protocol version
// signals tool-surface compatibility, not the bosun release version.
const ServerVersion = "0.2.0-alpha"

// SocketEnv is the environment variable agent sessions read to discover
// where the bosun MCP server is listening. Convention chosen so a session
// inside a worktree doesn't have to guess; bosun init wires it up
// automatically (planned for round 1).
const SocketEnv = "BOSUN_MCP_SOCK"

// Server wraps the MCP server with a Unix-socket listener and the bosun
// stores it operates against. Multiple concurrent client connections share
// the same *mcp.Server and the same backing stores — that's the whole
// point of running as a daemon vs. as a per-session subprocess.
type Server struct {
	mcp       *mcp.Server
	claims    *claims.Store
	state     *state.Store
	gitClient *git.Client
	listener  net.Listener

	mu       sync.Mutex
	connWG   sync.WaitGroup
	stopping bool
}

// toolRegistrations holds every tool registration function the package
// has accumulated via init(). Each tool_*.go file appends its own
// registration so adding a tool means adding a file, not editing
// server.go or tools.go. The point is to remove shared-file overlap
// when multiple parallel sessions add tools concurrently.
var toolRegistrations []func(*Server)

// registerTool is called from each tool file's init(). The package-init
// order is deterministic (alphabetical by file name within a package),
// so the tool order is stable across builds.
func registerTool(f func(*Server)) {
	toolRegistrations = append(toolRegistrations, f)
}

// NewServer builds a bosun MCP server with all tools registered against the
// provided stores. Call Listen() before Serve().
//
// gitClient may be nil for tools that only need claims/state; tools that
// inspect repo state (bosun_done) require it. Tests can pass nil and
// pre-seed Session state manually; production callers should pass git.New().
func NewServer(claimsStore *claims.Store, stateStore *state.Store, gitClient *git.Client) *Server {
	s := &Server{
		claims:    claimsStore,
		state:     stateStore,
		gitClient: gitClient,
	}
	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, nil)
	for _, register := range toolRegistrations {
		register(s)
	}
	return s
}

// Listen binds the server to a Unix socket at socketPath. Any pre-existing
// socket file is removed first — stale sockets from crashed processes are
// the common case and silently overwriting them matches the local-daemon
// convention.
func (s *Server) Listen(socketPath string) error {
	// Best-effort remove of a stale socket; ignore not-exist.
	_ = removeIfSocket(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socketPath, err)
	}
	s.listener = ln
	return nil
}

// ListenWith installs an already-bound listener. Used by tests that wire
// the server to an in-memory pipe or a pre-bound socket.
func (s *Server) ListenWith(ln net.Listener) {
	s.listener = ln
}

// Addr returns the listener's address (useful for tests).
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Serve accepts connections in a loop, handing each one to the MCP server
// in its own goroutine. Returns when the listener is closed via Stop() or
// when ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("mcp: Serve called before Listen")
	}
	// Close the listener when ctx is cancelled so Accept unblocks.
	go func() {
		<-ctx.Done()
		_ = s.Stop()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			stopping := s.stopping
			s.mu.Unlock()
			if stopping {
				s.connWG.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.connWG.Add(1)
		go s.handleConn(ctx, conn)
	}
}

// Stop closes the listener and waits for in-flight connections to finish.
// Safe to call multiple times.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// handleConn wraps a net.Conn as an MCP Transport and runs the server
// loop on it. The same *mcp.Server is shared across all connections —
// the SDK's server is concurrency-safe (the HTTP example does the same).
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer s.connWG.Done()
	defer conn.Close()

	transport := &connTransport{conn: conn}
	// Run blocks until the connection closes or the context is cancelled.
	// Errors from a single connection should not bring down the whole
	// server, so we swallow them here (logged at debug level in v0.2).
	_ = s.mcp.Run(ctx, transport)
}

// Run drives the bosun MCP server on a single supplied transport. Useful
// for tests that wire pipes directly without going through the Unix-socket
// listener (and for any future transport bosun adds — e.g. stdio for
// running as an MCP subprocess instead of a daemon).
func (s *Server) Run(ctx context.Context, transport mcp.Transport) error {
	return s.mcp.Run(ctx, transport)
}

// removeIfSocket deletes path only if it's a Unix socket file. Refusing
// to nuke arbitrary regular files makes the "stale socket" cleanup safe
// when a user accidentally passes a path that isn't actually a socket.
func removeIfSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a Unix socket", path)
	}
	return os.Remove(path)
}
