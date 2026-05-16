// Package web exposes the same data `bosun status` shows over HTTP: a
// JSON snapshot at /api/status and a server-sent-events stream at
// /api/events. A small embedded static page at / consumes both so the
// operator can keep a browser tab open instead of re-running the CLI.
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/state"
)

// Config bundles the inputs the web server needs from the caller. Mirrors
// the runCtx fields used by `bosun status` so the handlers can rebuild
// the session list each tick without dragging cmd/bosun into the
// dependency graph of this package.
type Config struct {
	RepoRoot string
	Git      *git.Client
	Cfg      config.Config
	Claims   *claims.Store
	State    *state.Store

	// Bind is the address (host or host:port) the server listens on.
	// Defaults are applied by the caller (cmd_serve.go).
	Bind string
	Port int

	// Interval is how often /api/status recomputes its underlying data
	// when called. The handler caches the last payload for this long so
	// rapid polling doesn't fork a `git status` per request.
	Interval time.Duration
}

// Server wraps an *http.Server with the bosun-specific handler set and
// owns the listener lifecycle. Start blocks until ctx is cancelled.
type Server struct {
	cfg Config
	srv *http.Server

	// addr is captured at listen time so tests that ask for a random
	// port (":0") can discover the assigned one via Addr(). Guarded by
	// mu because Start writes it from a goroutine that Addr's callers
	// (tests, status-line printers) race against.
	mu   sync.Mutex
	addr string
}

// New returns a Server ready to listen. Call Start to bind and serve.
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Addr returns the network address the server is bound to. Only useful
// after Start has begun listening (i.e. in tests that pass port 0).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start binds the configured listener and serves until ctx is cancelled.
// Returns nil on a clean shutdown (context cancellation) so the caller
// can translate SIGINT into a zero exit code.
func (s *Server) Start(ctx context.Context) error {
	bind := s.cfg.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	// Allow the caller to pass either a bare host (and a port) or a full
	// host:port via Bind. The "has colon" check is sufficient because
	// IPv6 literals are expected in [bracketed] form.
	addr := bind
	if _, _, err := net.SplitHostPort(bind); err != nil {
		addr = fmt.Sprintf("%s:%d", bind, s.cfg.Port)
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.mu.Lock()
	s.addr = lis.Addr().String()
	s.mu.Unlock()

	mux := http.NewServeMux()
	s.registerHandlers(mux)
	s.srv = &http.Server{
		Handler: mux,
		// SSE clients hold the connection open; no read/write timeouts
		// here on purpose. The handler exits when the request context
		// is cancelled (client disconnect or server shutdown).
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Run Serve in a goroutine so we can race it against ctx cancellation.
	errCh := make(chan error, 1)
	go func() {
		err := s.srv.Serve(lis)
		// http.ErrServerClosed is the clean-shutdown signal; surface
		// anything else.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		// Give in-flight requests a brief grace period to finish, then
		// force-close. SSE handlers exit cleanly when their request
		// context is cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}
